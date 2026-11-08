package progress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/meta"
)

func TestModelWarningSurvivesQuiet(t *testing.T) {
	var stderr bytes.Buffer
	r := New(&stderr, Options{Quiet: true})
	if err := r.WarnSpeakerLimit(context.Background()); err != nil || !strings.Contains(stderr.String(), "Warning:") {
		t.Fatal("quiet hid model limitation", err)
	}
}

func TestMalformedEventsCannotWriteArbitraryStrings(t *testing.T) {
	var stderr bytes.Buffer
	r := New(&stderr, Options{Verbose: true})
	if err := r.Stage(context.Background(), Stage(255)); !errors.Is(err, ErrEvent) {
		t.Fatal("unknown stage accepted")
	}
	for _, e := range []meta.Progress{{Kind: 255}, {Total: -1}, {Total: 1, Active: 2}, {Total: 1, Completed: 2}, {Total: 1, Chunk: 2}, {Attempt: 4}, {HTTPStatus: 999}, {RetryDelay: -1}, {Kind: meta.ProgressFinished, Total: 1}, {Kind: meta.ProgressStopped, Total: 1, Active: 1}} {
		if err := r.Transcription(context.Background(), e); !errors.Is(err, ErrEvent) {
			t.Fatal("malformed event accepted", e)
		}
	}
	if stderr.Len() != 0 {
		t.Fatal("invalid progress reached stderr")
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestWriterErrorsCancellationAndConcurrentClose(t *testing.T) {
	short := New(shortWriter{}, Options{})
	if err := short.Stage(context.Background(), Probe); err != nil {
		t.Fatal("short stderr write failed the operation", err)
	}
	if err := short.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short stderr write was not reported at close", err)
	}
	want := errors.New("broken stderr")
	r := New(failingWriter{want}, Options{})
	if err := r.Stage(context.Background(), Probe); err != nil {
		t.Fatal("broken stderr failed the operation", err)
	}
	if err := r.Transcription(context.Background(), meta.Progress{Kind: meta.ProgressWorkers, Total: 2, Active: 1}); err != nil {
		t.Fatal("later progress after a broken stderr failed the operation", err)
	}
	if err := r.Close(); !errors.Is(err, want) {
		t.Fatal("close lost prior writer error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	r = New(&stderr, Options{})
	if err := r.Stage(ctx, Probe); !errors.Is(err, context.Canceled) || stderr.Len() != 0 {
		t.Fatal("canceled stage wrote routine output")
	}
	// Final events are still accepted after cancellation; an interrupted step is
	// marked as stopped by Close (covered by TestTerminalLiveLineAndLog).
	if err := r.Transcription(ctx, meta.Progress{Kind: meta.ProgressStopped}); err != nil {
		t.Fatal("cancellation rejected the final pool event", err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { r.Stage(context.Background(), Assemble) })
	}
	wg.Go(func() { r.Close() })
	wg.Wait()
	if err := r.Stage(context.Background(), Probe); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed reporter accepted more progress")
	}
}

func TestSummaryCountsOnly(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	if err := r.Summary(37, 3, 2, 4*time.Minute+12400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := r.Summary(1, 1, 0, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := "Done in 4m12s: 37 turns, 3 speakers, 2 turns with an unknown speaker.\nDone in 0s: 1 turn, 1 speaker.\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
	out.Reset()
	if err := New(&out, Options{Quiet: true}).Summary(1, 1, 0, time.Second); err != nil || out.Len() != 0 {
		t.Fatal("quiet summary printed", err)
	}
	if err := New(&out, Options{}).Summary(1, 1, 2, time.Second); !errors.Is(err, ErrEvent) {
		t.Fatal("inconsistent summary accepted")
	}
}
