package meta

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/media"
)

type extractFunc func(context.Context, *media.Prepared, int64, int64) (*media.Clip, error)

func (f extractFunc) Extract(ctx context.Context, r *media.Prepared, start, end int64) (*media.Clip, error) {
	return f(ctx, r, start, end)
}

type transcribeFunc func(context.Context, *media.Clip, string, string) (*Response, error)

func (f transcribeFunc) Transcribe(ctx context.Context, c *media.Clip, key, mode string) (*Response, error) {
	return f(ctx, c, key, mode)
}

func poolFixture(t *testing.T, count int) (Pool, *media.Prepared, []chunking.Chunk) {
	t.Helper()
	recording := &media.Prepared{Path: "prepared.wav", Samples: int64(count) * media.SampleRate, Duration: time.Duration(count) * time.Second}
	plan := make([]chunking.Chunk, count)
	for i := range plan {
		plan[i] = chunking.Chunk{ID: fmt.Sprintf("chunk_%04d", i+1), StartSample: int64(i) * media.SampleRate, EndSample: int64(i+1) * media.SampleRate}
	}
	p := Pool{Extractor: extractFunc(func(ctx context.Context, _ *media.Prepared, start, end int64) (*media.Clip, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clip := clipFile(t, end-start)
		clip.StartSample, clip.EndSample = start, end
		return clip, nil
	})}
	return p, recording, plan
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("worker event did not arrive")
		var zero T
		return zero
	}
}

func TestPoolConcurrencyOrderAndOverrides(t *testing.T) {
	for _, concurrency := range []int{0, 1, 2, 5, 100} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			p, recording, plan := poolFixture(t, 7)
			p.Concurrency = concurrency
			n := concurrency
			if n == 0 {
				n = DefaultConcurrency
			}
			n = min(n, len(plan))
			started := make(chan int, len(plan))
			release := make([]chan struct{}, len(plan))
			for i := range release {
				release[i] = make(chan struct{})
			}
			var active, peak atomic.Int64
			p.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, key, mode string) (*Response, error) {
				if key != "test-key" || mode != Diarization {
					t.Error("options not forwarded")
				}
				current := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
				}
				i := int(c.StartSample / media.SampleRate)
				started <- i
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release[i]:
				}
				return &Response{SessionID: fmt.Sprint(i)}, nil
			})
			completed := make(chan int, len(plan))
			p.OnComplete = func(_ context.Context, r Result) error {
				completed <- int(r.Chunk.StartSample / media.SampleRate)
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type answer struct {
				results []Result
				err     error
			}
			done := make(chan answer, 1)
			go func() { r, err := p.Run(ctx, recording, plan, "test-key", ""); done <- answer{r, err} }()
			initial := make([]int, n)
			for i := range initial {
				initial[i] = receive(t, started)
			}
			if active.Load() != int64(n) {
				t.Fatal("default/override did not fill worker slots")
			}
			select {
			case <-started:
				t.Fatal("concurrency bound exceeded")
			default:
			}
			// Finish the initial batch in reverse start order; results must not follow it.
			for i := len(initial) - 1; i >= 0; i-- {
				close(release[initial[i]])
				if receive(t, completed) != initial[i] {
					t.Fatal("completion callback lost association")
				}
			}
			for i := n; i < len(plan); i++ {
				index := receive(t, started)
				close(release[index])
				receive(t, completed)
			}
			got := receive(t, done)
			if got.err != nil || len(got.results) != len(plan) || peak.Load() != int64(n) || active.Load() != 0 {
				t.Fatalf("worker bound/join failed: %v", got.err)
			}
			for i, r := range got.results {
				if r.Chunk != plan[i] || r.Response.SessionID != fmt.Sprint(i) || r.Attempts != 1 {
					t.Fatal("chronological association changed")
				}
			}
		})
	}
}

func TestPoolFailureCancelsAndJoinsOutstandingWork(t *testing.T) {
	p, recording, plan := poolFixture(t, 8)
	started := make(chan int, len(plan))
	fail := make(chan struct{})
	want := &Error{Kind: ErrHTTP, StatusCode: 413}
	var stopped atomic.Int64
	p.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, _, _ string) (*Response, error) {
		i := int(c.StartSample / media.SampleRate)
		started <- i
		if i == 0 {
			<-fail
			return nil, want
		}
		<-ctx.Done()
		stopped.Add(1)
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		results, err := p.Run(context.Background(), recording, plan, "test-key", Diarization)
		if results != nil {
			t.Error("partial run looked complete")
		}
		done <- err
	}()
	for range DefaultConcurrency {
		receive(t, started)
	}
	close(fail)
	if err := receive(t, done); !errors.Is(err, want) || stopped.Load() != DefaultConcurrency-1 {
		t.Fatalf("root error or outstanding cancellation lost: %v", err)
	}
	select {
	case <-started:
		t.Fatal("queued job started after terminal failure")
	default:
	}
}

