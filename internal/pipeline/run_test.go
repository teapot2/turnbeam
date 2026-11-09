package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/alignment"
	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/asr"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type fakeMedia struct {
	path                     string
	prepare, detect, extract int
	fail, detectFail         error
}

func (f *fakeMedia) Prepare(context.Context, string) (*media.Prepared, error) {
	f.prepare++
	if f.fail != nil {
		return nil, f.fail
	}
	return &media.Prepared{Path: f.path, Samples: media.SampleRate, Duration: time.Second}, nil
}
func (f *fakeMedia) DetectSilence(context.Context, *media.Prepared) ([]media.Silence, error) {
	f.detect++
	return nil, f.detectFail
}
func (f *fakeMedia) Extract(_ context.Context, _ *media.Prepared, start, end int64) (*media.Clip, error) {
	f.extract++
	return &media.Clip{Prepared: &media.Prepared{Path: f.path, Samples: end - start, Duration: time.Second}, StartSample: start, EndSample: end}, nil
}

type fakeDiarizer struct {
	path  string
	calls int
	fail  error
}

// Diarize returns the fixture RTTM and, like the real adapter, its parsed timeline.
func (f *fakeDiarizer) Diarize(ctx context.Context, recording *media.Prepared) (*diarization.Result, error) {
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	raw, err := os.Open(f.path)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	timeline, err := diarization.ParseRTTM(ctx, raw, "recording", recording.Duration)
	if err != nil {
		return nil, err
	}
	return &diarization.Result{Timeline: timeline, RTTMPath: f.path, RuntimeVersion: setup.RuntimeVersion, ModelRevision: setup.ModelRevision}, nil
}

type fakeCredentials struct {
	calls int
	fail  error
}

func (f *fakeCredentials) Resolve(context.Context) (string, credentials.Source, error) {
	f.calls++
	return "fake-private-key", credentials.Environment, f.fail
}

type transcriberFunc func(context.Context, *media.Clip, string, string) (*meta.Response, error)

func (f transcriberFunc) Transcribe(ctx context.Context, c *media.Clip, key, mode string) (*meta.Response, error) {
	return f(ctx, c, key, mode)
}

type fixture struct {
	runner     Runner
	input, dir string
	media      *fakeMedia
	diarizer   *fakeDiarizer
	auth       *fakeCredentials
	calls      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "public.wav")
	normalized := filepath.Join(dir, "normal.wav")
	raw := filepath.Join(dir, "native.rttm")
	for path, data := range map[string]string{input: "original test bytes", normalized: "normalized test bytes", raw: "SPEAKER recording 1 0.000 1.000 <NA> <NA> A <NA> <NA>\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{input: input, dir: dir, media: &fakeMedia{path: normalized}, diarizer: &fakeDiarizer{path: raw}, auth: &fakeCredentials{}}
	f.runner = Runner{Media: f.media, Diarizer: f.diarizer, Credentials: f.auth, CacheDir: filepath.Join(dir, "jobs"), Pool: meta.Pool{Transcriber: transcriberFunc(func(_ context.Context, _ *media.Clip, key, mode string) (*meta.Response, error) {
		f.calls++
		if key != "fake-private-key" || mode != meta.Diarization {
			t.Error("inference options changed")
		}
		return &meta.Response{SessionID: "session", Transcript: "Hello.", AudioDurationMS: 1040, Turns: []transcript.RawTurn{{TurnID: 0, StartMS: 200, EndMS: 800, Transcript: "Hello."}}}, nil
	})}}
	return f
}

