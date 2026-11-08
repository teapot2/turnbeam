package meta

import (
	"context"
	"sync"
	"time"
)

// ProgressKind describes worker activity, not completed meeting/output state.
type ProgressKind uint8

const (
	ProgressQueued ProgressKind = iota
	ProgressWorkers
	ProgressCompleted
	ProgressRetry
	ProgressFinished
	ProgressStopped
	// ProgressSending marks a request for Chunk being sent; Attempt is 1-based.
	ProgressSending
)

// Progress contains safe numbers only. Total/Completed refer to this Run's queued
// subset, excluding cached chunks. Active includes extraction, upload and backoff.
// Completed advances only after the existing completion callback succeeds.
type Progress struct {
	Kind                       ProgressKind
	Total, Completed, Active   int
	Chunk, Attempt, HTTPStatus int
	RetryDelay                 time.Duration
}

type poolProgress struct {
	mu       sync.Mutex
	state    Progress
	callback func(context.Context, Progress) error
}

func (p *poolProgress) send(ctx context.Context, kind ProgressKind, active, completed, chunk, attempt, status int, delay time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Active += active
	p.state.Completed += completed
	event := p.state
	event.Kind, event.Chunk, event.Attempt, event.HTTPStatus, event.RetryDelay = kind, chunk, attempt, status, delay
	if p.callback != nil {
		return p.callback(ctx, event)
	}
	return nil
}
