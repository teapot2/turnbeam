package jobs

import (
	"context"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
)

// Transcribe queues only uncommitted chunks. Cached completion needs neither
// temporary audio nor credentials; pending work must reproduce normalized bytes.
// Completion commits briefly detach cancellation so already-valid work survives
// interruption. Inference, extraction and backoff remain fully cancelable.
func (j *Job) Transcribe(ctx context.Context, pool meta.Pool, recording *media.Prepared, apiKey string) ([]meta.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	j.mu.Lock()
	if err := j.usable(); err != nil {
		j.mu.Unlock()
		return nil, err
	}
	if j.m.Plan == nil || j.running {
		j.mu.Unlock()
		return nil, ErrState
	}
	pending := []chunking.Chunk{}
	for _, c := range *j.m.Plan {
		if _, exists := j.m.Completed[c.ID]; !exists {
			pending = append(pending, c)
		}
	}
	settings := j.m.Settings
	j.running = true
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
	}()
	if len(pending) > 0 {
		if err := j.CheckMedia(ctx, recording); err != nil {
			return nil, err
		}
		if pool.Transcriber == nil {
			pool.Transcriber = meta.Client{Model: settings.MetaModel}
		}
		callback := pool.OnComplete
		pool.OnComplete = func(parent context.Context, result meta.Result) error {
			commit, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer cancel()
			if err := j.SaveResult(commit, result); err != nil {
				return err
			}
			if callback != nil {
				return callback(parent, result)
			}
			return nil
		}
		if _, err := pool.Run(ctx, recording, pending, apiKey, settings.MetaMode); err != nil {
			return nil, err
		}
	}
	if err := j.Finish(ctx); err != nil {
		return nil, err
	}
	results, _, err := j.Results(ctx)
	return results, err
}
