package media

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/testutil"
)

func extractionSource(t *testing.T) *Prepared {
	t.Helper()
	path := filepath.Join(t.TempDir(), "normalized.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(44 + 48000*2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return &Prepared{Path: path, Samples: 48000, Duration: 2 * time.Second}
}

func TestExtractContractAndOwnership(t *testing.T) {
	p := fakeProcessor(t, "", "")
	source := extractionSource(t)
	command := p.command
	p.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		if tool == "ffmpeg" {
			joined := strings.Join(args, " ")
			for _, want := range []string{"atrim=start_sample=1:end_sample=24001,asetpts=PTS-STARTPTS", "-c:a pcm_s16le", "-map_metadata -1", "-protocol_whitelist file", "-rf64 never", "-fs 30999999"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing extraction contract %q", want)
				}
			}
			for _, unwanted := range []string{"-ss", "-t", "-ar", "-ac"} {
				for _, arg := range args {
					if arg == unwanted {
						t.Fatalf("unexpected rounding/resampling option %s", arg)
					}
				}
			}
			stat, err := os.Stat(args[len(args)-1])
			if err != nil || stat.Mode().Perm() != 0600 {
				t.Fatal("chunk was not reserved privately")
			}
		}
		return command(ctx, tool, args...)
	}
	clip, err := p.Extract(context.Background(), source, 1, 24001)
	if err != nil {
		t.Fatal(err)
	}
	if clip.StartSample != 1 || clip.EndSample != 24001 || clip.Samples != 24000 || clip.Duration != time.Second || clip.Bytes != 48044 || clip.Bytes+RequestHeadroom >= MaxRequestBytes {
		t.Fatalf("wrong chunk metadata: %+v", clip)
	}
	stat, err := os.Stat(filepath.Dir(clip.Path))
	if err != nil || stat.Mode().Perm() != 0700 {
		t.Fatal("chunk directory is not private")
	}
	if err := clip.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clip.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source.Path); err != nil {
		t.Fatal("source removed with chunk")
	}
	assertEmpty(t, p.TempDir)
}

func TestExtractionRejectsBadRangesBeforeIO(t *testing.T) {
	p := Processor{command: func(context.Context, string, ...string) *exec.Cmd {
		t.Fatal("invalid input launched a process")
		return nil
	}}
	for _, recording := range []*Prepared{nil, {}, {Samples: math.MaxInt64, Duration: time.Second}, {Samples: 24000, Duration: 2 * time.Second}} {
		if clip, err := p.Extract(context.Background(), recording, 0, 1); clip != nil || !errors.Is(err, ErrInvalidMedia) {
			t.Fatalf("invalid recording accepted: %+v, %v", clip, err)
		}
	}
	source := &Prepared{Samples: MaxChunkSamples + 1, Duration: sampleDuration(MaxChunkSamples + 1)}
	for _, r := range [][2]int64{{-1, 1}, {1, 1}, {2, 1}, {0, source.Samples + 1}, {math.MinInt64, math.MaxInt64}, {0, math.MaxInt64}} {
		if clip, err := p.Extract(context.Background(), source, r[0], r[1]); clip != nil || !errors.Is(err, ErrInvalidMedia) {
			t.Fatalf("bad range accepted: %+v, %v", clip, err)
		}
	}
	if clip, err := p.Extract(context.Background(), source, 0, source.Samples); clip != nil || !errors.Is(err, ErrChunkSize) {
		t.Fatalf("oversized chunk accepted: %+v, %v", clip, err)
	}
}

func TestExtractionFailuresCleanUp(t *testing.T) {
	for _, mode := range []string{"extract-failure", "extract-count", "extract-format", "extract-offset", "extract-truncated", "extract-budget", "extract-missing", "extract-symlink", "extract-permission", "wrong-format"} {
		t.Run(mode, func(t *testing.T) {
			p := fakeProcessor(t, mode, "")
			source := extractionSource(t)
			clip, err := p.Extract(context.Background(), source, 0, 48000)
			if clip != nil || err == nil || strings.Contains(err.Error(), "private native metadata") {
				t.Fatalf("partial/unsafe chunk returned: %+v, %v", clip, err)
			}
			if mode == "extract-budget" && !errors.Is(err, ErrChunkSize) {
				t.Fatal("upload budget error lost")
			}
			if mode == "extract-failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal("child status lost")
				}
			}
			assertEmpty(t, p.TempDir)
			if _, err := os.Stat(source.Path); err != nil {
				t.Fatal("source lost on extraction failure")
			}
		})
	}
}

func TestExtractionCancellation(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	p := fakeProcessor(t, "hang-normalize", ready)
	source := extractionSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Extract(ctx, source, 0, 48000); done <- err }()
	testutil.WaitReady(t, ready, done, cancel)
	cancel()
	err := testutil.WaitError(t, done)
	var exit *exec.ExitError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exit) {
		t.Fatalf("cancellation or waited child status lost: %v", err)
	}
	if clip, err := p.Extract(ctx, source, 0, 48000); clip != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancellation lost")
	}
	assertEmpty(t, p.TempDir)
}

func TestExtractionStorageAndDependencyFailures(t *testing.T) {
	source := extractionSource(t)
	p := fakeProcessor(t, "", "")
	p.TempDir = filepath.Join(t.TempDir(), "missing")
	if clip, err := p.Extract(context.Background(), source, 0, 48000); clip != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("storage failure lost")
	}
	p = fakeProcessor(t, "", "")
	command := p.command
	p.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		if tool == "ffmpeg" {
			return exec.CommandContext(ctx, filepath.Join(p.TempDir, "missing-tool"), args...)
		}
		return command(ctx, tool, args...)
	}
	if clip, err := p.Extract(context.Background(), source, 0, 48000); clip != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dependency failure lost")
	}
	assertEmpty(t, p.TempDir)
	if err := os.Truncate(source.Path, 44); err != nil {
		t.Fatal(err)
	}
	p = fakeProcessor(t, "", "")
	if clip, err := p.Extract(context.Background(), source, 0, 1); clip != nil || !errors.Is(err, ErrInvalidMedia) {
		t.Fatal("truncated source accepted")
	}
	assertEmpty(t, p.TempDir)
}