func TestFullPipelineStdoutFileAndOfflineResume(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := f.runner.Run(ctx, f.input, Options{}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "[00:00:00.200] **Speaker 1:** Hello.\n" || !strings.Contains(stderr.String(), "==> Preparing audio\n    1s of audio\n==> Finding speakers (on device)\n    1 speaker\n==> Splitting at pauses\n    1 chunk\n==> Transcribing with Meta Muse\n    chunk 1 done (1/1)\n    1 chunk\n") || !strings.Contains(stderr.String(), ": 1 turn, 1 speaker.\n") || strings.Contains(stderr.String(), "Hello") || strings.Contains(stdout.String()+stderr.String(), "fake-private-key") {
		t.Fatal("stdout/stderr contract failed", stdout.String(), stderr.String())
	}
	if f.calls != 1 || f.media.prepare != 1 || f.diarizer.calls != 1 || f.auth.calls != 1 {
		t.Fatal("stages were not composed exactly once")
	}
	outDir := t.TempDir()
	path := filepath.Join(outDir, "meeting.md")
	f.media.fail = errors.New("must not prepare cached work")
	f.auth.fail = errors.New("must not resolve cached credentials")
	f.diarizer.fail = errors.New("must not rerun cached native work")
	stdout.Reset()
	stderr.Reset()
	if err := f.runner.Run(ctx, f.input, Options{Resume: true, Output: path, Render: transcript.RenderOptions{Timestamps: "none"}}, &stdout, &stderr); err != nil {
		t.Fatal("resume was not offline", err)
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "Preparing audio") || strings.Contains(stderr.String(), "Finding speakers") || !strings.Contains(stderr.String(), "already transcribed; reusing saved results") {
		t.Fatal("cached/file workflow emitted incorrect output")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "**Speaker 1:** Hello.\n" {
		t.Fatal("requested output was not written", err)
	}
	data, err = os.ReadFile(filepath.Join(outDir, "meeting.transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transcript.Decode(data)
	if err != nil || canonical.Turns[0].StartMS != 200 || canonical.Turns[0].EndMS != 800 || canonical.Turns[0].SpeakerID == nil {
		t.Fatal("canonical sidecar lost timing/identity", err)
	}
	if f.calls != 1 || f.media.prepare != 1 || f.auth.calls != 1 {
		t.Fatal("resume repeated expensive work")
	}
	if !strings.Contains(stderr.String(), "Saved to "+path+".\n") {
		t.Fatal("saved path not reported", stderr.String())
	}
	// A re-run keeps the existing pair and publishes under the next free name,
	// reporting it even in quiet mode.
	stderr.Reset()
	var saved, savedSidecar string
	opts := Options{Resume: true, Output: path, Render: transcript.RenderOptions{Timestamps: "none"}, Progress: progress.Options{Quiet: true},
		Saved: func(path, sidecar string) { saved, savedSidecar = path, sidecar }}
	if err := f.runner.Run(ctx, f.input, opts, io.Discard, &stderr); err != nil {
		t.Fatal("re-run with an existing output failed", err)
	}
	renamed := filepath.Join(outDir, "meeting-2.md")
	if saved != renamed || savedSidecar != filepath.Join(outDir, "meeting-2.transcript.json") {
		t.Fatal("saved paths not reported to the caller", saved, savedSidecar)
	}
	if stderr.String() != "Saved to "+renamed+" because meeting.md already exists.\n" {
		t.Fatalf("renamed output not reported in quiet mode: %q", stderr.String())
	}
	for _, name := range []string{path, renamed} {
		if data, err := os.ReadFile(name); err != nil || string(data) != "**Speaker 1:** Hello.\n" {
			t.Fatal("existing or renamed output changed", name, err)
		}
	}
	if _, err := os.Stat(savedSidecar); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatal("re-run caused a paid request")
	}
}

func TestFailuresNeverPublishAndFreshDoesNotImplicitlyResume(t *testing.T) {
	for _, mode := range []string{"credentials", "media", "native", "remote", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			want := errors.New("simulated failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "credentials":
				f.auth.fail = want
			case "media":
				f.media.fail = want
			case "native":
				f.diarizer.fail = want
			case "remote":
				f.runner.Pool.Transcriber = transcriberFunc(func(context.Context, *media.Clip, string, string) (*meta.Response, error) { return nil, want })
			case "cancel":
				cancel()
				want = context.Canceled
			}
			dir := t.TempDir()
			var stdout bytes.Buffer
			err := f.runner.Run(ctx, f.input, Options{Output: filepath.Join(dir, "out.md")}, &stdout, io.Discard)
			if !errors.Is(err, want) || stdout.Len() != 0 {
				t.Fatal("failure lost its cause or emitted transcript", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("failure left completed/temporary output", err)
			}
		})
	}
	f := newFixture(t)
	ctx := context.Background()
	if err := f.runner.Run(ctx, f.input, Options{}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Run(ctx, f.input, Options{Fresh: true}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || f.diarizer.calls != 2 || f.auth.calls != 2 {
		t.Fatal("fresh silently reused prior results")
	}
}

func TestEarlyValidationAndQuietProgress(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	for _, opts := range []Options{{Resume: true, Fresh: true}, {Concurrency: -1}, {Mode: "invalid"}, {Render: transcript.RenderOptions{Format: "invalid"}}} {
		opts.Output = filepath.Join(dir, "out.md")
		if err := f.runner.Run(context.Background(), f.input, opts, io.Discard, io.Discard); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if f.auth.calls != 0 {
		t.Fatal("invalid options reached credentials")
	}
	var stdout, stderr bytes.Buffer
	opts := Options{}
	opts.Progress.Quiet = true
	if err := f.runner.Run(context.Background(), f.input, opts, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || stdout.Len() == 0 {
		t.Fatal("quiet suppressed transcript or leaked routine progress")
	}
}

func TestResumeAllSavedChunksBeforeFinishNeedsNoInference(t *testing.T) {
	f := newFixture(t)
	want := errors.New("stopped after successful chunk save")
	f.runner.Pool.OnProgress = func(_ context.Context, e meta.Progress) error {
		if e.Kind == meta.ProgressCompleted {
			return want
		}
		return nil
	}
	var stdout bytes.Buffer
	if err := f.runner.Run(context.Background(), f.input, Options{}, &stdout, io.Discard); !errors.Is(err, want) || stdout.Len() != 0 {
		t.Fatal("interrupted meeting looked complete", err)
	}
	f.runner.Pool.OnProgress = nil
	f.media.fail = want
	f.auth.fail = want
	f.diarizer.fail = want
	if err := f.runner.Run(context.Background(), f.input, Options{Resume: true}, &stdout, io.Discard); err != nil || stdout.Len() == 0 {
		t.Fatal("saved chunks needed audio or credentials before finalization", err)
	}
	if f.calls != 1 {
		t.Fatal("finalization repeated a paid request")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("stderr closed") }

func TestBrokenProgressStreamDoesNotFailPaidWork(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "meeting.md")
	if err := f.runner.Run(context.Background(), f.input, Options{Output: path}, io.Discard, brokenWriter{}); err != nil {
		t.Fatal("broken stderr failed or canceled transcription", err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 || f.calls != 1 {
		t.Fatal("transcript was not published once", err)
	}
}

// A crash after the speaker timeline is saved but before chunk planning must
// resume without running the native model again.
func TestResumeAfterDiarizationBeforePlanning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.media.detectFail = errors.New("simulated crash before planning")
	if err := f.runner.Run(ctx, f.input, Options{}, io.Discard, io.Discard); err == nil || f.diarizer.calls != 1 || f.calls != 0 {
		t.Fatalf("first run: err=%v diarizer=%d meta=%d", err, f.diarizer.calls, f.calls)
	}
	f.media.detectFail = nil
	f.diarizer.fail = errors.New("must reuse the saved speaker timeline")
	var stdout bytes.Buffer
	if err := f.runner.Run(ctx, f.input, Options{Resume: true}, &stdout, io.Discard); err != nil {
		t.Fatal("resume did not reuse the saved timeline", err)
	}
	if f.diarizer.calls != 1 || f.media.detect != 2 || f.calls != 1 || stdout.Len() == 0 {
		t.Fatalf("resume reran the native model or skipped planning: diarizer=%d detect=%d meta=%d", f.diarizer.calls, f.media.detect, f.calls)
	}
}

// If the normalized audio no longer matches the checkpoint, resuming must stop
// before any paid request and publish nothing.
func TestResumeRejectsChangedNormalizedAudio(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	transcriber := f.runner.Pool.Transcriber
	f.runner.Pool.Transcriber = transcriberFunc(func(context.Context, *media.Clip, string, string) (*meta.Response, error) {
		f.calls++
		return nil, &meta.Error{Kind: meta.ErrHTTP, StatusCode: 400}
	})
	if err := f.runner.Run(ctx, f.input, Options{}, io.Discard, io.Discard); err == nil {
		t.Fatal("first run should fail at transcription")
	}
	paid := f.calls
	f.runner.Pool.Transcriber = transcriber
	if err := os.WriteFile(f.media.path, []byte("different normalized bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "meeting.md")
	err := f.runner.Run(ctx, f.input, Options{Resume: true, Output: out}, io.Discard, io.Discard)
	if !errors.Is(err, jobs.ErrMismatch) || f.calls != paid {
		t.Fatalf("changed audio: err=%v requests=%d (before %d)", err, f.calls, paid)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a transcript was published for mismatched audio")
	}
}

type fakeRecognizer struct {
	calls int
	model string
	words []asr.Word
	fail  error
}

func (f *fakeRecognizer) Transcribe(_ context.Context, _ *media.Prepared, model setup.LocalModel) ([]asr.Word, error) {
	f.calls++
	f.model = model.Name
	return f.words, f.fail
}

func localFixture(t *testing.T) (*fixture, *fakeRecognizer) {
	t.Helper()
	f := newFixture(t)
	recognizer := &fakeRecognizer{words: []asr.Word{{Text: "Hello.", Start: 200 * time.Millisecond, End: 800 * time.Millisecond}}}
	f.runner.Transcriber = recognizer
	return f, recognizer
}

func TestLocalModelsNeedNoKeyMetaOrCache(t *testing.T) {
	f, recognizer := localFixture(t)
	var stdout, stderr bytes.Buffer
	if err := f.runner.Run(context.Background(), f.input, Options{Model: "parakeet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "[00:00:00.200] **Speaker 1:** Hello.\n" {
		t.Fatalf("stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "==> Preparing audio\n    1s of audio\n==> Transcribing (on device)\n    1 word\n==> Finding speakers (on device)\n    1 speaker\n==> Matching words to speakers\n") || strings.Contains(stderr.String(), "Hello") {
		t.Fatalf("stderr: %s", stderr.String())
	}
	if recognizer.calls != 1 || recognizer.model != "parakeet" || f.diarizer.calls != 1 || f.auth.calls != 0 || f.calls != 0 || f.media.detect != 0 {
		t.Fatalf("stages: recognizer %d, diarizer %d, key %d, Meta %d, silence %d", recognizer.calls, f.diarizer.calls, f.auth.calls, f.calls, f.media.detect)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "jobs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local run wrote a checkpoint: %v", err)
	}

	stdout.Reset()
	opts := Options{Model: "nemotron-3.5", Render: transcript.RenderOptions{Format: transcript.FormatJSON}}
	if err := f.runner.Run(context.Background(), f.input, opts, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	doc, err := transcript.Decode(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	p := doc.Pipeline
	nemotron, _ := setup.FindLocalModel("nemotron-3.5")
	if recognizer.model != "nemotron-3.5" || p.Engine != transcript.EngineLocal || p.ASRModel != nemotron.File || p.ASRRevision != nemotron.Revision || p.MetaModel != "" || p.DiarizationRevision != setup.ModelRevision || len(doc.SourceChunks) != 0 {
		t.Fatalf("local provenance: %+v", doc)
	}
}

type fakeApple struct {
	language string
	calls    int
}

func (f *fakeApple) Transcribe(_ context.Context, _ *media.Prepared, language string) (apple.Result, error) {
	f.calls++
	f.language = language
	return apple.Result{Locale: "es-MX", OS: "Version 27.0 (Build test)", Words: []asr.Word{{Text: "Hola.", Start: 200 * time.Millisecond, End: 800 * time.Millisecond}}}, nil
}

func TestAppleModel(t *testing.T) {
	f, recognizer := localFixture(t)
	fake := &fakeApple{}
	f.runner.Apple = fake
	var stdout bytes.Buffer
	opts := Options{Model: apple.Model, Language: "es-MX", Render: transcript.RenderOptions{Format: transcript.FormatJSON}}
	if err := f.runner.Run(context.Background(), f.input, opts, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	doc, err := transcript.Decode(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 || fake.language != "es-MX" || recognizer.calls != 0 || f.auth.calls != 0 || f.calls != 0 ||
		doc.Pipeline.Engine != transcript.EngineLocal || doc.Pipeline.ASRModel != "Apple SpeechTranscriber (es-MX)" || doc.Pipeline.ASRRevision != "Version 27.0 (Build test)" ||
		len(doc.Turns) != 1 || doc.Turns[0].Text != "Hola." {
		t.Fatalf("apple run: %+v", doc)
	}
	for _, bad := range []Options{{Model: apple.Model, Resume: true}, {Model: "parakeet", Language: "es-MX"}, {Model: apple.Model, Language: "es MX"}} {
		if err := f.runner.Run(context.Background(), f.input, bad, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid options accepted: %+v", bad)
		}
	}
	// Without an injected recognizer, an unbuilt helper fails before any work.
	f.runner.Apple = nil
	prepares := f.media.prepare
	if err := f.runner.Run(context.Background(), f.input, Options{Model: apple.Model, InstallDir: t.TempDir()}, io.Discard, io.Discard); !errors.Is(err, apple.ErrNotSetUp) || f.media.prepare != prepares {
		t.Fatalf("unbuilt helper: %v", err)
	}
}

func TestCustomModelFile(t *testing.T) {
	f, recognizer := localFixture(t)
	path := filepath.Join(f.dir, "fine-tuned.gguf")
	if err := os.WriteFile(path, []byte("gguf"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	opts := Options{Model: path, Render: transcript.RenderOptions{Format: transcript.FormatJSON}, Progress: progress.Options{Quiet: true}}
	if err := f.runner.Run(context.Background(), f.input, opts, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	doc, err := transcript.Decode(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if recognizer.model != path || doc.Pipeline.Engine != transcript.EngineLocal || doc.Pipeline.ASRModel != "fine-tuned.gguf" ||
		doc.Pipeline.ASRRevision != "sha256:1cb1b7e0f8b96cee3445e317b8064d8805bf35c7dc7de82cddcb9f78d4c95e0e" {
		t.Fatalf("custom model provenance: %s %+v", recognizer.model, doc.Pipeline)
	}
	if !strings.Contains(stderr.String(), "Warning: fine-tuned.gguf is your own model file") {
		t.Fatalf("quiet run hid the custom model warning: %q", stderr.String())
	}
	calls := recognizer.calls
	if err := f.runner.Run(context.Background(), f.input, Options{Model: filepath.Join(f.dir, "missing.gguf")}, io.Discard, io.Discard); !errors.Is(err, setup.ErrCustomModel) {
		t.Fatalf("missing model file: %v", err)
	}
	if err := f.runner.Run(context.Background(), f.input, Options{Model: path, Resume: true}, io.Discard, io.Discard); err == nil {
		t.Fatal("resume accepted for a custom model")
	}
	if recognizer.calls != calls {
		t.Fatal("a rejected model file was transcribed")
	}
}

func TestLocalModelFailures(t *testing.T) {
	ctx := context.Background()
	f, recognizer := localFixture(t)
	for _, opts := range []Options{{Model: "parakeet", Resume: true}, {Model: "parakeet", Fresh: true}, {Model: "whisper"}} {
		if err := f.runner.Run(ctx, f.input, opts, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
	if f.media.prepare != 0 || recognizer.calls != 0 {
		t.Fatal("invalid options reached processing")
	}

	recognizer.fail = &setup.ModelError{Name: "parakeet", Err: os.ErrNotExist}
	var stdout bytes.Buffer
	if err := f.runner.Run(ctx, f.input, Options{Model: "parakeet"}, &stdout, io.Discard); !errors.Is(err, setup.ErrLocalModel) {
		t.Fatalf("missing local model: %v", err)
	}
	if f.diarizer.calls != 0 || stdout.Len() != 0 {
		t.Fatal("diarization ran or output was written after transcription failed")
	}

	recognizer.fail = nil
	f.diarizer.fail = diarization.ErrRTTM
	output := filepath.Join(f.dir, "meeting.md")
	if err := f.runner.Run(ctx, f.input, Options{Model: "parakeet", Output: output}, io.Discard, io.Discard); !errors.Is(err, diarization.ErrRTTM) {
		t.Fatalf("diarization failure: %v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete local run published output")
	}

	f.diarizer.fail = nil
	recognizer.words = []asr.Word{{Text: "late", Start: 900 * time.Millisecond, End: 2 * time.Second}}
	if err := f.runner.Run(ctx, f.input, Options{Model: "parakeet"}, io.Discard, io.Discard); !errors.Is(err, alignment.ErrAlignment) {
		t.Fatalf("word past the recording accepted: %v", err)
	}
}
