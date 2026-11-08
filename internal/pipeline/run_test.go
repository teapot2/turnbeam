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

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
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

func (f *fakeDiarizer) Diarize(context.Context, *media.Prepared) (*diarization.Result, error) {
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	return &diarization.Result{RTTMPath: f.path, RuntimeVersion: setup.RuntimeVersion, ModelRevision: setup.ModelRevision}, nil
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
	if err := f.runner.Run(ctx, f.input, Options{Resume: true, Output: path}, io.Discard, io.Discard); !errors.Is(err, output.ErrConflict) {
		t.Fatal("existing output not protected", err)
	}
	if f.calls != 1 {
		t.Fatal("conflict caused a paid request")
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
