package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// Streams that are not the artifact they are handed over as, and what has to come of them: nothing
// at the key, and the key still free for the artifact. The fake store commits every request that
// reaches it whole with a matching checksum, whether or not the client stays for the answer. MinIO
// does the same when it answers before the client hangs up (stream_integration_test.go).

var errReadFailed = errors.New("read failed")

// changing is a file that holds the artifact while gitdr's checksum reads it, and later from then
// on: it grew, shrank or broke on disk between the checksum and the upload. The checksum reads it
// through and rewinds it, and that rewind is the change. Once changed, a read at or past fail fails,
// -1 for never, and hold, when set, runs once before the first read past the artifact's bytes, where
// a test can wait on the store.
type changing struct {
	artifact, later []byte
	fail            int64
	hold            func()

	changed bool
	off     int64
	read    int64 // bytes read since the last rewind
}

func (c *changing) holds() []byte {
	if c.changed {
		return c.later
	}
	return c.artifact
}

func (c *changing) Read(p []byte) (int, error) {
	data, fail := c.artifact, int64(-1)
	if c.changed {
		data, fail = c.later, c.fail
		if c.hold != nil && c.off >= int64(len(c.artifact)) {
			hold := c.hold
			c.hold = nil
			hold()
		}
	}
	if fail >= 0 && c.off >= fail {
		return 0, errReadFailed
	}
	if c.off >= int64(len(data)) {
		return 0, io.EOF
	}
	if fail >= 0 && int64(len(p)) > fail-c.off {
		p = p[:fail-c.off]
	}
	n := copy(p, data[c.off:])
	c.off += int64(n)
	c.read += int64(n)
	return n, nil
}

func (c *changing) Seek(offset int64, whence int) (int64, error) {
	pos := offset
	switch whence {
	case io.SeekCurrent:
		pos += c.off
	case io.SeekEnd:
		pos += int64(len(c.holds()))
	}
	if pos < 0 {
		return 0, errors.New("seek before the start")
	}
	if pos == 0 {
		if c.read >= int64(len(c.artifact)) {
			c.changed = true
		}
		c.read = 0
	}
	c.off = pos
	return pos, nil
}

// onDisk is a file as gitdr reads one, at offsets for an upload in parts and from where it stands
// for one PUT, and it can fail or change as a disk can. Once after bytes have been read from it, a
// read that reaches offset bad fails there (-1 for never), and it holds one byte more if it grows.
// Parts are read four at a time, so it counts under a lock.
type onDisk struct {
	data       []byte // the artifact, and the byte it gains if it grows
	after, bad int64
	grows      bool

	mu   sync.Mutex
	read int64
	off  int64 // for Read and Seek
}

