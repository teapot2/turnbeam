package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/setup"
)

const modelURL = "https://huggingface.co/nvidia/Nemotron-3-Diarization/resolve/rev/model.gguf"

func model(done int64) setup.Progress {
	return setup.Progress{Step: setup.StepDownload, Title: "Nemotron 3 diarization model", Source: modelURL, Done: done, Total: 100_000_000}
}

func TestSetupDownloadBarOnTerminal(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	r.tty = true
	clock := time.Unix(0, 0)
	r.now = func() time.Time { return clock }
	download := func(done int64, after time.Duration) {
		t.Helper()
		clock = clock.Add(after)
		if err := r.Setup(model(done)); err != nil {
			t.Fatal(err)
		}
	}
	download(0, 0)
	download(10_000_000, 50*time.Millisecond) // within the redraw period: skipped
	download(20_000_000, 60*time.Millisecond)
	download(60_000_000, 1890*time.Millisecond) // 2.0 s after the start
	download(100_000_000, 10*time.Millisecond)  // completion always draws
	if err := r.Setup(setup.Progress{Step: setup.StepVerify}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if draws := strings.Count(got, "\r\x1b[2K"); draws != 4 {
		t.Fatalf("redraws not limited: %d in %q", draws, got)
	}
	for _, want := range []string{
		"==> Nemotron 3 diarization model\n    " + modelURL + "\n",
		"    ░░░░░░░░░░░░░░░░░░░░░░░░   0%  0.0 KB / 100.0 MB",
		"  20%  20.0 MB / 100.0 MB\r",
		"  60%  60.0 MB / 100.0 MB  30.0 MB/s",
		"    ████████████████████████ 100%  100.0 MB\n==> Verifying installation\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, " 10.0 MB /") || strings.Contains(got, "\x1b[3") {
		t.Fatalf("redraw inside the throttle period or unexpected color: %q", got)
	}
}

func TestSetupColorOnlyWhenEnabled(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	r.tty, r.color = true, true
	for _, done := range []int64{0, 100_000_000} {
		if err := r.Setup(model(done)); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); !strings.Contains(got, "\x1b[1;34m==>\x1b[0m \x1b[1mNemotron 3 diarization model\x1b[0m") || !strings.Contains(got, "\x1b[32m████") {
		t.Fatalf("colored heading or completed bar missing: %q", got)
	}
}

func TestSetupPlainLinesQuietAndValidation(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	runtime := setup.Progress{Step: setup.StepDownload, Title: "NeMo-Speech.cpp runtime", Source: "https://github.com/NVIDIA/NeMo-Speech.cpp/runtime.tar.gz", Total: 2_619}
	for _, p := range []setup.Progress{runtime, {Step: runtime.Step, Title: runtime.Title, Source: runtime.Source, Done: 1_000, Total: 2_619}, {Step: runtime.Step, Title: runtime.Title, Source: runtime.Source, Done: 2_619, Total: 2_619}, {Step: setup.StepCompatibility}} {
		if err := r.Setup(p); err != nil {
			t.Fatal(err)
		}
	}
	want := "==> NeMo-Speech.cpp runtime\n    https://github.com/NVIDIA/NeMo-Speech.cpp/runtime.tar.gz\n    Downloaded 2.6 KB\n==> Checking Metal compatibility\n"
	if out.String() != want {
		t.Fatalf("non-terminal output = %q, want %q", out.String(), want)
	}

	out.Reset()
	quiet := New(&out, Options{Quiet: true})
	quiet.tty = true
	if err := quiet.Setup(model(0)); err != nil || out.Len() != 0 {
		t.Fatal("quiet setup wrote progress", err)
	}

	signed := model(0)
	signed.Source = modelURL + "?X-Amz-Signature=secret"
	credentials := model(0)
	credentials.Source = "https://user:pass@example.invalid/model"
	inconsistent := model(0)
	inconsistent.Done = inconsistent.Total + 1
	for _, p := range []setup.Progress{signed, credentials, inconsistent, {Step: setup.StepDownload, Source: modelURL}, {Step: setup.StepVerify, Title: "extra"}, {Step: setup.StepCompatibility + 1}} {
		if err := New(&out, Options{}).Setup(p); !errors.Is(err, ErrEvent) {
			t.Fatalf("invalid event accepted: %+v", p)
		}
	}
}
