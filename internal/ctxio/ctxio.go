// Package ctxio makes a long copy stop with its context.
//
// io.Copy runs to the end of its source whatever happens around it, and a source of many GiB on a
// slow disk takes minutes. Read through Reader, the copy ends at the next read after a stop.
package ctxio

import (
	"context"
	"io"
)

// Reader reads from r for as long as ctx lasts. Once ctx is done, Read returns ctx.Err() and reads
// nothing, so a copy from it fails with that error at its next read.
//
// The context is asked before every read. A copy reads 32 KiB or more at a time, so the check costs
// nothing a reader of files would notice.
func Reader(ctx context.Context, r io.Reader) io.Reader {
	return &reader{ctx: ctx, r: r}
}

type reader struct {
	ctx context.Context
	r   io.Reader
}

func (c *reader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
