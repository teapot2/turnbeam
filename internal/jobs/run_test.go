package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/progress"
)

type extractFunc func(context.Context, *media.Prepared, int64, int64) (*media.Clip, error)

func (f extractFunc) Extract(ctx context.Context, r *media.Prepared, start, end int64) (*media.Clip, error) {
	return f(ctx, r, start, end)
}

type transcribeFunc func(context.Context, *media.Clip, string, string) (*meta.Response, error)

func (f transcribeFunc) Transcribe(ctx context.Context, c *media.Clip, key, mode string) (*meta.Response, error) {
	return f(ctx, c, key, mode)
}

func fakePool(t *testing.T) meta.Pool {
	t.Helper()
	dir := t.TempDir()
	return meta.Pool{Extractor: extractFunc(func(ctx context.Context, _ *media.Prepared, start, end int64) (*media.Clip, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, fmt.Sprintf("%d-%d.wav", start, end))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("fake clip %d %d", start, end)), 0600); err != nil {
			return nil, err
		}
		return &media.Clip{Prepared: &media.Prepared{Path: path, Samples: end - start, Duration: time.Duration(end-start) * time.Second / media.SampleRate}, StartSample: start, EndSample: end}, nil
	})}
}

func TestInterruptedWorkersResumeOnlyPendingChunks(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	pool := fakePool(t)
	var diagnostics bytes.Buffer
	reporter := progress.New(&diagnostics, progress.Options{})
	t.Cleanup(func() { reporter.Close() })
	pool.OnProgress = reporter.Transcription
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	pool.Transcriber = transcribeFunc(func(ctx context.Context, c *media.Clip, _, _ string) (*meta.Response, error) {
		if c.StartSample != 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		cancel()
		return response(f.plan[0]).Response, nil
	})
	done := make(chan error, 1)
	go func() {
		rows, err := j.Transcribe(ctx, pool, f.recording, "not-a-real-key")
		if rows != nil {
			t.Error("interrupted job returned a complete transcript")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not start")
	}
	if _, err := j.Transcribe(context.Background(), pool, f.recording, "not-a-real-key"); !errors.Is(err, ErrState) {
		t.Error("concurrent run allowed duplicate uploads")
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation cause lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not join")
	}
	if len(j.m.Completed) != 1 || j.m.Complete {
		t.Fatal("valid completion was lost or whole job looked complete")
	}
	j.Close()
	j = f.open(t, true)
	if _, ok, err := j.Diarization(context.Background()); err != nil || !ok {
		t.Fatal("cached global diarization was lost")
	}
	pool = fakePool(t)
	pool.OnProgress = reporter.Transcription
	pool.Concurrency = 1 // Scheduling changes do not invalidate recovery.
	var requested []int
	pool.Transcriber = transcribeFunc(func(_ context.Context, c *media.Clip, key, mode string) (*meta.Response, error) {
		if key != "not-a-real-key" || mode != f.settings.MetaMode {
			t.Error("inference options changed")
		}
		i := int(c.StartSample / media.SampleRate)
		requested = append(requested, i)
		return response(f.plan[i]).Response, nil
	})
	rows, err := j.Transcribe(context.Background(), pool, f.recording, "not-a-real-key")
	if err != nil || len(rows) != len(f.plan) || !reflect.DeepEqual(requested, []int{1, 2}) {
		t.Fatalf("resume repeated/lost work: requests=%v err=%v", requested, err)
	}
	for i, r := range rows {
		if r.Chunk != f.plan[i] || r.AudioSHA256 == "" || r.Response.SessionID != response(f.plan[i]).Response.SessionID {
			t.Fatal("original plan/checksum/response association changed")
		}
	}
	if !strings.Contains(diagnostics.String(), " done (2/2)\n") || strings.Contains(diagnostics.String(), "not-a-real-key") {
		t.Fatalf("resume progress miscounted cached work or exposed credentials: %q", diagnostics.String())
	}
}

// The child intentionally exits without defers, like an abruptly terminated CLI.
func TestRecoveryAfterProcessExit(t *testing.T) {
	if os.Getenv("TURNBEAM_CHECKPOINT_CHILD") == "1" {
		f := fixture{input: os.Getenv("TURNBEAM_CHILD_INPUT"), dir: os.Getenv("TURNBEAM_CHILD_CACHE"), settings: DefaultSettings(meta.Diarization)}
		j := f.open(t, true)
		if err := j.CheckMedia(context.Background(), &media.Prepared{Path: os.Getenv("TURNBEAM_CHILD_MEDIA"), Samples: 3 * media.SampleRate, Duration: 3 * time.Second}); err != nil {
			t.Fatal(err)
		}
		if err := j.SaveResult(context.Background(), response(j.Plan()[0])); err != nil {
			t.Fatal(err)
		}
		os.Exit(7)
	}
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	j.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestRecoveryAfterProcessExit$")
	cmd.Env = append(os.Environ(), "TURNBEAM_CHECKPOINT_CHILD=1", "TURNBEAM_CHILD_INPUT="+f.input, "TURNBEAM_CHILD_CACHE="+f.dir, "TURNBEAM_CHILD_MEDIA="+f.recording.Path)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("child did not interrupt after commit: %v %s", err, output)
	}
	j = f.open(t, true)
	if len(j.m.Completed) != 1 || j.m.Complete {
		t.Fatal("abrupt exit lost committed work or looked complete")
	}
}
