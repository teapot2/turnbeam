package meta

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/media"
)

// DefaultConcurrency bounds both owned temporary clips and in-flight requests.
const DefaultConcurrency = 3

// ClipExtractor supplies owned clips, supporting concurrent calls and cancellation.
type ClipExtractor interface {
	Extract(context.Context, *media.Prepared, int64, int64) (*media.Clip, error)
}

// Transcriber supports concurrent, cancelable attempts; the pool owns retries.
type Transcriber interface {
	Transcribe(context.Context, *media.Clip, string, string) (*Response, error)
}

// Result associates an unchanged request-local response with its original range.
type Result struct {
	Chunk       chunking.Chunk
	Response    *Response
	Attempts    int
	AudioSHA256 string
}

// Pool extracts lazily, retaining at most one clip per worker across retries.
// Zero values use three workers, media.Processor and Client. Configuration and
// input must not be mutated during Run. Callbacks run serially per Run and may
// receive an already-canceled context.
type Pool struct {
	Concurrency int
	Extractor   ClipExtractor
	Transcriber Transcriber
	// OnComplete receives each valid response, including work drained after
	// another chunk fails; it is not a completed-meeting signal.
	OnComplete func(context.Context, Result) error
	// OnProgress carries no upstream text or paths. Its errors cancel work but
	// still let valid completions drain to OnComplete.
	OnProgress func(context.Context, Progress) error
	wait       func(context.Context, time.Duration) error
	backoff    func(int) time.Duration
}

// Run accepts a chronological plan or subset (for explicit resume). The returned
// slice is complete and ordered, or nil on any failure. All workers are joined and
// owned clips closed before return. Successful per-chunk callbacks are independent
// of that all-or-error result, allowing callers to persist valid completed work.
func (p Pool) Run(parent context.Context, recording *media.Prepared, plan []chunking.Chunk, apiKey, mode string) ([]Result, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if mode == "" {
		mode = Diarization
	}
	if p.Concurrency < 0 || !token(apiKey, 8192) || mode != Diarization && mode != Endpointing {
		return nil, &Error{Kind: ErrRequest}
	}
	if err := validateJobs(recording, plan); err != nil {
		return nil, err
	}
	n := p.Concurrency
	if n == 0 {
		n = DefaultConcurrency
	}
	n = min(n, len(plan))
	if p.Extractor == nil {
		p.Extractor = media.Processor{}
	}
	if p.Transcriber == nil {
		p.Transcriber = Client{}
	}
	if p.wait == nil {
		p.wait = waitContext
	}
	if p.backoff == nil {
		p.backoff = retryBackoff
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	progress := poolProgress{state: Progress{Total: len(plan)}, callback: p.OnProgress}
	report := func(kind ProgressKind, active, completed, chunk, attempt, status int, delay time.Duration) error {
		err := progress.send(parent, kind, active, completed, chunk, attempt, status, delay)
		if err != nil {
			cancel(fmt.Errorf("report transcription progress: %w", err))
		}
		return err
	}
	if err := report(ProgressQueued, 0, 0, 0, 0, 0, 0); err != nil {
		return nil, err
	}
	type outcome struct {
		index  int
		result Result
	}
	out := make(chan outcome, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(plan) || ctx.Err() != nil {
					return
				}
				if err := report(ProgressWorkers, 1, 0, i+1, 0, 0, 0); err != nil {
					report(ProgressWorkers, -1, 0, i+1, 0, 0, 0)
					return
				}
				r, err := p.runChunk(ctx, recording, plan[i], apiKey, mode, func(attempt int) error {
					return report(ProgressSending, 0, 0, i+1, attempt, 0, 0)
				}, func(attempt, status int, delay time.Duration) error {
					return report(ProgressRetry, 0, 0, i+1, attempt, status, delay)
				})
				if err != nil {
					cancel(fmt.Errorf("transcribe chunk %d: %w", i+1, err))
				}
				report(ProgressWorkers, -1, 0, i+1, 0, 0, 0)
				out <- outcome{i, r}
				if err != nil {
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(out) }()
	results := make([]Result, len(plan))
	for o := range out {
		if o.result.Response == nil {
			continue
		}
		results[o.index] = o.result
		if p.OnComplete != nil {
			if err := p.OnComplete(parent, o.result); err != nil {
				cancel(fmt.Errorf("record completed chunk %d: %w", o.index+1, err))
				continue
			}
		}
		report(ProgressCompleted, 0, 1, o.index+1, o.result.Attempts, 0, 0)
	}
	kind := ProgressFinished
	if context.Cause(ctx) != nil {
		kind = ProgressStopped
	}
	report(kind, 0, 0, 0, 0, 0, 0)
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func validateJobs(recording *media.Prepared, plan []chunking.Chunk) error {
	if !recording.ValidTimeline() || recording.Path == "" {
		return &Error{Kind: ErrRequest}
	}
	ids := make(map[string]bool, len(plan))
	for i, c := range plan {
		if strings.TrimSpace(c.ID) == "" || ids[c.ID] || c.StartSample < 0 || c.StartSample >= c.EndSample || c.EndSample > recording.Samples || c.EndSample-c.StartSample > media.MaxChunkSamples || c.OverlapSamples < 0 || c.OverlapSamples > c.EndSample-c.StartSample || i > 0 && (c.StartSample <= plan[i-1].StartSample || c.EndSample <= plan[i-1].EndSample) {
			return &Error{Kind: ErrRequest}
		}
		ids[c.ID] = true
	}
	return nil
}

func (p Pool) runChunk(ctx context.Context, recording *media.Prepared, chunk chunking.Chunk, apiKey, mode string, sending func(int) error, retry func(int, int, time.Duration) error) (result Result, err error) {
	result.Chunk = chunk
	clip, err := p.Extractor.Extract(ctx, recording, chunk.StartSample, chunk.EndSample)
	if clip != nil && clip.Prepared != nil {
		defer func() { err = errors.Join(err, clip.Close()) }()
	}
	if err != nil {
		return result, err
	}
	if clip == nil || clip.Prepared == nil {
		return result, &Error{Kind: ErrRequest}
	}
	if clip.StartSample != chunk.StartSample || clip.EndSample != chunk.EndSample {
		return result, &Error{Kind: ErrRequest}
	}
	result.AudioSHA256, err = media.FileSHA256(ctx, clip.Path)
	if err != nil {
		return result, err
	}
	serverFailures := 0
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Attempts++
		if err := sending(result.Attempts); err != nil {
			return result, err
		}
		result.Response, err = p.Transcriber.Transcribe(ctx, clip, apiKey, mode)
		if err == nil {
			if result.Response == nil {
				return result, &Error{Kind: ErrResponse}
			}
			return result, nil
		}
		result.Response = nil
		var remote *Error
		if errors.As(err, &remote) && remote.StatusCode == 500 {
			serverFailures++
		}
		if attempt == MaxAttempts-1 || serverFailures > 1 || !retryable(err) || ctx.Err() != nil {
			return result, err
		}
		delay := p.backoff(attempt)
		if remote != nil {
			delay = max(delay, remote.RetryAfter)
		}
		status := 0
		if remote != nil {
			status = remote.StatusCode
		}
		if err := retry(result.Attempts+1, status, delay); err != nil {
			return result, err
		}
		if waitErr := p.wait(ctx, delay); waitErr != nil {
			return result, waitErr
		}
	}
	return result, err
}
