package filelock

import (
	"context"
	"errors"
	"os"
	"time"
)

// WaitDirectory retries TryDirectory with backoff until it succeeds, wait
// elapses, or ctx ends. It returns ErrBusy if the lock stays held.
func WaitDirectory(ctx context.Context, root *os.Root, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	delay := 25 * time.Millisecond
	for {
		f, err := TryDirectory(root)
		if !errors.Is(err, ErrBusy) || time.Now().Add(delay).After(deadline) {
			return f, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 500*time.Millisecond)
	}
}
