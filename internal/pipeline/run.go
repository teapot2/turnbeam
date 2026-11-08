// Package pipeline composes media, recovery, attribution and completed output.
package pipeline

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/alignment"
	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type processor interface {
	Prepare(context.Context, string) (*media.Prepared, error)
	DetectSilence(context.Context, *media.Prepared) ([]media.Silence, error)
	Extract(context.Context, *media.Prepared, int64, int64) (*media.Clip, error)
}
type diarizer interface {
	Diarize(context.Context, *media.Prepared) (*diarization.Result, error)
}
type resolver interface {
	Resolve(context.Context) (string, credentials.Source, error)
}

// ErrInput identifies an unavailable original recording without echoing its path.
var ErrInput = errors.New("inspect recording: expected an accessible regular file")

// Runner uses product implementations by default. Dependencies sit at their
// consuming boundary; injection permits complete offline workflow tests.
type Runner struct {
	Media       processor
	Diarizer    diarizer
	Credentials resolver
	Pool        meta.Pool
	CacheDir    string
}

// Options separates inference/recovery choices from output presentation.
type Options struct {
	Mode          string
	Concurrency   int
	Resume, Fresh bool
	Output        string
	Render        transcript.RenderOptions
	Progress      progress.Options
	// InstallDir locates the managed runtime/model; empty uses setup.DefaultDir.
	InstallDir string
}

// Run writes no transcript until all processing and owned-resource cleanup succeed.
// Cached complete work needs no media tools, model installation or credential.
func (r Runner) Run(ctx context.Context, input string, opts Options, stdout, stderr io.Writer) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode := strings.ToUpper(opts.Mode)
	if mode == "" {
		mode = meta.Diarization
	}
	if opts.Resume && opts.Fresh || opts.Concurrency < 0 || mode != meta.Diarization && mode != meta.Endpointing {
		return errors.New("invalid pipeline options")
	}
	if err := opts.Render.Validate(); err != nil {
		return err
	}
	// Reject missing/nonregular input before creating any cache/output artifacts.
	stat, err := os.Stat(input)
	if err != nil {
		return errors.Join(ErrInput, err)
	}
	if !stat.Mode().IsRegular() {
		return errors.Join(ErrInput, media.ErrInvalidMedia)
	}
	started := time.Now()
	reporter := progress.New(stderr, opts.Progress)
	// Progress is diagnostics only: a broken stderr must not fail a published transcript.
	defer reporter.Close()
	if err = reporter.Stage(ctx, progress.Probe); err != nil {
		return err
	}
	destination, err := output.Open(ctx, opts.Output, opts.Render, stdout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	canonical, err := r.process(ctx, input, mode, opts, reporter)
	if err != nil {
		return err
	}
	if err = reporter.Stage(ctx, progress.Write); err != nil {
		return err
	}
	if err = destination.Write(ctx, canonical); err != nil {
		return err
	}
	unknown := 0
	for _, turn := range canonical.Turns {
		if turn.SpeakerID == nil {
			unknown++
		}
	}
	// Diagnostics only: the transcript is already published.
	reporter.Summary(len(canonical.Turns), len(canonical.Speakers), unknown, time.Since(started))
	return nil
}

