package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jsonstrict"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
)

// verify checks the manifest's structure. Deep checks also read and validate
// the RTTM and every saved response; callers that read them anyway skip that.
func (j *Job) verify(ctx context.Context, deep bool) error {
	if !validHash(j.m.InputSHA256) || !j.m.Settings.valid() || j.m.Completed == nil {
		return ErrCorrupt
	}
	if j.m.Media == nil {
		if j.m.RTTM != nil || j.m.Plan != nil || len(j.m.Completed) != 0 || j.m.Complete || j.m.PlanSHA256 != "" {
			return ErrCorrupt
		}
		return nil
	}
	n := j.m.Media
	if !(&media.Prepared{Samples: n.Samples, Duration: n.duration()}).ValidTimeline() || !validHash(n.SHA256) {
		return ErrCorrupt
	}
	if j.m.RTTM != nil && deep {
		if _, err := j.timeline(ctx); err != nil {
			return fmt.Errorf("%w: diarization: %w", ErrCorrupt, err)
		}
	}
	if j.m.Plan == nil {
		if len(j.m.Completed) != 0 || j.m.Complete || j.m.PlanSHA256 != "" {
			return ErrCorrupt
		}
		return nil
	}
	if j.m.RTTM == nil {
		return ErrCorrupt
	}
	if err := chunking.Validate(ctx, *j.m.Plan, n.Samples); err != nil {
		return corrupt(err)
	}
	data, err := json.Marshal(*j.m.Plan)
	if err != nil || digest(data) != j.m.PlanSHA256 {
		return ErrCorrupt
	}
	known := map[string]bool{}
	for _, c := range *j.m.Plan {
		known[c.ID] = true
		if done, exists := j.m.Completed[c.ID]; exists && deep {
			if _, err := j.response(ctx, c, done); err != nil {
				return fmt.Errorf("%w: completed response: %w", ErrCorrupt, err)
			}
		}
	}
	for id := range j.m.Completed {
		if !known[id] {
			return ErrCorrupt
		}
	}
	if j.m.Complete && len(j.m.Completed) != len(*j.m.Plan) {
		return ErrCorrupt
	}
	return nil
}

// timeline parses the committed RTTM once per session; callers get a copy.
func (j *Job) timeline(ctx context.Context) (diarization.Timeline, error) {
	if j.parsed == nil || j.parsedFrom != j.m.RTTM.SHA256 {
		data, err := j.readArtifact(ctx, "rttm", *j.m.RTTM, maxRTTM)
		if err != nil {
			return diarization.Timeline{}, err
		}
		t, err := diarization.ParseRTTM(ctx, bytes.NewReader(data), "recording", j.m.Media.duration())
		if err != nil {
			return diarization.Timeline{}, err
		}
		j.parsed, j.parsedFrom = &t, j.m.RTTM.SHA256
	}
	return cloneTimeline(*j.parsed), nil
}

func cloneTimeline(t diarization.Timeline) diarization.Timeline {
	t.Speakers, t.Segments = slices.Clone(t.Speakers), slices.Clone(t.Segments)
	t.Adjustments, t.Warnings = slices.Clone(t.Adjustments), slices.Clone(t.Warnings)
	return t
}

func (j *Job) response(ctx context.Context, c chunking.Chunk, done completed) (*meta.Response, error) {
	if !validHash(done.AudioSHA256) || done.Attempts < 1 || done.Attempts > meta.MaxAttempts {
		return nil, ErrCorrupt
	}
	data, err := j.readArtifact(ctx, "response", done.Response, maxResponse)
	if err != nil {
		return nil, err
	}
	var closed meta.Response
	if err := jsonstrict.Decode(data, &closed); err != nil {
		return nil, corrupt(err)
	}
	return meta.DecodeResponse(data, (c.EndSample-c.StartSample)/(media.SampleRate/1000))
}

// CheckMedia verifies regenerated normalized bytes before any new inference or
// upload. A fully cached job can be read without preparing temporary audio again.
func (j *Job) CheckMedia(ctx context.Context, recording *media.Prepared) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if !recording.ValidTimeline() {
		return ErrMismatch
	}
	original, err := media.FileSHA256(ctx, j.input)
	if err != nil {
		return err
	}
	if original != j.m.InputSHA256 {
		return ErrMismatch
	}
	hash, err := media.FileSHA256(ctx, recording.Path)
	if err != nil {
		return err
	}
	n := normalized{Samples: recording.Samples, SHA256: hash}
	if j.m.Media != nil {
		if *j.m.Media != n {
			return ErrMismatch
		}
	} else {
		next := j.m
		next.Media = &n
		if err := j.commit(ctx, next); err != nil {
			return err
		}
	}
	j.mediaChecked = true
	j.checked.path, j.checked.samples = recording.Path, recording.Samples
	return nil
}

// alreadyChecked reports whether CheckMedia verified this recording this session.
func (j *Job) alreadyChecked(recording *media.Prepared) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.mediaChecked && recording != nil && recording.Path == j.checked.path && recording.Samples == j.checked.samples
}

