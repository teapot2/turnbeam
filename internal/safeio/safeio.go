// Package safeio holds small I/O helpers shared by packages that write
// recovery data and outputs: cancelable reads and durable directory updates.
package safeio

import (
	"context"
	"errors"
	"io"
	"os"
)

// Reader returns r, failing reads once ctx is done so long copies stop promptly.
func Reader(ctx context.Context, r io.Reader) io.Reader { return reader{ctx, r} }

type reader struct {
	ctx context.Context
	r   io.Reader
}

func (r reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// SyncDir makes completed renames and creations inside root durable.
func SyncDir(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
