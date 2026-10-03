package ctxio_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"gitdr.io/gitdr/internal/ctxio"
)

// stopsAfter is a source that stops the copy, the way a SIGTERM does, during its read number at.
type stopsAfter struct {
	r     io.Reader
	at    int
	reads int
	stop  context.CancelFunc
}

func (s *stopsAfter) Read(p []byte) (int, error) {
	s.reads++
	if s.reads == s.at {
		s.stop()
	}
	return s.r.Read(p)
}

// A copy through the reader ends at the first read after the stop, with the context's error, and
// copies everything when nothing stops it.
func TestACopyEndsAtTheFirstReadAfterTheStop(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	src := &stopsAfter{r: bytes.NewReader(make([]byte, 1<<20)), at: 2, stop: stop}
	n, err := io.Copy(io.Discard, ctxio.Reader(ctx, src))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the stopped copy returned %v, want context.Canceled", err)
	}
	if src.reads != 2 {
		t.Errorf("the source was read %d times, want 2: nothing is read after the stop", src.reads)
	}
	if n == 0 || n >= 1<<20 {
		t.Errorf("the stopped copy moved %d bytes, want what the two reads gave", n)
	}

	n, err = io.Copy(io.Discard, ctxio.Reader(t.Context(), bytes.NewReader(make([]byte, 1<<20))))
	if err != nil || n != 1<<20 {
		t.Errorf("a copy nothing stopped moved %d bytes with %v, want all of them", n, err)
	}
}
