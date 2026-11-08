package diarization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/testutil"
)

const fakeRTTM = "SPEAKER recording 1 600.000 0.300 <NA> <NA> native_b <NA> <NA>\n" +
	"SPEAKER recording 1 0.000 0.300 <NA> <NA> native_b <NA> <NA>\n" +
	"SPEAKER recording 1 1.000 0.300 <NA> <NA> native_a <NA> <NA>\n"

func preparedFile(t *testing.T) *media.Prepared {
	t.Helper()
	path := filepath.Join(t.TempDir(), "normalized audio.wav")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	samples := int64(610 * media.SampleRate)
	if err := f.Truncate(44 + samples*2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return &media.Prepared{Path: path, Samples: samples, Duration: 610 * time.Second}
}

func fakeAdapter(t *testing.T, mode, ready string) Adapter {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Adapter{
		TempDir: t.TempDir(),
		ready: func(ctx context.Context) (setup.Installation, error) {
			return setup.Installation{Runtime: "verified-native", Model: "verified-model"}, ctx.Err()
		},
		command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestNemoProcess$", "--"}, args...)...)
			cmd.Env = append(os.Environ(), "TURNBEAM_DIARIZATION_HELPER=1", "TURNBEAM_DIARIZATION_MODE="+mode, "TURNBEAM_DIARIZATION_READY="+ready, "GORACE=atexit_sleep_ms=0")
			return cmd
		},
	}
}

func TestNemoProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_DIARIZATION_HELPER") != "1" {
		return
	}
	args := os.Args
	for args[0] != "--" {
		args = args[1:]
	}
	output := ""
	for i := range args {
		if args[i] == "--output" && i+1 < len(args) {
			output = args[i+1]
		}
	}
	mode := os.Getenv("TURNBEAM_DIARIZATION_MODE")
	switch mode {
	case "nothing":
		os.Exit(0)
	case "missing":
		os.Remove(output)
		os.Exit(0)
	case "symlink":
		os.Remove(output)
		if err := os.Symlink("/tmp/not-an-rttm", output); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "oversized":
		if err := os.Truncate(output, maxRTTMBytes+1); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	data := fakeRTTM
	if mode == "malformed" {
		data += "SPEAKER invalid\n"
	}
	if mode == "silent" {
		data = ""
	}
	if mode == "padding" {
		data = row("609.900", "0.110", "native")
	}
	if mode == "overrun" {
		data = row("609.900", "0.111", "native")
	}
	if err := os.WriteFile(output, []byte(data), 0600); err != nil {
		os.Exit(2)
	}
	if mode == "hang" {
		if err := os.WriteFile(os.Getenv("TURNBEAM_DIARIZATION_READY"), []byte("ready"), 0600); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Hour)
	}
	if mode == "exit" {
		fmt.Fprintln(os.Stderr, "private native metadata")
		os.Exit(7)
	}
	for _, name := range []string{"META_API_KEY", "GGML_BACKEND_PATH", "NEMO_SPEECH_CONFIG"} {
		if os.Getenv(name) != "" {
			os.Exit(3)
		}
	}
	// Even unexpected native stdout must not contaminate transcript output.
	fmt.Print("native output discarded")
	os.Exit(0)
}

func TestAdapterWholeRecordingAndOwnership(t *testing.T) {
	a := fakeAdapter(t, "", "")
	recording := preparedFile(t)
	t.Setenv("META_API_KEY", "test-only-secret")
	t.Setenv("GGML_BACKEND_PATH", "untrusted-test-backend")
	t.Setenv("NEMO_SPEECH_CONFIG", "untrusted-test-config")
	command := a.command
	var calls int
	var captured []string
	a.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		calls++
		captured = append([]string{tool}, args...)
		return command(ctx, tool, args...)
	}
	result, err := a.Diarize(context.Background(), recording)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if calls != 1 || len(result.Timeline.Segments) != 3 || result.Timeline.Segments[0].SpeakerID != result.Timeline.Segments[2].SpeakerID {
		t.Fatalf("inference was split or returning label changed: calls=%d, %+v", calls, result.Timeline)
	}
	for _, flag := range []string{"--backend metal", "--preset v3-offline", "--format rttm", "--recording-id recording", "--model verified-model"} {
		if !strings.Contains(strings.Join(captured, " "), flag) {
			t.Fatalf("missing native contract %q", flag)
		}
	}
	for _, arg := range captured {
		if arg == "--offline" {
			t.Fatal("full-attention offline mode was enabled")
		}
	}
	if result.RuntimeVersion != setup.RuntimeVersion || result.ModelRevision != setup.ModelRevision {
		t.Fatal("inference provenance missing")
	}
	data, err := os.ReadFile(result.RTTMPath)
	if err != nil || string(data) != fakeRTTM {
		t.Fatalf("raw RTTM was changed: %v", err)
	}
	for path, want := range map[string]os.FileMode{result.dir: 0700, result.RTTMPath: 0600} {
		stat, err := os.Stat(path)
		if err != nil || stat.Mode().Perm() != want {
			t.Fatalf("insecure result permissions: %v", err)
		}
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, a.TempDir)
	if _, err := os.Stat(recording.Path); err != nil {
		t.Fatalf("adapter removed its caller's recording: %v", err)
	}
}

