package dest

import (
	"errors"
	"io"
)

// ErrResponseTooLarge is what a capped answer returns once it runs past its cap.
var ErrResponseTooLarge = errors.New("the answer is longer than gitdr reads")

// LimitBody caps rc at limit bytes. A read past them returns ErrResponseTooLarge, and so does every
// read after it. Close closes rc.
//
// It takes at most limit+1 bytes from rc, the one extra telling an answer of exactly limit bytes
// from a longer one, so a 64 MiB answer costs what a 1 MiB one does.
func LimitBody(rc io.ReadCloser, limit int64) io.ReadCloser {
	if limit < 0 {
		limit = 0
	}
	return &limitedBody{rc: rc, left: limit}
}

type limitedBody struct {
	rc   io.ReadCloser
	left int64 // bytes still allowed
	err  error // sticky: the cap was passed, or rc failed or ended
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if ask := b.left + 1; ask > 0 && int64(len(p)) > ask {
		p = p[:ask]
	}
	n, err := b.rc.Read(p)
	if int64(n) > b.left {
		n, b.left, b.err = int(b.left), 0, ErrResponseTooLarge
		return n, b.err
	}
	b.left -= int64(n)
	b.err = err
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
