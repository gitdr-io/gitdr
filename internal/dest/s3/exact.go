package s3

import (
	"errors"
	"fmt"
	"io"
)

// A store commits a write once the whole request has reached it, and nothing gitdr does after that
// takes it back. So the request that carries an artifact must never be whole unless the stream it
// was read from is the artifact: exactly size bytes, ending there.
//
// Content-Length does not see to that. net/http sends a body against its Content-Length and looks
// for a byte past it only after the last one has gone (net/http/transfer.go, lines 372-393, Go
// 1.26.9). By then the store holds a whole request, and the CRC32 header gitdr computed over those
// same bytes matches it. A stream that ends sooner does leave the body short of its length, and a
// store refuses such a body (AWS's IncompleteBody), but a short body is all the length catches.

// exactReader passes r on as the artifact of size bytes it is meant to be, and counts what it
// passes. It passes the artifact's last bytes only once it has seen r end there: the read that
// would complete the artifact first reads one byte further, and when r holds another byte, or
// fails, that read returns an error in place of the last bytes. So a stream that runs long, or fails
// where it should end, never makes a request whole, and neither does one that ends short.
//
// It says io.EOF itself where r ends at size, as io.Copy and net/http compare it, when r's end came
// wrapped in another error. Nothing past size is passed on. It is read by one goroutine at a time:
// the checksum's, then an attempt's, whose body the SDK wraps in a reader that takes a lock for every
// read and ends once the attempt is over (smithy-go v1.28.2, transport/http/internal/io/safe.go,
// lines 52-60).
type exactReader struct {
	r     io.Reader
	size  int64
	start int64 // where r stood when it was handed over, for one that can seek
	n     int64 // bytes passed on since start
	err   error // how the stream ended, given again to every read after it until a seek
}

// exactSeeker is an exactReader over a reader that can seek, and it can seek too, so the SDK can
// still rewind the body. Over plain HTTP the SDK reads the body once for the payload's SHA-256
// before sending it, and it sends the body again on a retry. A seek moves r and counts from where r
// lands, and a read anywhere outside the artifact's bytes fails.
type exactSeeker struct {
	*exactReader
	s io.Seeker
}

// exact wraps r as the body of a write of size bytes, as an exactSeeker when r can seek.
func exact(r io.Reader, size int64) (io.Reader, error) {
	e := &exactReader{r: r, size: size}
	s, ok := r.(io.Seeker)
	if !ok {
		return e, nil
	}
	start, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	e.start = start
	return &exactSeeker{exactReader: e, s: s}, nil
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if e.n < 0 || e.n > e.size {
		e.err = fmt.Errorf("the stream was moved to %d bytes from the artifact's start, outside its %d bytes", e.n, e.size)
		return 0, e.err
	}
	if rest := e.size - e.n; int64(len(p)) > rest {
		p = p[:rest]
	}
	var k int
	var err error
	if len(p) > 0 {
		k, err = e.r.Read(p)
	}
	if e.n+int64(k) < e.size {
		e.n += int64(k)
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("the stream ended after %d of the artifact's %d bytes", e.n, e.size)
		}
		e.err = err
		return k, err
	}
	// These k bytes complete the artifact, and they go only where the stream ends.
	if err == nil {
		err = e.past()
	}
	if !errors.Is(err, io.EOF) {
		e.err = err
		return 0, err
	}
	e.n += int64(k)
	e.err = io.EOF
	if k == 0 {
		return 0, io.EOF
	}
	return k, nil
}

// emptyReads bounds the reads of nothing that past takes before it gives up, as bufio does.
const emptyReads = 100

// past reads one byte beyond the artifact. Where the stream ends it returns io.EOF, or an error
// wrapping it; where the stream holds another byte, an error saying so; and any failure as it came.
func (e *exactReader) past() error {
	var b [1]byte
	for range emptyReads {
		k, err := e.r.Read(b[:])
		if k > 0 {
			return fmt.Errorf("the stream holds more than the artifact's %d bytes", e.size)
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
}

func (e *exactSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := e.s.Seek(offset, whence)
	if err != nil {
		return pos, err
	}
	e.n, e.err = pos-e.start, nil
	return pos, nil
}