func TestAdapterFailuresDoNotReturnPartialTimeline(t *testing.T) {
	for _, mode := range []string{"exit", "malformed", "missing", "nothing", "oversized", "symlink", "overrun"} {
		t.Run(mode, func(t *testing.T) {
			a := fakeAdapter(t, mode, "")
			result, err := a.Diarize(context.Background(), preparedFile(t))
			if result != nil || err == nil || strings.Contains(err.Error(), "private native metadata") {
				t.Fatalf("partial or unsafe result returned: %+v, %v", result, err)
			}
			if mode == "exit" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("native exit error was lost: %v", err)
				}
			}
			assertEmpty(t, a.TempDir)
		})
	}
}

func TestAdapterSilentOutput(t *testing.T) {
	a := fakeAdapter(t, "silent", "")
	result, err := a.Diarize(context.Background(), preparedFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if len(result.Timeline.Segments) != 0 || len(result.Timeline.Speakers) != 0 {
		t.Fatal("silent output acquired invented speakers")
	}
}

func TestAdapterRetainsRawPaddingAndReportsAdjustment(t *testing.T) {
	a := fakeAdapter(t, "padding", "")
	recording := preparedFile(t)
	result, err := a.Diarize(context.Background(), recording)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	data, err := os.ReadFile(result.RTTMPath)
	if err != nil || string(data) != row("609.900", "0.110", "native") {
		t.Fatalf("native padding evidence was changed: %v", err)
	}
	adjustments := result.Timeline.Adjustments
	if len(adjustments) != 1 || adjustments[0].RawEnd != recording.Duration+EndTolerance || adjustments[0].End != recording.Duration || result.Timeline.Segments[0].End != recording.Duration {
		t.Fatalf("bounded adjustment not reported: %+v", result.Timeline)
	}
}

func TestAdapterCancellationAndDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint("timeout=", timeout), func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			a := fakeAdapter(t, "hang", ready)
			recording := preparedFile(t)
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				want = context.DeadlineExceeded
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := a.Diarize(ctx, recording); done <- err }()
			testutil.WaitReady(t, ready, done, cancel)
			if !timeout {
				cancel()
			}
			err := testutil.WaitError(t, done)
			var exit *exec.ExitError
			if !errors.Is(err, want) || !errors.As(err, &exit) {
				t.Fatalf("cancellation/child wait was lost: %v", err)
			}
			assertEmpty(t, a.TempDir)
		})
	}
}

func TestAdapterInputAndVerificationFailures(t *testing.T) {
	a := fakeAdapter(t, "", "")
	for _, recording := range []*media.Prepared{nil, {}, {Path: "unused", Samples: math.MaxInt64, Duration: time.Second}, {Path: t.TempDir(), Samples: 24000, Duration: time.Second}} {
		if result, err := a.Diarize(context.Background(), recording); result != nil || err == nil {
			t.Fatalf("invalid input accepted: %+v, %v", result, err)
		}
	}
	a.ready = func(context.Context) (setup.Installation, error) { return setup.Installation{}, setup.ErrIntegrity }
	a.command = func(context.Context, string, ...string) *exec.Cmd {
		t.Fatal("unverified native code was executed")
		return nil
	}
	if _, err := a.Diarize(context.Background(), preparedFile(t)); !errors.Is(err, setup.ErrIntegrity) {
		t.Fatalf("installation verification failure was lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Diarize(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertEmpty(t, a.TempDir)
}

func TestAdapterDependencyAndTemporaryStorageFailures(t *testing.T) {
	a := fakeAdapter(t, "", "")
	missing := filepath.Join(t.TempDir(), "missing-native")
	a.command = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, missing, args...)
	}
	if result, err := a.Diarize(context.Background(), preparedFile(t)); result != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native launch error was lost: %+v, %v", result, err)
	}
	assertEmpty(t, a.TempDir)
	a.TempDir = filepath.Join(t.TempDir(), "missing-directory")
	if result, err := a.Diarize(context.Background(), preparedFile(t)); result != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary storage failure was lost: %+v, %v", result, err)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial RTTM remains: %v, %v", entries, err)
	}
}
