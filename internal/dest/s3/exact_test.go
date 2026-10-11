package s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

// sendAsNetHTTP reads body as net/http sends a request body of Content-Length size: up to size
// bytes through an io.LimitReader into the connection, then on into io.Discard to look for a byte
// past them (net/http/transfer.go, lines 372-393, Go 1.26.9). It returns what went into the
// connection, read reads at a time. The request a store receives is whole when that is size bytes,
// and only then can it be committed.
func sendAsNetHTTP(body io.Reader, size int64, read int) ([]byte, error) {
	var conn bytes.Buffer
	_, err := io.CopyBuffer(struct{ io.Writer }{&conn}, io.LimitReader(body, size), make([]byte, read))
	if err == nil {
		_, err = io.Copy(io.Discard, body)
	}
	return conn.Bytes(), err
}

// lastBytesWith hands r's last bytes over together with err, in place of the io.EOF after them.
type lastBytesWith struct {
	r   io.Reader
	err error
}

func (l lastBytesWith) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == io.EOF {
		err = l.err
	}
	return n, err
}

// A stream that is not the artifact must never make a request whole: the reader never passes on
// the artifact's last bytes for it, however the stream hands its bytes over. Read through to its
// end, as the checksum reads it, it never says io.EOF either, and for the artifact it says nothing
// else. Up to v0.1.21 the body was the stream itself, and net/http sent a long stream's first size
// bytes as a whole request before it found the byte after them.
func TestExactReaderNeverCompletesARequestFromAnotherStream(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuv")
	readFailed := errors.New("read failed")
	sources := []struct {
		name string
		wrap func(io.Reader) io.Reader
		ends bool // the stream ends after its bytes, rather than failing
	}{
		{"as it is", func(r io.Reader) io.Reader { return r }, true},
		{"a byte at a time", iotest.OneByteReader, true},
		{"half of each read", iotest.HalfReader, true},
		{"its end with its last bytes", iotest.DataErrReader, true},
		// An end is an end however it is worded, and the reader says it as io.EOF itself, which is
		// what io.Copy and net/http compare with.
		{"an end wrapped in an error", func(r io.Reader) io.Reader {
			return io.MultiReader(r, iotest.ErrReader(fmt.Errorf("the pipe closed: %w", io.EOF)))
		}, true},
		{"a read that fails after its bytes", func(r io.Reader) io.Reader {
			return io.MultiReader(r, iotest.ErrReader(readFailed))
		}, false},
		{"its last bytes with a failure", func(r io.Reader) io.Reader {
			return lastBytesWith{iotest.DataErrReader(r), readFailed}
		}, false},
	}
	// Reads are 4 bytes. An artifact of 19 ends inside one, as nearly every real artifact does, so
	// the last read asks for more than is left of it. A stream of 16 for an artifact of 20 ends on a
	// full read, so its end comes on a read of its own.
	pairs := []struct{ size, stream int }{
		{20, 20}, {19, 19}, {8, 8}, {0, 0},
		{20, 19}, {19, 18}, {8, 7}, {20, 16}, {20, 0},
		{20, 21}, {19, 20}, {8, 9}, {8, 20}, {0, 1},
	}
	for _, src := range sources {
		for _, tc := range pairs {
			what := fmt.Sprintf("%s: a %d-byte stream for a %d-byte artifact", src.name, tc.stream, tc.size)
			artifact := tc.stream == tc.size && src.ends
			open := func() *exactReader {
				return &exactReader{r: src.wrap(bytes.NewReader(data[:tc.stream])), size: int64(tc.size)}
			}

			e := open()
			sent, err := sendAsNetHTTP(e, int64(tc.size), 4)
			if e.n != int64(len(sent)) {
				t.Errorf("%s passed on %d bytes and counted %d", what, len(sent), e.n)
			}
			switch {
			case artifact && (err != nil || !bytes.Equal(sent, data[:tc.size])):
				t.Errorf("%s sent %q, %v; want the artifact, whole", what, sent, err)
			// A request of no bytes is whole before its body is read, which is why the checksum
			// reads the body first; below, that read fails.
			case !artifact && tc.size > 0 && len(sent) == tc.size:
				t.Errorf("%s made a whole request of %q", what, sent)
			case !artifact && err == nil:
				t.Errorf("%s was sent without an error", what)
			}

			e = open()
			var whole bytes.Buffer
			_, err = io.CopyBuffer(struct{ io.Writer }{&whole}, e, make([]byte, 4))
			if artifact && (err != nil || !bytes.Equal(whole.Bytes(), data[:tc.size])) {
				t.Errorf("%s read through as %q, %v; want the artifact and its end", what, whole.Bytes(), err)
			}
			if !artifact && (err == nil || errors.Is(err, io.EOF)) {
				t.Errorf("%s read through to %v, which a checksum would take for the artifact's end", what, err)
			}
			if whole.Len() > tc.size {
				t.Errorf("%s passed on %d bytes, past the artifact's %d", what, whole.Len(), tc.size)
			}
		}
	}

	if err := iotest.TestReader(&exactReader{r: bytes.NewReader(data), size: int64(len(data))}, data); err != nil {
		t.Error(err)
	}
}

