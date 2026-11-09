package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"
)

const (
	defaultWaitSeconds = 20
	maxWaitSeconds     = 300
	cancelWait         = 5 * time.Second
	// maxFinishedJobs bounds the finished jobs kept for job_status.
	maxFinishedJobs = 100
)

type jobState string

const (
	stateQueued   jobState = "queued"
	stateRunning  jobState = "running"
	stateDone     jobState = "done"
	stateFailed   jobState = "failed"
	stateCanceled jobState = "canceled"
)

func terminal(state jobState) bool {
	return state == stateDone || state == stateFailed || state == stateCanceled
}

// jobSummary counts a finished transcript; it never includes its text.
type jobSummary struct {
	Turns           int     `json:"turns"`
	Speakers        int     `json:"speakers"`
	UnknownTurns    int     `json:"unknown_turns"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// jobView is what the assistant sees of a job.
type jobView struct {
	JobID          string      `json:"job_id"`
	State          jobState    `json:"state"`
	Recording      string      `json:"recording"`
	Model          string      `json:"model"`
	Output         string      `json:"output"`
	Transcript     string      `json:"transcript"`
	Stage          string      `json:"stage,omitempty"`
	ChunksDone     int         `json:"chunks_done,omitempty"`
	ChunksTotal    int         `json:"chunks_total,omitempty"`
	ElapsedSeconds float64     `json:"elapsed_seconds"`
	Error          string      `json:"error,omitempty"`
	Hint           string      `json:"hint,omitempty"`
	Summary        *jobSummary `json:"summary,omitempty"`
}

// job is one transcription. Every change closes and replaces changed, so
// waiters wake without polling.
type job struct {
	mu      sync.Mutex
	changed chan struct{}
	cancel  context.CancelFunc

	id, input, output, sidecar, model string
	// requested is the output asked for; output is a free numbered name when
	// that one is taken, and is updated if another writer takes it meanwhile.
	requested, note string
	language        string
	render          transcript.RenderOptions
	resume          bool

	state                      jobState
	created, started, finished time.Time
	stage                      string
	done, total                int
	events                     int
	errText, hint              string
	summary                    *jobSummary
}

func (j *job) update(fn func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn()
	j.events++
	close(j.changed)
	j.changed = make(chan struct{})
}

// observe receives pipeline progress; it runs under the reporter's lock.
func (j *job) observe(event progress.Event) {
	j.update(func() {
		j.stage = event.Stage.String()
		if event.Total > 0 {
			j.done, j.total = event.Done, event.Total
		}
	})
}

func (j *job) snapshot() (jobView, <-chan struct{}, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	view := jobView{JobID: j.id, State: j.state, Recording: j.input, Model: j.model, Output: j.output, Transcript: j.sidecar,
		Error: j.errText, Hint: j.hint, Summary: j.summary}
	if !terminal(j.state) {
		view.Stage = j.stage
		view.ChunksDone, view.ChunksTotal = j.done, j.total
	}
	if !j.started.IsZero() {
		end := j.finished
		if end.IsZero() {
			end = time.Now()
		}
		view.ElapsedSeconds = math.Round(end.Sub(j.started).Seconds()*10) / 10
	}
	return view, j.changed, j.events
}

// jobTable runs transcriptions one at a time for the server's lifetime: the
// speaker and speech models share the GPU, and Meta runs are billed.
type jobTable struct {
	ctx context.Context
	run func(context.Context, *job)
	sem chan struct{}
	wg  sync.WaitGroup

	mu    sync.Mutex
	jobs  map[string]*job
	order []string
}

func newJobTable(ctx context.Context, run func(context.Context, *job)) *jobTable {
	return &jobTable{ctx: ctx, run: run, sem: make(chan struct{}, 1), jobs: map[string]*job{}}
}

func (t *jobTable) start(j *job) {
	var nonce [6]byte
	rand.Read(nonce[:])
	j.id = hex.EncodeToString(nonce[:])
	j.state, j.created, j.changed = stateQueued, time.Now(), make(chan struct{})
	j.hint = "Call job_status with this job_id to wait for the result."
	if j.note != "" {
		j.hint = j.note + " " + j.hint
	}
	ctx, cancel := context.WithCancel(t.ctx)
	j.cancel = cancel
	t.mu.Lock()
	t.jobs[j.id] = j
	t.order = append(t.order, j.id)
	t.prune()
	t.mu.Unlock()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		select {
		case t.sem <- struct{}{}:
		case <-ctx.Done():
			j.update(func() { j.state, j.finished, j.hint = stateCanceled, time.Now(), "Canceled before it started." })
			return
		}
		defer func() { <-t.sem }()
		t.run(ctx, j)
	}()
}

// prune drops the oldest finished jobs beyond the cap. Callers hold t.mu.
func (t *jobTable) prune() {
	finished := 0
	for _, id := range t.order {
		if view, _, _ := t.jobs[id].snapshot(); terminal(view.State) {
			finished++
		}
	}
	for i := 0; finished > maxFinishedJobs && i < len(t.order); {
		id := t.order[i]
		if view, _, _ := t.jobs[id].snapshot(); terminal(view.State) {
			delete(t.jobs, id)
			t.order = slices.Delete(t.order, i, i+1)
			finished--
			continue
		}
		i++
	}
}

func (t *jobTable) get(id string) (*job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	return j, ok
}

// active returns the unfinished job writing output, if any.
func (t *jobTable) active(output string) (*job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range t.order {
		if view, _, _ := t.jobs[id].snapshot(); !terminal(view.State) && view.Output == output {
			return t.jobs[id], true
		}
	}
	return nil, false
}

func (t *jobTable) list() []jobView {
	t.mu.Lock()
	defer t.mu.Unlock()
	views := make([]jobView, 0, len(t.order))
	for _, id := range t.order {
		view, _, _ := t.jobs[id].snapshot()
		views = append(views, view)
	}
	return views
}

func (t *jobTable) wait() { t.wg.Wait() }

// runJob transcribes one recording and records the outcome on the job.
func (s *Server) runJob(ctx context.Context, j *job) {
	j.update(func() { j.state, j.started = stateRunning, time.Now() })
	var sidecar string
	opts := pipeline.Options{Model: j.model, Output: j.requested, Render: j.render, Resume: j.resume, InstallDir: s.InstallDir, Language: j.language,
		Progress: progress.Options{Quiet: true, Observer: j.observe},
		Saved: func(path, side string) {
			sidecar = side
			j.update(func() { j.output, j.sidecar = path, side })
		}}
	err := s.Transcribe(ctx, j.input, opts, io.Discard, s.Stderr)
	var summary *jobSummary
	if err == nil && sidecar != "" {
		summary = summarize(sidecar)
	}
	j.update(func() {
		j.finished = time.Now()
		switch {
		case err == nil:
			j.state, j.summary = stateDone, summary
			j.hint = "Call read_transcript with the transcript path to read it."
		case ctx.Err() != nil:
			j.state, j.hint = stateCanceled, retryHint(j.model)
		default:
			j.state, j.errText, j.hint = stateFailed, s.describe(err), retryHint(j.model)
		}
	})
	if s.Verbose {
		view, _, _ := j.snapshot()
		s.log.Printf("job %s %s", j.id, view.State)
	}
}

func retryHint(model string) string {
	if model == pipeline.MetaModel {
		return "Completed chunks are checkpointed: call transcribe again with resume=true to continue without paying for them again."
	}
	return "Local runs save nothing: call transcribe again to start over."
}

// summarize counts the published sidecar's turns and speakers; a sidecar it
// cannot read leaves the summary out rather than failing a finished job.
func summarize(sidecar string) *jobSummary {
	t, err := loadTranscript(sidecar)
	if err != nil {
		return nil
	}
	summary := &jobSummary{Turns: len(t.Turns), Speakers: len(t.Speakers), DurationSeconds: float64(t.Recording.DurationMS) / 1000}
	for _, turn := range t.Turns {
		if turn.SpeakerID == nil {
			summary.UnknownTurns++
		}
	}
	return summary
}

type transcribeArgs struct {
	Path        string `json:"path"`
	Model       string `json:"model"`
	Output      string `json:"output"`
	Format      string `json:"format"`
	Timestamps  string `json:"timestamps"`
	Resume      bool   `json:"resume"`
	Language    string `json:"language"`
	WaitSeconds *int   `json:"wait_seconds"`
}

func transcribe(ctx context.Context, s *Server, c *call, raw json.RawMessage) (*toolResult, error) {
	var args transcribeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	wait, err := waitSeconds(args.WaitSeconds)
	if err != nil {
		return nil, err
	}
	input, err := s.roots.resolve(args.Path, true)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(input); err != nil || !info.Mode().IsRegular() {
		return nil, argumentError("path is not a regular file: %s", args.Path)
	}
	model := args.Model
	if model == "" {
		if model, err = s.defaultModel(ctx); err != nil {
			return nil, err
		}
	}
	if !slices.Contains(modelNames(), model) {
		return nil, argumentError("model must be one of: %s", strings.Join(modelNames(), ", "))
	}
	if _, local := setup.FindLocalModel(model); (local || model == apple.Model) && args.Resume {
		return nil, argumentError("resume applies only to model meta; local runs are not checkpointed")
	}
	if args.Language != "" && (model != apple.Model || !apple.ValidLanguage(args.Language)) {
		return nil, argumentError("language applies only to model apple and must be a tag such as en-US or es-MX")
	}
	render, err := renderOptions(args.Format, args.Timestamps, args.Output)
	if err != nil {
		return nil, err
	}
	requested := args.Output
	if requested == "" {
		requested = output.DefaultPath(input, render.Format)
	} else if strings.HasSuffix(requested, string(filepath.Separator)) {
		return nil, argumentError("output must be a file, not a directory")
	}
	if requested, err = s.roots.resolve(requested, false); err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(requested), ".transcript.json") && render.Format != transcript.FormatJSON {
		return nil, argumentError("an output named *.transcript.json must use format json")
	}
	// Existing transcripts are kept: a taken name moves this run to a numbered one.
	out, err := output.Available(requested)
	if errors.Is(err, output.ErrNoFreeName) {
		return nil, argumentError("%s and its numbered alternatives already exist; choose another output", requested)
	} else if err != nil {
		return nil, argumentError("cannot check output %s: %v", requested, err)
	}
	sidecar := sidecarFor(out)
	if strings.HasSuffix(strings.ToLower(out), ".transcript.json") {
		sidecar = out
	}
	var note string
	if out != requested {
		note = fmt.Sprintf("%s or its .transcript.json sidecar already exists, so this run writes %s (call read_transcript to read the existing transcript instead).", requested, out)
	}
	if existing, ok := s.jobs.active(out); ok {
		view, _, _ := existing.snapshot()
		return nil, argumentError("job %s is already transcribing to %s; call job_status", view.JobID, out)
	}
	j := &job{input: input, requested: requested, output: out, sidecar: sidecar, note: note, model: model, language: args.Language, render: render, resume: args.Resume}
	s.jobs.start(j)
	if s.Verbose {
		s.log.Printf("job %s started with model %s", j.id, model)
	}
	return s.await(ctx, c, j, wait)
}

type jobArgs struct {
	JobID       string `json:"job_id"`
	WaitSeconds *int   `json:"wait_seconds"`
}

func jobStatus(ctx context.Context, s *Server, c *call, raw json.RawMessage) (*toolResult, error) {
	var args jobArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	wait, err := waitSeconds(args.WaitSeconds)
	if err != nil {
		return nil, err
	}
	j, err := s.lookupJob(args.JobID)
	if err != nil {
		return nil, err
	}
	return s.await(ctx, c, j, wait)
}

func cancelJob(ctx context.Context, s *Server, c *call, raw json.RawMessage) (*toolResult, error) {
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	j, err := s.lookupJob(args.JobID)
	if err != nil {
		return nil, err
	}
	j.cancel()
	ctx, cancel := context.WithTimeout(ctx, cancelWait)
	defer cancel()
	view, err := s.waitJob(ctx, c, j, cancelWait)
	if err != nil && ctx.Err() == nil {
		return nil, err
	}
	return result(view)
}

func (s *Server) lookupJob(id string) (*job, error) {
	j, ok := s.jobs.get(id)
	if !ok {
		return nil, argumentError("unknown job_id %q; jobs exist only while this server runs, and status lists the current ones", id)
	}
	return j, nil
}

func waitSeconds(value *int) (time.Duration, error) {
	if value == nil {
		return defaultWaitSeconds * time.Second, nil
	}
	if *value < 0 || *value > maxWaitSeconds {
		return 0, argumentError("wait_seconds must be between 0 and %d", maxWaitSeconds)
	}
	return time.Duration(*value) * time.Second, nil
}

func (s *Server) await(ctx context.Context, c *call, j *job, wait time.Duration) (*toolResult, error) {
	view, err := s.waitJob(ctx, c, j, wait)
	if err != nil {
		return nil, err
	}
	return result(view)
}

// waitJob returns the job's state once it finishes or wait elapses, sending
// progress notifications along the way when the client asked for them.
func (s *Server) waitJob(ctx context.Context, c *call, j *job, wait time.Duration) (jobView, error) {
	deadline := time.Now().Add(wait)
	reported := -1
	for {
		view, changed, events := j.snapshot()
		if events > reported {
			s.progress(c, events, progressMessage(view))
			reported = events
		}
		remaining := time.Until(deadline)
		if terminal(view.State) || remaining <= 0 {
			return view, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-changed:
			timer.Stop()
		case <-timer.C:
			view, _, _ = j.snapshot()
			return view, nil
		case <-ctx.Done():
			timer.Stop()
			return view, ctx.Err()
		}
	}
}

func progressMessage(view jobView) string {
	message := string(view.State)
	if view.Stage != "" {
		message += ": " + view.Stage
	}
	if view.ChunksTotal > 0 {
		message += fmt.Sprintf(" (%d/%d chunks)", view.ChunksDone, view.ChunksTotal)
	}
	return message
}