func (d *onDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	changed := d.read >= d.after
	d.mu.Unlock()
	size := int64(len(d.data))
	if d.grows && !changed {
		size--
	}
	if off >= size {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), size)
	var err error
	if changed && d.bad >= 0 && off <= d.bad && d.bad < end {
		end, err = d.bad, errReadFailed
	}
	n := copy(p, d.data[off:end])
	d.mu.Lock()
	d.read += int64(n)
	d.mu.Unlock()
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (d *onDisk) Read(p []byte) (int, error) {
	n, err := d.ReadAt(p, d.off)
	d.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (d *onDisk) Seek(offset int64, whence int) (int64, error) {
	pos := offset
	switch whence {
	case io.SeekCurrent:
		pos += d.off
	case io.SeekEnd:
		pos += int64(len(d.data))
	}
	if pos < 0 {
		return 0, errors.New("seek before the start")
	}
	d.off = pos
	return pos, nil
}

// streamCase is a stream handed over for an artifact of size bytes. open makes it from data, which
// is longer than the artifact, and the artifact is data[:size].
type streamCase struct {
	name    string
	size    int
	inParts bool
	open    func(data []byte) io.Reader
}

// One PUT and in parts, the artifact's size for each. One PUT is 64 KiB, so a request's body leaves
// the client's 4 KiB write buffer and reaches the store before net/http looks past it; a smaller one
// never leaves the buffer when the write fails. In parts it is two parts of 5 MiB and one byte, with
// the threshold at 5 MiB.
const (
	onePut  = 64 << 10
	inParts = 2*5*mib + 1
)

func streamCases() []streamCase {
	const n, p = onePut, inParts
	return []streamCase{
		{"one PUT/a byte short", n, false, func(d []byte) io.Reader { return bytes.NewReader(d[:n-1]) }},
		{"one PUT/a byte over", n, false, func(d []byte) io.Reader { return bytes.NewReader(d[:n+1]) }},
		{"one PUT/a read that fails part way", n, false, func(d []byte) io.Reader {
			return &onDisk{data: d[:n], bad: n / 2}
		}},
		{"one PUT/bytes, for an artifact that has none", 0, false, func(d []byte) io.Reader { return bytes.NewReader(d[:10]) }},
		{"one PUT/grows once it is checksummed", n, false, func(d []byte) io.Reader {
			return &changing{artifact: d[:n], later: d[:n+1], fail: -1}
		}},
		{"one PUT/shrinks once it is checksummed", n, false, func(d []byte) io.Reader {
			return &changing{artifact: d[:n], later: d[:n-1], fail: -1}
		}},
		{"one PUT/fails part way once it is checksummed", n, false, func(d []byte) io.Reader {
			return &changing{artifact: d[:n], later: d[:n], fail: n / 2}
		}},
		{"one PUT/fails where it should end, once it is checksummed", n, false, func(d []byte) io.Reader {
			return &changing{artifact: d[:n], later: d[:n], fail: n}
		}},
		{"in parts/a byte short", p, true, func(d []byte) io.Reader { return bytes.NewReader(d[:p-1]) }},
		{"in parts/a byte over", p, true, func(d []byte) io.Reader { return bytes.NewReader(d[:p+1]) }},
		{"in parts/a read that fails part way", p, true, func(d []byte) io.Reader {
			return &onDisk{data: d[:p], bad: 7 * mib}
		}},
		{"in parts/grows while its parts are in flight", p, true, func(d []byte) io.Reader {
			// Grown once the checksum and every part have read it.
			return &onDisk{data: d[:p+1], after: 2 * p, bad: -1, grows: true}
		}},
		{"in parts/fails part way while its parts are in flight", p, true, func(d []byte) io.Reader {
			return &onDisk{data: d[:p], after: p, bad: 7 * mib}
		}},
	}
}

// artifactBytes is random data for c, longer than its artifact.
func artifactBytes(t *testing.T, c streamCase) []byte {
	t.Helper()
	data := make([]byte, c.size+16)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// streamOptions are the thresholds the cases are sized for.
func streamOptions() s3backend.Options {
	return s3backend.Options{MultipartThreshold: 5 * mib, PartSize: 5 * mib}
}

// A stream that ends short, runs long or fails part way is not the artifact that was checksummed,
// and it must not become the object at the key. Up to v0.1.21 a long stream did: one PUT sent its
// first size bytes as a whole request, as it did the bytes of a stream that failed where it should
// have ended, and an upload in parts read only up to size and completed. Each was stored and the
// write reported done, and the key could take no other copy. Now the write fails with nothing at the
// key, nothing completed and nothing aborted, and the artifact written next finds the key free. On
// AWS's path, with If-None-Match, and on a store checked with HeadObject first.
func TestNothingIsCommittedFromAStreamOfAnotherSize(t *testing.T) {
	ret := dest.Retention{Mode: dest.RetentionCompliance, Until: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, m := range modes() {
		for _, c := range streamCases() {
			t.Run(m.name+"/"+c.name, func(t *testing.T) {
				fake, srv := newFakeStore(t)
				fake.conditional = m.conditional
				b := newBackendWith(t, srv, streamOptions())
				s3backend.NoBackoff(b)
				if m.conditional {
					s3backend.ConditionalWrites(b)
				}
				data := artifactBytes(t, c)
				const key = "github.com/octo/hello/2026-10-11/hello.bundle"
				ctx := context.Background()

				if _, err := b.PutImmutable(ctx, key, c.open(data), int64(c.size), ret); err == nil {
					t.Fatalf("the stream was stored as the %d-byte artifact", c.size)
				}
				if o := fake.object(key); o != nil {
					t.Fatalf("after the refusal the store holds %d bytes in %d versions at the key, want nothing", len(o.data), o.versions)
				}
				if c.inParts && fake.count("CompleteMultipartUpload") != 0 {
					t.Error("an upload of a stream that is not the artifact was completed")
				}
				noAbort(t, fake)

				res, err := b.PutImmutable(ctx, key, bytes.NewReader(data[:c.size]), int64(c.size), ret)
				if err != nil {
					t.Fatalf("the artifact itself, written next, was refused: %v", err)
				}
				if res.Size != int64(c.size) || !res.RetainUntil.Equal(ret.Until) {
					t.Errorf("put recorded %+v", res)
				}
				if o := fake.object(key); o == nil || o.versions != 1 || !bytes.Equal(o.data, data[:c.size]) {
					t.Errorf("the store does not hold the artifact once at the key")
				}
			})
		}
	}
}
