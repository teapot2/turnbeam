package media

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/testutil"
)

func TestParseSilence(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       []Silence
	}{
		{"active", "", []Silence{}},
		{"silent", "frame:11 pts:11264 pts_time:0.469333\nlavfi.silence_start=0\n", []Silence{{0, 2 * time.Second}}},
		{"interior and tail", "lavfi.silence_start=0.1\nlavfi.silence_end=0.9\nlavfi.silence_duration=0.8\nlavfi.silence_start=1.2\n", []Silence{{100 * time.Millisecond, 900 * time.Millisecond}, {1200 * time.Millisecond, 2 * time.Second}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSilence([]byte(tc.data), 2*time.Second)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for _, data := range []string{
		"garbage", "lavfi.silence_start=NaN", "lavfi.silence_start=-1", "lavfi.silence_start=2",
		"lavfi.silence_end=1", "lavfi.silence_start=1\nlavfi.silence_end=0.5",
		"lavfi.silence_start=0\nlavfi.silence_start=1", "lavfi.silence_start=0\nlavfi.silence_end=3",
		"lavfi.silence_start=0\nlavfi.silence_end=1\nlavfi.silence_start=0.5",
		"unknown=0", strings.Repeat("x", 65537),
	} {
		if got, err := parseSilence([]byte(data), 2*time.Second); got != nil || !errors.Is(err, ErrInvalidMedia) {
			t.Fatalf("invalid metadata accepted: %+v, %v", got, err)
		}
	}
}

func TestDetectSilence(t *testing.T) {
	recording := &Prepared{Path: inputFile(t), Samples: 48000, Duration: 2 * time.Second}
	p := fakeProcessor(t, "silence", "")
	command := p.command
	p.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		for _, flag := range []string{"silencedetect=noise=-40dB:d=0.500", "ametadata=mode=print:file=-", "-protocol_whitelist file", "-f null -"} {
			if !strings.Contains(joined, flag) {
				t.Fatalf("missing silence contract %q", flag)
			}
		}
		return command(ctx, tool, args...)
	}
	got, err := p.DetectSilence(context.Background(), recording)
	if err != nil || !reflect.DeepEqual(got, []Silence{{250 * time.Millisecond, 1250 * time.Millisecond}}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	long := &Prepared{Path: recording.Path, Samples: 14400 * SampleRate, Duration: 4 * time.Hour}
	if got, err := fakeProcessor(t, "silence-many", "").DetectSilence(context.Background(), long); err != nil || len(got) != 12000 || got[11999] != (Silence{11999 * time.Second, 11999*time.Second + 500*time.Millisecond}) {
		t.Fatalf("long recording silence metadata was truncated or rejected: %d, %v", len(got), err)
	}
	for _, mode := range []string{"silence-invalid", "silence-failure"} {
		p = fakeProcessor(t, mode, "")
		got, err := p.DetectSilence(context.Background(), recording)
		if got != nil || err == nil || strings.Contains(err.Error(), "private native metadata") {
			t.Fatalf("partial/unsafe result: %+v, %v", got, err)
		}
		if mode == "silence-failure" {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal("child status lost")
			}
		}
	}
	if _, err := p.DetectSilence(context.Background(), nil); !errors.Is(err, ErrInvalidMedia) {
		t.Fatal(err)
	}
	recording.Path = filepath.Join(t.TempDir(), "missing")
	if _, err := p.DetectSilence(context.Background(), recording); err == nil {
		t.Fatal("missing audio accepted")
	}
}

func TestSilenceCancellation(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	p := fakeProcessor(t, "silence-hang", ready)
	recording := &Prepared{Path: inputFile(t), Samples: 48000, Duration: 2 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.DetectSilence(ctx, recording); done <- err }()
	testutil.WaitReady(t, ready, done, cancel)
	cancel()
	err := testutil.WaitError(t, done)
	var exit *exec.ExitError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exit) {
		t.Fatalf("cancellation/child status lost: %v", err)
	}
	if _, err := p.DetectSilence(ctx, recording); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertEmpty(t, p.TempDir)
}
