package meta

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

func TestRetryPolicyAndBudgets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		cause    error
		attempts int
		success  bool
	}{
		{"rate limited", []int{429, 429, 429}, nil, 3, false},
		{"service recovery", []int{502, 503, 200}, nil, 3, true},
		{"500 one retry", []int{500, 500, 200}, nil, 2, false},
		{"mixed budget", []int{500, 429, 500}, nil, 3, false},
		{"400", []int{400}, nil, 1, false}, {"401", []int{401}, nil, 1, false}, {"403", []int{403}, nil, 1, false}, {"413", []int{413}, nil, 1, false}, {"501", []int{501}, nil, 1, false},
		{"connection reset", nil, &Error{Kind: ErrTransport, cause: syscall.ECONNRESET}, 3, false},
		{"per-attempt timeout", nil, &Error{Kind: ErrTransport, cause: context.DeadlineExceeded}, 3, false},
		{"incomplete response", nil, &Error{Kind: ErrTransport, cause: io.ErrUnexpectedEOF}, 3, false},
		{"canceled", nil, &Error{Kind: ErrTransport, cause: context.Canceled}, 1, false},
		{"bad certificate", nil, &Error{Kind: ErrTransport, cause: x509.UnknownAuthorityError{}}, 1, false},
		{"permanent DNS", nil, &Error{Kind: ErrTransport, cause: &net.DNSError{IsNotFound: true}}, 1, false},
		{"invalid response", nil, &Error{Kind: ErrResponse}, 1, false}, {"invalid request", nil, &Error{Kind: ErrRequest}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, recording, plan := poolFixture(t, 1)
			extractor := p.Extractor
			extractions := 0
			p.Extractor = extractFunc(func(ctx context.Context, r *media.Prepared, s, e int64) (*media.Clip, error) {
				extractions++
				return extractor.Extract(ctx, r, s, e)
			})
			calls, waits := 0, 0
			var last error
			p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
				calls++
				last = tc.cause
				if last == nil {
					status := tc.statuses[calls-1]
					if status == 200 {
						return &Response{SessionID: "done"}, nil
					}
					last = &Error{Kind: ErrHTTP, StatusCode: status, RetryAfter: 2 * time.Minute}
				}
				return nil, last
			})
			p.backoff = func(int) time.Duration { return time.Second }
			p.wait = func(_ context.Context, delay time.Duration) error {
				waits++
				want := time.Second
				if tc.cause == nil {
					want = 2 * time.Minute
				}
				if delay != want {
					t.Errorf("Retry-After capped/ignored: %v", delay)
				}
				return nil
			}
			results, err := p.Run(context.Background(), recording, plan, "test-key", Diarization)
			if calls != tc.attempts || waits != calls-1 || extractions != 1 {
				t.Fatalf("wrong attempt/extraction budget: %d/%d/%d", calls, waits, extractions)
			}
			if tc.success {
				if err != nil || len(results) != 1 || results[0].Attempts != calls {
					t.Fatal("retry recovery failed")
				}
			} else if results != nil || !errors.Is(err, last) {
				t.Fatalf("retry exhaustion lost cause: %v", err)
			}
		})
	}
}

func TestBackoffAndCancelableServerWait(t *testing.T) {
	for retry := 0; retry < 50; retry++ {
		for range 20 {
			delay := retryBackoff(retry)
			base := time.Second
			for i := 0; i < retry && base < maxBackoff; i++ {
				base = min(base*2, maxBackoff)
			}
			if delay < base || delay > min(base*2, maxBackoff) {
				t.Fatalf("jitter/backoff out of bounds: %v", delay)
			}
		}
	}
	p, recording, plan := poolFixture(t, 3)
	p.Concurrency = 1
	waiting := make(chan struct{})
	var calls atomic.Int64
	p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
		calls.Add(1)
		return nil, &Error{Kind: ErrHTTP, StatusCode: 429, RetryAfter: MaxServerWait}
	})
	p.wait = func(ctx context.Context, delay time.Duration) error {
		if delay != MaxServerWait {
			t.Error("server wait was shortened")
		}
		close(waiting)
		return waitContext(ctx, delay)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Run(ctx, recording, plan, "test-key", Diarization); done <- err }()
	receive(t, waiting)
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("backoff did not stop: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("cancellation started queued work or retried")
	}
	if err := waitContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestOverlongServerWaitStopsInsteadOfStalling(t *testing.T) {
	p, recording, plan := poolFixture(t, 1)
	var calls atomic.Int64
	last := &Error{Kind: ErrHTTP, StatusCode: 429, RetryAfter: MaxServerWait + time.Second}
	p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
		calls.Add(1)
		return nil, last
	})
	p.wait = func(context.Context, time.Duration) error {
		t.Error("an overlong Retry-After was waited out")
		return nil
	}
	results, err := p.Run(context.Background(), recording, plan, "test-key", Diarization)
	if results != nil || !errors.Is(err, ErrRetryLater) || !errors.Is(err, last) || calls.Load() != 1 {
		t.Fatalf("overlong wait: results=%v err=%v calls=%d", results, err, calls.Load())
	}
}