func TestPoolDrainsValidCompletionsAfterFailure(t *testing.T) {
	p, recording, plan := poolFixture(t, 2)
	p.Concurrency = 2
	ready := make(chan struct{})
	want := errors.New("exhausted")
	p.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, _, _ string) (*Response, error) {
		if c.StartSample == 0 {
			<-ready
			return nil, want
		}
		close(ready)
		<-ctx.Done()
		// A completed response can be awaiting local cleanup when its sibling fails.
		return &Response{SessionID: "completed"}, nil
	})
	completed := 0
	p.OnComplete = func(ctx context.Context, r Result) error {
		if ctx.Err() != nil || r.Chunk != plan[1] || r.Response.SessionID != "completed" {
			t.Fatal("valid completion was lost or changed")
		}
		completed++
		return nil
	}
	if results, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); results != nil || !errors.Is(err, want) || completed != 1 {
		t.Fatalf("valid completion not drained: %v", err)
	}
}

func TestPoolCompletionFailureStopsRun(t *testing.T) {
	p, recording, plan := poolFixture(t, 4)
	p.Concurrency = 1
	p.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, _, _ string) (*Response, error) {
		if c.StartSample > 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &Response{SessionID: "done"}, nil
	})
	want := errors.New("completion save failed")
	p.OnComplete = func(context.Context, Result) error { return want }
	if results, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); results != nil || !errors.Is(err, want) {
		t.Fatalf("completion error ignored: %v", err)
	}
}

func TestPoolValidationAndSubset(t *testing.T) {
	p, recording, plan := poolFixture(t, 3)
	p.Transcriber = transcribeFunc(func(_ context.Context, c *media.Clip, _, _ string) (*Response, error) {
		return &Response{SessionID: fmt.Sprint(c.StartSample)}, nil
	})
	if results, err := p.Run(context.Background(), recording, plan[1:], "test-key", Endpointing); err != nil || len(results) != 2 || results[0].Chunk != plan[1] {
		t.Fatal("resume subset was not retained")
	}
	if results, err := p.Run(context.Background(), recording, nil, "test-key", Diarization); err != nil || len(results) != 0 {
		t.Fatal("empty queue failed")
	}
	for _, bad := range [][]chunking.Chunk{{plan[1], plan[0]}, {plan[0], plan[0]}, {{ID: "outside", StartSample: 0, EndSample: recording.Samples + 1}}, {{ID: "negative", StartSample: -1, EndSample: 1}}, {{ID: "empty", StartSample: 0, EndSample: 0}}} {
		if results, err := p.Run(context.Background(), recording, bad, "test-key", Diarization); results != nil || !errors.Is(err, ErrRequest) {
			t.Fatal("invalid plan accepted")
		}
	}
	p.Concurrency = -1
	if _, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); !errors.Is(err, ErrRequest) {
		t.Fatal("negative concurrency accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Run(ctx, recording, plan, "test-key", Diarization); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancellation lost")
	}
}

func TestPoolRejectsInvalidDependencyResults(t *testing.T) {
	p, recording, plan := poolFixture(t, 1)
	p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) { return nil, nil })
	if _, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); !errors.Is(err, ErrResponse) {
		t.Fatal("nil response accepted")
	}
	p.Extractor = extractFunc(func(context.Context, *media.Prepared, int64, int64) (*media.Clip, error) { return nil, nil })
	if _, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); !errors.Is(err, ErrRequest) {
		t.Fatal("nil clip accepted")
	}
}

func TestPoolCancellationDuringExtraction(t *testing.T) {
	p, recording, plan := poolFixture(t, 4)
	started := make(chan struct{}, DefaultConcurrency)
	var stopped atomic.Int64
	p.Extractor = extractFunc(func(ctx context.Context, _ *media.Prepared, _, _ int64) (*media.Clip, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped.Add(1)
		return nil, ctx.Err()
	})
	p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
		t.Error("canceled extraction triggered upload")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Run(ctx, recording, plan, "test-key", Diarization); done <- err }()
	for range DefaultConcurrency {
		receive(t, started)
	}
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) || stopped.Load() != DefaultConcurrency {
		t.Fatalf("extractors were not canceled/joined: %v", err)
	}
}
