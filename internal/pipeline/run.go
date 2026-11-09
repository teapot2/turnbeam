// Package pipeline composes media, recovery, attribution and completed output.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/alignment"
	"github.com/teapot2/turnbeam/internal/asr"
	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"
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
type transcriber interface {
	Transcribe(context.Context, *media.Prepared, setup.LocalModel) ([]asr.Word, error)
}

// MetaModel selects Meta Muse transcription in Options.Model.
const MetaModel = "meta"

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
	// Transcriber recognizes speech on device with a local model.
	Transcriber transcriber
}

// Options separates inference/recovery choices from output presentation.
type Options struct {
	// Model is MetaModel (the default when empty) or a setup.LocalModel name.
	// Local models have no Meta mode or concurrency and are not checkpointed.
	Model         string
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
	local, isLocal := setup.FindLocalModel(opts.Model)
	if opts.Resume && opts.Fresh || opts.Concurrency < 0 || mode != meta.Diarization && mode != meta.Endpointing ||
		opts.Model != "" && opts.Model != MetaModel && !isLocal || isLocal && (opts.Resume || opts.Fresh) {
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
	var canonical *transcript.Transcript
	if isLocal {
		canonical, err = r.processLocal(ctx, input, local, opts, reporter)
	} else {
		canonical, err = r.process(ctx, input, mode, opts, reporter)
	}
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
	w := &invocation{Runner: r, job: j, reporter: reporter, input: input, opts: opts}
	defer func() {
		if w.prepared != nil {
			err = errors.Join(err, w.prepared.Close())
		}
	}()
	if info.TranscriptionReady {
		reporter.ReusingResults()
	} else if err = w.prepare(ctx); err != nil {
		return nil, err
	}
	rows, err := w.transcribe(ctx)
	if err != nil {
		return nil, err
	}
	return w.finish(ctx, settings, info.InputSHA256, rows)
}

// invocation holds one run's state while process moves it through the stages.
type invocation struct {
	Runner
	job      *jobs.Job
	reporter *progress.Reporter
	input    string
	opts     Options
	media    processor
	prepared *media.Prepared
	key      string
}

// prepare resolves the key, normalizes the recording and saves whatever the
// checkpoint still lacks: the speaker timeline and the chunk plan.
func (w *invocation) prepare(ctx context.Context) (err error) {
	provider := w.Credentials
	if provider == nil {
		provider = credentials.Provider{}
	}
	if w.key, _, err = provider.Resolve(ctx); err != nil {
		return err
	}
	w.media = w.Media
	if w.media == nil {
		w.media = media.Processor{}
	}
	if err = w.reporter.Stage(ctx, progress.Normalize); err != nil {
		return err
	}
	if w.prepared, err = w.media.Prepare(ctx, w.input); err != nil {
		return err
	}
	if !w.prepared.ValidTimeline() {
		return media.ErrInvalidMedia
	}
	w.reporter.AudioPrepared(w.prepared.Duration)
	if err = w.job.CheckMedia(ctx, w.prepared); err != nil {
		return err
	}
	if err = w.diarize(ctx); err != nil {
		return err
	}
	if err = w.plan(ctx); err != nil {
		return err
	}
	if w.Pool.Extractor == nil {
		w.Pool.Extractor = w.media
	}
	if err = w.reporter.Stage(ctx, progress.Transcribe); err != nil {
		return err
	}
	info, err := w.job.Info()
	if err != nil {
		return err
	}
	return w.reporter.TranscribeStarting(info.Chunks, info.ChunksDone)
}

// diarize runs the local speaker model unless the checkpoint has its timeline.
func (w *invocation) diarize(ctx context.Context) error {
	if _, cached, err := w.job.Diarization(ctx); err != nil || cached {
		return err
	}
	if err := w.reporter.Stage(ctx, progress.Diarize); err != nil {
		return err
	}
	d := w.Diarizer
	if d == nil {
		d = diarization.Adapter{InstallDir: w.opts.InstallDir}
	}
	native, err := d.Diarize(ctx, w.prepared)
	if err != nil {
		if native != nil {
			err = errors.Join(err, native.Close())
		}
		return err
	}
	if native == nil {
		return diarization.ErrRTTM
	}
	if err := errors.Join(w.job.SaveDiarization(ctx, native), native.Close()); err != nil {
		return err
	}
	if timeline, ok, err := w.job.Diarization(ctx); err == nil && ok {
		w.reporter.SpeakersFound(len(timeline.Speakers))
	}
	return nil
}

// plan splits the recording at pauses unless the checkpoint has a plan.
func (w *invocation) plan(ctx context.Context) error {
	if w.job.Plan() != nil {
		return nil
	}
	if err := w.reporter.Stage(ctx, progress.Chunk); err != nil {
		return err
	}
	timeline, _, err := w.job.Diarization(ctx)
	if err != nil {
		return err
	}
	silence, err := w.media.DetectSilence(ctx, w.prepared)
	if err != nil {
		return err
	}
	plan, err := chunking.Plan(ctx, w.prepared.Samples, timeline, silence)
	if err != nil {
		return err
	}
	if err := w.job.SavePlan(ctx, plan); err != nil {
		return err
	}
	w.reporter.ChunksPlanned(len(plan))
	return nil
}

// transcribe sends the chunks the checkpoint still lacks to Meta.
func (w *invocation) transcribe(ctx context.Context) ([]meta.Result, error) {
	if w.opts.Concurrency > 0 {
		w.Pool.Concurrency = w.opts.Concurrency
	}
	observer := w.Pool.OnProgress
	w.Pool.OnProgress = func(ctx context.Context, event meta.Progress) error {
		err := w.reporter.Transcription(ctx, event)
		if observer != nil {
			err = errors.Join(err, observer(ctx, event))
		}
		return err
	}
	return w.job.Transcribe(ctx, w.Pool, w.prepared, w.key)
}

// finish places Meta's turns on the recording clock and attributes speakers.
func (w *invocation) finish(ctx context.Context, settings jobs.Settings, inputHash string, rows []meta.Result) (*transcript.Transcript, error) {
	timeline, ok, err := w.job.Diarization(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, jobs.ErrCorrupt
	}
	if slices.Contains(timeline.Warnings, diarization.SpeakerLimitReached) {
		if err := w.reporter.WarnSpeakerLimit(ctx); err != nil {
			return nil, err
		}
	}
	if err := w.reporter.Stage(ctx, progress.Assemble); err != nil {
		return nil, err
	}
	recording := transcript.Recording{Name: filepath.Base(w.input), DurationMS: timeline.Duration.Milliseconds(), SHA256: inputHash}
	provenance := transcript.Pipeline{Engine: transcript.EngineMeta, MetaModel: settings.MetaModel, MetaMode: settings.MetaMode, DiarizationModel: settings.DiarizationModel, DiarizationRevision: settings.ModelRevision, RuntimeVersion: settings.RuntimeVersion}
	canonical, err := alignment.Assemble(ctx, recording, provenance, w.job.Plan(), rows)
	if err != nil {
		return nil, err
	}
	if err := w.reporter.Stage(ctx, progress.Align); err != nil {
		return nil, err
	}
	return alignment.Align(ctx, canonical, timeline, alignment.DefaultPolicy())
}

// processLocal transcribes the whole recording on device. Nothing is sent
// anywhere or checkpointed: a rerun costs only time. Transcription runs before
// diarization so a missing local model fails before the longer analysis.
func (r Runner) processLocal(ctx context.Context, input string, model setup.LocalModel, opts Options, reporter *progress.Reporter) (result *transcript.Transcript, err error) {
	defer func() {
		if err != nil {
			result = nil
		}
	}()
	inputHash, err := media.FileSHA256(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("fingerprint recording: %w", err)
	}
	processor := r.Media
	if processor == nil {
		processor = media.Processor{}
	}
	if err := reporter.Stage(ctx, progress.Normalize); err != nil {
		return nil, err
	}
	prepared, err := processor.Prepare(ctx, input)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, prepared.Close()) }()
	if !prepared.ValidTimeline() {
		return nil, media.ErrInvalidMedia
	}
	reporter.AudioPrepared(prepared.Duration)

	if err := reporter.Stage(ctx, progress.TranscribeLocal); err != nil {
		return nil, err
	}
	recognizer := r.Transcriber
	if recognizer == nil {
		recognizer = asr.Adapter{InstallDir: opts.InstallDir}
	}
	recognized, err := recognizer.Transcribe(ctx, prepared, model)
	if err != nil {
		return nil, err
	}
	reporter.WordsTranscribed(len(recognized))

	if err := reporter.Stage(ctx, progress.Diarize); err != nil {
		return nil, err
	}
	d := r.Diarizer
	if d == nil {
		d = diarization.Adapter{InstallDir: opts.InstallDir}
	}
	native, err := d.Diarize(ctx, prepared)
	if native != nil {
		err = errors.Join(err, native.Close())
	}
	if err != nil {
		return nil, err
	}
	if native == nil {
		return nil, diarization.ErrRTTM
	}
	if native.RuntimeVersion != setup.RuntimeVersion || native.ModelRevision != setup.ModelRevision {
		return nil, errors.New("diarization result does not match the pinned runtime and model")
	}
	timeline := native.Timeline
	reporter.SpeakersFound(len(timeline.Speakers))
	if slices.Contains(timeline.Warnings, diarization.SpeakerLimitReached) {
		if err := reporter.WarnSpeakerLimit(ctx); err != nil {
			return nil, err
		}
	}

	if err := reporter.Stage(ctx, progress.AlignWords); err != nil {
		return nil, err
	}
	words := make([]alignment.Word, len(recognized))
	for i, w := range recognized {
		words[i] = alignment.Word{Text: w.Text, Start: w.Start, End: w.End}
	}
	settings := jobs.DefaultSettings(meta.Diarization)
	recording := transcript.Recording{Name: filepath.Base(input), DurationMS: timeline.Duration.Milliseconds(), SHA256: inputHash}
	provenance := transcript.Pipeline{Engine: transcript.EngineLocal, ASRModel: model.File, ASRRevision: model.Revision,
		DiarizationModel: settings.DiarizationModel, DiarizationRevision: settings.ModelRevision, RuntimeVersion: settings.RuntimeVersion}
	return alignment.AlignWords(ctx, recording, provenance, words, timeline, alignment.DefaultPolicy())
}
