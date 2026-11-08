package meta

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

func TestProgressCountsRetriesAndCommittedCompletions(t *testing.T) {
	p, recording, plan := poolFixture(t, 3)
	p.Concurrency = 3
	started, release := make(chan struct{}, 3), make(chan struct{})
	var calls [3]atomic.Int64
	p.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, _, _ string) (*Response, error) {
		i := c.StartSample / media.SampleRate
		if calls[i].Add(1) == 1 {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if i == 0 {
				return nil, &Error{Kind: ErrHTTP, StatusCode: 429, RetryAfter: 2 * time.Second}
			}
		}
		return &Response{SessionID: "valid"}, nil
	})
	p.wait = func(context.Context, time.Duration) error { return nil }
	p.backoff = func(int) time.Duration { return time.Second }
	var events []Progress
	var committed atomic.Int64
	p.OnComplete = func(context.Context, Result) error { committed.Add(1); return nil }
	p.OnProgress = func(_ context.Context, e Progress) error {
		if e.Total != 3 || e.Active < 0 || e.Active > 3 || e.Completed < 0 || e.Completed > 3 || e.Active+e.Completed > e.Total {
			t.Error("invalid progress counts")
		}
		if e.Kind == ProgressCompleted && int64(e.Completed) != committed.Load() {
			t.Error("completion was reported before successful save")
		}
		events = append(events, e)
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); done <- err }()
	for range 3 {
		receive(t, started)
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	first, last := events[0], events[len(events)-1]
	if first.Kind != ProgressQueued || first.Active != 0 || last.Kind != ProgressFinished || last.Active != 0 || last.Completed != 3 {
		t.Fatal("queue or joined final progress missing")
	}
	retries, completed, peak := 0, 0, 0
	for _, e := range events {
		peak = max(peak, e.Active)
		if e.Kind == ProgressRetry {
			retries++
			if e.Chunk != 1 || e.Attempt != 2 || e.HTTPStatus != 429 || e.RetryDelay != 2*time.Second {
				t.Fatal("retry details changed")
			}
		}
		if e.Kind == ProgressCompleted {
			completed++
		}
	}
	if retries != 1 || completed != 3 || peak != 3 || calls[0].Load() != 2 {
		t.Fatal("retries inflated completion or workers were not tracked")
	}
}

func TestProgressErrorsStillDrainValidCompletion(t *testing.T) {
	p, recording, plan := poolFixture(t, 1)
	p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
		return &Response{SessionID: "valid"}, nil
	})
	want := errors.New("progress writer failed")
	committed, final := 0, Progress{}
	p.OnComplete = func(context.Context, Result) error { committed++; return nil }
	p.OnProgress = func(_ context.Context, e Progress) error {
		final = e
		// Failure happens after extraction/HTTP finish, before outcome consumption.
		if e.Kind == ProgressWorkers && e.Active == 0 {
			return want
		}
		return nil
	}
	if result, err := p.Run(context.Background(), recording, plan, "test-key", Diarization); result != nil || !errors.Is(err, want) {
		t.Fatal("observer failure was ignored", err)
	}
	if committed != 1 || final.Kind != ProgressStopped || final.Active != 0 || final.Completed != 1 {
		t.Fatal("observer failure lost valid checkpoint or looked successful")
	}
}

func TestProgressFailureAndCancellationFinishAtZero(t *testing.T) {
	for _, mode := range []string{"extract", "transcribe", "save", "observer-start", "observer-retry", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			p, recording, plan := poolFixture(t, 1)
			want := errors.New("failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.Transcriber = transcribeFunc(func(context.Context, *media.Clip, string, string) (*Response, error) {
				switch mode {
				case "transcribe":
					return nil, want
				case "observer-retry":
					return nil, &Error{Kind: ErrHTTP, StatusCode: 503}
				case "cancel":
					cancel()
					return nil, ctx.Err()
				}
				return &Response{SessionID: "valid"}, nil
			})
			if mode == "extract" {
				p.Extractor = extractFunc(func(context.Context, *media.Prepared, int64, int64) (*media.Clip, error) { return nil, want })
			}
			p.OnComplete = func(context.Context, Result) error {
				if mode == "save" {
					return want
				}
				return nil
			}
			var final Progress
			p.OnProgress = func(_ context.Context, e Progress) error {
				final = e
				if mode == "observer-start" && e.Kind == ProgressWorkers && e.Active > 0 || mode == "observer-retry" && e.Kind == ProgressRetry {
					return want
				}
				return nil
			}
			rows, err := p.Run(ctx, recording, plan, "test-key", Diarization)
			if mode == "cancel" {
				want = context.Canceled
			}
			if rows != nil || !errors.Is(err, want) || final.Kind != ProgressStopped || final.Active != 0 || final.Completed != 0 {
				t.Fatalf("failure looked successful: %v %+v", err, final)
			}
		})
	}
}

func TestEmptyProgressAndInitialObserverFailure(t *testing.T) {
	p, recording, _ := poolFixture(t, 1)
	var events []Progress
	p.OnProgress = func(_ context.Context, e Progress) error { events = append(events, e); return nil }
	if _, err := p.Run(context.Background(), recording, nil, "test-key", Diarization); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []Progress{{Kind: ProgressQueued}, {Kind: ProgressFinished}}) {
		t.Fatal("empty queue pretended to start workers")
	}
	want := errors.New("unwritable stderr")
	p.OnProgress = func(context.Context, Progress) error { return want }
	if rows, err := p.Run(context.Background(), recording, nil, "test-key", Diarization); rows != nil || !errors.Is(err, want) {
		t.Fatal("initial observer error was ignored")
	}
}