// The SDK rewinds the body: over plain HTTP it reads it once for the payload's SHA-256 before it
// sends it, it sends it again on a retry, and it measures it by seeking to its end and back. A
// reader that can seek is wrapped in one that can, from where it stood, and every pass reads the
// artifact again. One that cannot is not offered as one: the SDK would take it at its word.
func TestExactSeekerRewindsAsTheSDKDoes(t *testing.T) {
	r := bytes.NewReader([]byte("skip this|the object"))
	if _, err := r.Seek(int64(len("skip this|")), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body, err := exact(r, int64(len("the object")))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := body.(io.ReadSeeker)
	if !ok {
		t.Fatalf("a reader that can seek was wrapped as %T, which cannot", body)
	}
	start, err := s.Seek(0, io.SeekCurrent)
	if err != nil || start != 10 {
		t.Fatalf("Seek(0, current) = %d, %v; want 10, where the reader stood", start, err)
	}
	if end, err := s.Seek(0, io.SeekEnd); err != nil || end != 20 {
		t.Fatalf("Seek(0, end) = %d, %v; want 20", end, err)
	}
	for pass := range 3 {
		if _, err := s.Seek(start, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(s)
		if err != nil || string(got) != "the object" {
			t.Errorf("pass %d read %q, %v; want the object", pass+1, got, err)
		}
	}

	// A read from anywhere but the artifact's own bytes fails rather than counting from there.
	if _, err := s.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(s); err == nil || len(got) != 0 {
		t.Errorf("a read from before the artifact gave %q, %v; want an error and nothing", got, err)
	}

	// A failed pass does not stay failed once the body is rewound: a retry reads it again.
	long := bytes.NewReader([]byte("the object, and more"))
	body, err = exact(long, int64(len("the object")))
	if err != nil {
		t.Fatal(err)
	}
	s = body.(io.ReadSeeker)
	for pass := range 2 {
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(s); err == nil {
			t.Errorf("pass %d of a long stream read through without an error", pass+1)
		}
	}

	onlyReads := struct{ io.Reader }{bytes.NewReader([]byte("the object"))}
	body, err = exact(onlyReads, int64(len("the object")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body.(io.Seeker); ok {
		t.Errorf("a reader that cannot seek was wrapped as %T, which says it can", body)
	}

	data := []byte("0123456789abcdefghijklmnopqrstuv")
	body, err = exact(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := iotest.TestReader(body, data); err != nil {
		t.Error(err)
	}
}

// readerAtFunc is a ReaderAt made of a function.
type readerAtFunc func(p []byte, off int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

// An upload in parts reads each part at its offset, so it can never see a byte past the artifact.
// endsAt is what reads there, and only nothing at all, an end, is an end.
func TestEndsAtSaysOnlyAnEndIsAnEnd(t *testing.T) {
	data := []byte("head|the artifact|")
	readFailed := errors.New("read failed")
	for _, tc := range []struct {
		name string
		r    io.ReaderAt
		off  int64
		ok   bool
	}{
		{"the end of the stream", bytes.NewReader(data), int64(len(data)), true},
		{"past the end of the stream", bytes.NewReader(data), int64(len(data)) + 7, true},
		{"one byte before its end", bytes.NewReader(data), int64(len(data)) - 1, false},
		{"its last byte with an end", readerAtFunc(func(p []byte, _ int64) (int, error) { p[0] = 'x'; return 1, io.EOF }), 4, false},
		{"a failure", readerAtFunc(func([]byte, int64) (int, error) { return 0, readFailed }), 4, false},
		{"neither a byte nor an end", readerAtFunc(func([]byte, int64) (int, error) { return 0, nil }), 4, false},
		{"an end wrapped in an error", readerAtFunc(func([]byte, int64) (int, error) { return 0, fmt.Errorf("closed: %w", io.EOF) }), 4, true},
	} {
		err := endsAt(tc.r, tc.off, 13)
		if (err == nil) != tc.ok {
			t.Errorf("%s: endsAt = %v, want an end: %v", tc.name, err, tc.ok)
		}
	}
}