func (r Runner) process(ctx context.Context, input, mode string, opts Options, reporter *progress.Reporter) (result *transcript.Transcript, err error) {
	defer func() {
		if err != nil {
			result = nil
		}
	}()
	settings := jobs.DefaultSettings(mode)
	j, err := jobs.Open(ctx, input, settings, jobs.Options{Dir: r.CacheDir, Resume: opts.Resume, Fresh: opts.Fresh})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, j.Close()) }()
	info, err := j.Info()
	if err != nil {
		return nil, err
	}
	var prepared *media.Prepared
	var key string
	if info.TranscriptionReady {
		reporter.ReusingResults()
	} else {
		provider := r.Credentials
		if provider == nil {
			provider = credentials.Provider{}
		}
		key, _, err = provider.Resolve(ctx)
		if err != nil {
			return nil, err
		}
		p := r.Media
		if p == nil {
			p = media.Processor{}
		}
		if err = reporter.Stage(ctx, progress.Normalize); err != nil {
			return nil, err
		}
		prepared, err = p.Prepare(ctx, input)
		if prepared != nil {
			defer func() { err = errors.Join(err, prepared.Close()) }()
		}
		if err != nil {
			return nil, err
		}
		if !prepared.ValidTimeline() {
			return nil, media.ErrInvalidMedia
		}
		reporter.AudioPrepared(prepared.Duration)
		if err = j.CheckMedia(ctx, prepared); err != nil {
			return nil, err
		}
		_, cached, e := j.Diarization(ctx)
		if e != nil {
			return nil, e
		}
		if !cached {
			if err = reporter.Stage(ctx, progress.Diarize); err != nil {
				return nil, err
			}
			d := r.Diarizer
			if d == nil {
				d = diarization.Adapter{InstallDir: opts.InstallDir}
			}
			native, e := d.Diarize(ctx, prepared)
			if e != nil {
				if native != nil {
					e = errors.Join(e, native.Close())
				}
				return nil, e
			}
			if native == nil {
				return nil, diarization.ErrRTTM
			}
			err = errors.Join(j.SaveDiarization(ctx, native), native.Close())
			if err != nil {
				return nil, err
			}
			if timeline, ok, e := j.Diarization(ctx); e == nil && ok {
				reporter.SpeakersFound(len(timeline.Speakers))
			}
		}
		if j.Plan() == nil {
			if err = reporter.Stage(ctx, progress.Chunk); err != nil {
				return nil, err
			}
			timeline, _, e := j.Diarization(ctx)
			if e != nil {
				return nil, e
			}
			silence, e := p.DetectSilence(ctx, prepared)
			if e != nil {
				return nil, e
			}
			plan, e := chunking.Plan(ctx, prepared.Samples, timeline, silence)
			if e != nil {
				return nil, e
			}
			if err = j.SavePlan(ctx, plan); err != nil {
				return nil, err
			}
			reporter.ChunksPlanned(len(plan))
		}
		if r.Pool.Extractor == nil {
			r.Pool.Extractor = p
		}
		if err = reporter.Stage(ctx, progress.Transcribe); err != nil {
			return nil, err
		}
		if info, err = j.Info(); err != nil {
			return nil, err
		}
		if err = reporter.TranscribeStarting(info.Chunks, info.ChunksDone); err != nil {
			return nil, err
		}
	}
	if opts.Concurrency > 0 {
		r.Pool.Concurrency = opts.Concurrency
	}
	observer := r.Pool.OnProgress
	r.Pool.OnProgress = func(ctx context.Context, event meta.Progress) error {
		err := reporter.Transcription(ctx, event)
		if observer != nil {
			err = errors.Join(err, observer(ctx, event))
		}
		return err
	}
	rows, err := j.Transcribe(ctx, r.Pool, prepared, key)
	if err != nil {
		return nil, err
	}
	timeline, ok, err := j.Diarization(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, jobs.ErrCorrupt
	}
	for _, warning := range timeline.Warnings {
		if warning == diarization.SpeakerLimitReached {
			if err = reporter.WarnSpeakerLimit(ctx); err != nil {
				return nil, err
			}
			break
		}
	}
	if err = reporter.Stage(ctx, progress.Assemble); err != nil {
		return nil, err
	}
	canonical, err := alignment.Assemble(ctx, transcript.Recording{Name: filepath.Base(input), DurationMS: timeline.Duration.Milliseconds(), SHA256: info.InputSHA256}, transcript.Pipeline{MetaModel: settings.MetaModel, MetaMode: settings.MetaMode, DiarizationModel: settings.DiarizationModel, DiarizationRevision: settings.ModelRevision, RuntimeVersion: settings.RuntimeVersion}, j.Plan(), rows)
	if err != nil {
		return nil, err
	}
	if err = reporter.Stage(ctx, progress.Align); err != nil {
		return nil, err
	}
	result, err = alignment.Align(ctx, canonical, timeline, alignment.DefaultPolicy())
	return result, err
}
