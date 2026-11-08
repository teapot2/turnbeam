package meta

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"
)

// MaxAttempts bounds requests per chunk, including the first; checkpoints and
// progress events validate attempt counts against it.
const MaxAttempts = 3
const maxBackoff = 30 * time.Second

// MaxServerWait caps how long a Retry-After response is honored. Longer waits
// stop the run instead of stalling it; completed chunks stay checkpointed.
const MaxServerWait = 5 * time.Minute

// ErrRetryLater reports that the service asked to wait longer than MaxServerWait.
var ErrRetryLater = errors.New("the transcription service asked to wait more than 5 minutes; completed chunks are saved, so rerun later with --resume")

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var remote *Error
	if errors.As(err, &remote) && errors.Is(err, ErrHTTP) {
		switch remote.StatusCode {
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	if !errors.Is(err, ErrTransport) {
		return false
	}
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) ||
		errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func retryBackoff(retry int) time.Duration {
	base := time.Second
	for i := 0; i < retry && base < maxBackoff; i++ {
		base = min(base*2, maxBackoff)
	}
	return base + time.Duration(rand.Int64N(int64(min(base, maxBackoff-base))+1))
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