// SaveDiarization commits unchanged RTTM independently of transcription results.
// Parsing the stored bytes reconstructs global IDs and all timing diagnostics.
func (j *Job) SaveDiarization(ctx context.Context, result *diarization.Result) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if !j.mediaChecked || result == nil || result.RuntimeVersion != j.m.Settings.RuntimeVersion || result.ModelRevision != j.m.Settings.ModelRevision {
		return ErrMismatch
	}
	f, err := os.Open(result.RTTMPath)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		f.Close()
		return errors.Join(ErrState, err)
	}
	data, readErr := readBounded(ctx, f, maxRTTM)
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return err
	}
	parsed, err := diarization.ParseRTTM(ctx, bytes.NewReader(data), "recording", j.m.Media.duration())
	if err != nil {
		return err
	}
	if j.m.RTTM != nil {
		if j.m.RTTM.SHA256 != digest(data) {
			return ErrMismatch
		}
		return nil
	}
	a, err := j.writeArtifact(ctx, "rttm", data, maxRTTM)
	if err != nil {
		return err
	}
	next := j.m
	next.RTTM = &a
	if err := j.commit(ctx, next); err != nil {
		return err
	}
	j.parsed, j.parsedFrom = &parsed, a.SHA256
	return nil
}

// Diarization reuses only committed, checksummed raw data; silence is valid.
func (j *Job) Diarization(ctx context.Context) (diarization.Timeline, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return diarization.Timeline{}, false, err
	}
	if j.m.RTTM == nil {
		return diarization.Timeline{}, false, nil
	}
	t, err := j.timeline(ctx)
	return t, err == nil, err
}

// SavePlan freezes the complete original plan, including forced overlaps.
func (j *Job) SavePlan(ctx context.Context, plan []chunking.Chunk) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if !j.mediaChecked || j.m.RTTM == nil {
		return ErrState
	}
	if err := chunking.Validate(ctx, plan, j.m.Media.Samples); err != nil {
		return errors.Join(ErrState, err)
	}
	copy := append([]chunking.Chunk(nil), plan...)
	if j.m.Plan != nil {
		if !reflect.DeepEqual(*j.m.Plan, copy) {
			return ErrMismatch
		}
		return nil
	}
	data, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	next := j.m
	next.Plan, next.PlanSHA256 = &copy, digest(data)
	return j.commit(ctx, next)
}

// Plan returns a detached copy; nil means planning has not committed yet.
func (j *Job) Plan() []chunking.Chunk {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.m.Plan == nil {
		return nil
	}
	return append([]chunking.Chunk(nil), (*j.m.Plan)...)
}

// SaveResult commits a validated allowlisted response first, then its manifest
// reference. Failed commits cannot make a partial response look complete.
func (j *Job) SaveResult(ctx context.Context, result meta.Result) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if j.m.Plan == nil || !j.mediaChecked || result.Response == nil || !validHash(result.AudioSHA256) || result.Attempts < 1 || result.Attempts > meta.MaxAttempts {
		return ErrState
	}
	found := false
	for _, c := range *j.m.Plan {
		if c == result.Chunk {
			found = true
			break
		}
	}
	if !found {
		return ErrMismatch
	}
	data, err := json.Marshal(result.Response)
	if err != nil {
		return err
	}
	if _, err := meta.DecodeResponse(data, (result.Chunk.EndSample-result.Chunk.StartSample)/(media.SampleRate/1000)); err != nil {
		return err
	}
	if old, ok := j.m.Completed[result.Chunk.ID]; ok {
		if old.Response.SHA256 != digest(data) || old.AudioSHA256 != result.AudioSHA256 || old.Attempts != result.Attempts {
			return ErrMismatch
		}
		return nil
	}
	a, err := j.writeArtifact(ctx, "response", data, maxResponse)
	if err != nil {
		return err
	}
	next := j.m
	next.Completed = make(map[string]completed, len(j.m.Completed)+1)
	for id, value := range j.m.Completed {
		next.Completed[id] = value
	}
	next.Completed[result.Chunk.ID] = completed{Response: a, AudioSHA256: result.AudioSHA256, Attempts: result.Attempts}
	return j.commit(ctx, next)
}

// Results returns only a complete, committed job, never a partial transcript.
func (j *Job) Results(ctx context.Context) ([]meta.Result, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, false, err
	}
	if !j.m.Complete {
		return nil, false, nil
	}
	results, err := j.results(ctx)
	return results, err == nil, err
}
func (j *Job) results(ctx context.Context) ([]meta.Result, error) {
	if j.m.Plan == nil || len(j.m.Completed) != len(*j.m.Plan) {
		return nil, ErrState
	}
	results := make([]meta.Result, 0, len(*j.m.Plan))
	for _, c := range *j.m.Plan {
		done, ok := j.m.Completed[c.ID]
		if !ok {
			return nil, ErrState
		}
		r, err := j.response(ctx, c, done)
		if err != nil {
			return nil, fmt.Errorf("%w: response: %w", ErrCorrupt, err)
		}
		results = append(results, meta.Result{Chunk: c, Response: r, Attempts: done.Attempts, AudioSHA256: done.AudioSHA256})
	}
	return results, nil
}

// Finish validates every reference before atomically marking a job complete.
func (j *Job) Finish(ctx context.Context) error {
	_, err := j.finish(ctx)
	return err
}

// finish reads and validates every saved response once and returns them.
func (j *Job) finish(ctx context.Context) ([]meta.Result, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, err
	}
	if err := j.verify(ctx, false); err != nil {
		return nil, err
	}
	results, err := j.results(ctx)
	if err != nil {
		return nil, err
	}
	if !j.m.Complete {
		next := j.m
		next.Complete = true
		if err := j.commit(ctx, next); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func corrupt(err error) error { return errors.Join(ErrCorrupt, err) }
