package progress

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/meta"
)

// fakeTerminal returns a terminal reporter with a manual clock and no ticker.
func fakeTerminal(out *bytes.Buffer) (*Reporter, func(time.Duration)) {
	r := New(out, Options{})
	r.tty, r.autoTick = true, false
	clock := time.Unix(0, 0)
	r.now = func() time.Time { return clock }
	return r, func(d time.Duration) { clock = clock.Add(d) }
}

func TestPlainStepLog(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	clock := time.Unix(0, 0)
	r.now = func() time.Time { return clock }
	ctx := context.Background()
	step := func(s Stage, after time.Duration) {
		t.Helper()
		clock = clock.Add(after)
		if err := r.Stage(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	step(Normalize, 0)
	r.AudioPrepared(125 * time.Second)
	step(Diarize, 3*time.Second)
	r.SpeakersFound(3)
	step(Chunk, 48*time.Second)
	r.ChunksPlanned(5)
	step(Transcribe, 0)
	if err := r.TranscribeStarting(5, 2); err != nil {
		t.Fatal(err)
	}
	for _, e := range []meta.Progress{
		{Kind: meta.ProgressQueued, Total: 3},
		{Kind: meta.ProgressWorkers, Total: 3, Active: 1, Chunk: 1},
		{Kind: meta.ProgressSending, Total: 3, Active: 1, Chunk: 1, Attempt: 1},
		{Kind: meta.ProgressRetry, Total: 3, Active: 1, Chunk: 1, Attempt: 2, HTTPStatus: 503, RetryDelay: 8 * time.Second},
		{Kind: meta.ProgressCompleted, Total: 3, Completed: 1, Chunk: 1},
	} {
		if err := r.Transcription(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	step(Assemble, 70*time.Second)
	if err := r.Summary(37, 3, 2, 122*time.Second); err != nil {
		t.Fatal(err)
	}
	want := "==> Preparing audio\n    2m05s of audio · 3s\n" +
		"==> Finding speakers (on device)\n    3 speakers · 48s\n" +
		"==> Splitting at pauses\n    5 chunks\n" +
		"==> Transcribing with Meta Muse\n" +
		"    resuming: 2 of 5 chunks already transcribed\n" +
		"    chunk 3: HTTP 503, retrying in 8s (attempt 2 of 3)\n" +
		"    chunk 3 done (3/5)\n" +
		"    5 chunks, 2 reused · 1m10s\n" +
		"==> Assembling transcript\n" +
		"Done in 2m02s: 37 turns, 3 speakers, 2 turns with an unknown speaker.\n"
	if out.String() != want {
		t.Fatalf("plain log:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestTerminalLiveLineAndLog(t *testing.T) {
	var out bytes.Buffer
	r, advance := fakeTerminal(&out)
	ctx := context.Background()
	if err := r.Stage(ctx, Diarize); err != nil {
		t.Fatal(err)
	}
	advance(12 * time.Second)
	r.drawLive()
	r.SpeakersFound(2)
	if err := r.Stage(ctx, Transcribe); err != nil {
		t.Fatal(err)
	}
	if err := r.TranscribeStarting(4, 0); err != nil {
		t.Fatal(err)
	}
	advance(23 * time.Second)
	if err := r.Transcription(ctx, meta.Progress{Kind: meta.ProgressSending, Total: 4, Active: 2, Chunk: 2, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Transcription(ctx, meta.Progress{Kind: meta.ProgressRetry, Total: 4, Active: 2, Chunk: 2, Attempt: 2, RetryDelay: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"\r\x1b[2K  ⠋ Finding speakers (on device)  0:12",
		"\r\x1b[2K==> Finding speakers (on device)  2 speakers · 12s\n",
		"Transcribing with Meta Muse   ░░░░░░░░░░░░░░░░ 0/4 chunks · chunk 2 sent, waiting for Meta · 2 in progress · 0:23",
		"\r\x1b[2K    chunk 2: connection failed, retrying in 1s (attempt 2 of 3)\n",
		"\r\x1b[2K  ✗ Transcribing with Meta Muse   stopped · 23s\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, " \n") {
		t.Fatalf("trailing spaces on a finished step: %q", got)
	}
	if strings.Contains(got, "\x1b[3") {
		t.Fatalf("color without a color terminal: %q", got)
	}
}

func TestTickerRedrawsWhileWaiting(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{})
	r.tty = true
	if err := r.Stage(context.Background(), Diarize); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if draws := strings.Count(out.String(), "Finding speakers"); draws < 3 {
		t.Fatalf("live line was not redrawn while waiting: %d draws", draws)
	}
	if err := r.Close(); err != nil {
		t.Fatal("second close failed", err)
	}
}

func TestQuietStepsPrintNothing(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, Options{Quiet: true})
	r.tty = true
	ctx := context.Background()
	r.Stage(ctx, Normalize)
	r.AudioPrepared(time.Minute)
	r.Stage(ctx, Transcribe)
	r.TranscribeStarting(2, 1)
	r.Transcription(ctx, meta.Progress{Kind: meta.ProgressRetry, Total: 1, Active: 1, Chunk: 1, Attempt: 2, HTTPStatus: 429, RetryDelay: time.Second})
	r.Summary(1, 1, 0, time.Second)
	r.Close()
	if out.Len() != 0 {
		t.Fatalf("quiet mode printed progress: %q", out.String())
	}
}

func TestDurationFormats(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 45 * time.Second: "45s", 252 * time.Second: "4m12s", 3903 * time.Second: "1h05m03s"} {
		if got := took(d); got != want {
			t.Fatalf("took(%v) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{7 * time.Second: "0:07", 754 * time.Second: "12:34", 3723 * time.Second: "1:02:03"} {
		if got := stopwatch(d); got != want {
			t.Fatalf("stopwatch(%v) = %q, want %q", d, got, want)
		}
	}
}
