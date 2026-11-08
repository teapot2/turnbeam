package diarization

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

// Preset selects larger stateful streaming chunks, not full-attention offline mode.
const Preset = "v3-offline"

const recordingID = "recording"

// ErrCleanup reports native analysis data that could not be removed.
var ErrCleanup = errors.New("temporary diarization cleanup failed; inspect the temporary directory")

// Adapter runs exactly one diarization inference pass per complete prepared WAV.
// The caller's context supplies cancellation and any desired wall-clock deadline.
// Zero values use the default managed installation and system temporary directory.
type Adapter struct {
	InstallDir string
	TempDir    string
	ready      func(context.Context) (setup.Installation, error)
	command    func(context.Context, string, ...string) *exec.Cmd
}

// Result owns the unchanged raw RTTM until Close. Its timeline is analysis-only;
// recording checkpoints must copy the raw artifact before this result is closed.
type Result struct {
	Timeline       Timeline
	RTTMPath       string
	RuntimeVersion string
	ModelRevision  string
	dir            string
}

// Close removes only this result's private temporary directory; it is idempotent.
func (r *Result) Close() error {
	if err := os.RemoveAll(r.dir); err != nil {
		return errors.Join(ErrCleanup, err)
	}
	return nil
}

// Diarize consumes a complete WAV from media.Processor.Prepare without owning or
// modifying it. It never resets native labels at remote transcription boundaries,
// downloads models, or returns a partial result after inference/parse failure.
func (a Adapter) Diarize(ctx context.Context, recording *media.Prepared) (_ *Result, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !recording.ValidTimeline() || recording.Path == "" {
		return nil, errors.New("diarization requires a complete normalized recording")
	}
	input, err := filepath.Abs(recording.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve normalized recording: %w", err)
	}
	stat, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("inspect normalized recording: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Size() < 44 || recording.Samples > (stat.Size()-44)/2 {
		return nil, errors.New("prepared recording is nonregular or truncated")
	}
	file, err := os.Open(input)
	if err != nil {
		return nil, fmt.Errorf("open normalized recording: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close normalized recording: %w", err)
	}
	ready := a.ready
	if ready == nil {
		ready = (setup.Manager{Dir: a.InstallDir}).Ready
	}
	installed, err := ready(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare diarization runtime: %w", err)
	}
	dir, err := os.MkdirTemp(a.TempDir, "turnbeam-diarization-")
	if err != nil {
		return nil, fmt.Errorf("create private diarization directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			if cleanup := os.RemoveAll(dir); cleanup != nil {
				err = errors.Join(err, ErrCleanup, cleanup)
			}
		}
	}()
	partial := filepath.Join(dir, "partial.rttm")
	// An invalid sentinel distinguishes missing output from a genuine empty/silent
	// result while reserving the output file with private permissions before launch.
	if err := os.WriteFile(partial, []byte("INCOMPLETE\n"), 0600); err != nil {
		return nil, fmt.Errorf("create private RTTM: %w", err)
	}
	command := a.command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, installed.Runtime, "diarize", input, "--model", installed.Model,
		"--backend", "metal", "--preset", Preset, "--format", "rttm", "--recording-id", recordingID, "--output", partial, "--force")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := subprocess.Run(ctx, cmd); err != nil {
		return nil, fmt.Errorf("diarize recording: %w", err)
	}
	stat, err = os.Lstat(partial)
	if err != nil {
		return nil, fmt.Errorf("inspect native RTTM: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxRTTMBytes {
		return nil, fmt.Errorf("%w: nonregular or oversized output", ErrRTTM)
	}
	file, err = os.Open(partial)
	if err != nil {
		return nil, fmt.Errorf("open native RTTM: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure native RTTM: %w", err)
	}
	timeline, parseErr := ParseRTTM(ctx, file, recordingID, recording.Duration)
	err = errors.Join(parseErr, file.Close())
	if err != nil {
		return nil, fmt.Errorf("parse native RTTM: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output := filepath.Join(dir, "timeline.rttm")
	if err := os.Rename(partial, output); err != nil {
		return nil, fmt.Errorf("publish verified RTTM: %w", err)
	}
	complete = true
	return &Result{Timeline: timeline, RTTMPath: output, RuntimeVersion: setup.RuntimeVersion, ModelRevision: setup.ModelRevision, dir: dir}, nil
}
