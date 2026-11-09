package asr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/setup"
)

func prepared(t *testing.T) *media.Prepared {
	t.Helper()
	path := filepath.Join(t.TempDir(), "normalized audio.wav")
	samples := int64(10 * media.SampleRate)
	if err := os.WriteFile(path, make([]byte, 44+samples*2), 0600); err != nil {
		t.Fatal(err)
	}
	return &media.Prepared{Path: path, Samples: samples, Duration: 10 * time.Second}
}

func fakeAdapter(t *testing.T, mode string) Adapter {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Adapter{
		TempDir: t.TempDir(),
		ready: func(ctx context.Context) (setup.Installation, error) {
			return setup.Installation{Runtime: "verified-native", Model: "verified-model", ASRModels: map[string]string{"parakeet": "verified-asr", "nemotron-3.5": "verified-asr"}}, ctx.Err()
		},
		command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestNemoProcess$", "--"}, args...)...)
			cmd.Env = append(os.Environ(), "TURNBEAM_ASR_HELPER=1", "TURNBEAM_ASR_MODE="+mode, "GORACE=atexit_sleep_ms=0")
			return cmd
		},
	}
}

// Words arrive out of order, with punctuation, an empty token and a final
// word whose end overruns the 10 s recording by native frame padding.
const nativeJSON = `{"file":"x.wav","text":"Um Hello, there. Bye","confidence":1,"duration":10,"languages":["en"],"words":[
{"word":"there.","start":1.2,"end":1.6,"confidence":1},
{"word":"Hello,","start":0.4,"end":0.96,"confidence":0.9,"speaker":2},
{"word":"Um","start":0.1,"end":0.4,"confidence":1},
{"word":" ","start":2,"end":2,"confidence":1},
{"word":"Bye","start":9.6,"end":10.5,"confidence":1}]}`

func TestNemoProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_ASR_HELPER") != "1" {
		return
	}
	args := os.Args
	for args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	output := ""
	for i := range args {
		if args[i] == "--output" && i+1 < len(args) {
			output = args[i+1]
		}
	}
	// The pinned flags are part of the contract: word timings in JSON on Metal.
	for _, want := range []string{"transcribe", "--word-times", "--quiet"} {
		if !slices.Contains(args, want) {
			os.Exit(3)
		}
	}
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "verified-asr" {
		os.Exit(3)
	}
	data := nativeJSON
	switch mode := os.Getenv("TURNBEAM_ASR_MODE"); mode {
	case "exit":
		os.Exit(1)
	case "nothing":
		os.Exit(0)
	case "symlink":
		if err := os.Symlink("/tmp/not-a-transcript", output); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "oversized":
		f, err := os.Create(output)
		if err != nil || f.Truncate(maxOutputBytes+1) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "hang":
		time.Sleep(time.Hour)
	case "silent":
		data = `{"text":"","words":[]}`
	default:
		if mode != "" {
			data = mode
		}
	}
	if err := os.WriteFile(output, []byte(data), 0600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func model(t *testing.T, name string) setup.LocalModel {
	t.Helper()
	m, ok := setup.FindLocalModel(name)
	if !ok {
		t.Fatalf("no model %s", name)
	}
	return m
}

func TestTranscribeOrdersAndClipsWords(t *testing.T) {
	a := fakeAdapter(t, "")
	words, err := a.Transcribe(context.Background(), prepared(t), model(t, "parakeet"))
	if err != nil {
		t.Fatal(err)
	}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	want := []Word{
		{"Um", ms(100), ms(400)},
		{"Hello,", ms(400), ms(960)},
		{"there.", ms(1200), ms(1600)},
		{"Bye", ms(9600), 10 * time.Second},
	}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words:\n got %+v\nwant %+v", words, want)
	}
	assertEmpty(t, a.TempDir)
}

// Nemotron 3.5 reports words late, so its times move 300 ms earlier; near the
// start they move only as far as zero, keeping the word's length.
func TestTranscribeCorrectsModelLatency(t *testing.T) {
	m := model(t, "nemotron-3.5")
	if m.Latency != 300*time.Millisecond {
		t.Fatalf("expectations assume a 300 ms latency, not %v", m.Latency)
	}
	words, err := fakeAdapter(t, "").Transcribe(context.Background(), prepared(t), m)
	if err != nil {
		t.Fatal(err)
	}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	want := []Word{{"Um", 0, ms(300)}, {"Hello,", ms(100), ms(660)}, {"there.", ms(900), ms(1300)}, {"Bye", ms(9300), 10 * time.Second}}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words:\n got %+v\nwant %+v", words, want)
	}
}

// A late model reports its last word past the end of the audio; after the
// latency correction it lies within the recording and is kept.
func TestTranscribeKeepsLateFinalWord(t *testing.T) {
	a := fakeAdapter(t, `{"text":"Bye","words":[{"word":"Bye","start":10.1,"end":10.3}]}`)
	words, err := a.Transcribe(context.Background(), prepared(t), model(t, "nemotron-3.5"))
	want := []Word{{"Bye", 9800 * time.Millisecond, 10 * time.Second}}
	if err != nil || !reflect.DeepEqual(words, want) {
		t.Fatalf("late final word: %+v, %v", words, err)
	}
	if _, err := a.Transcribe(context.Background(), prepared(t), model(t, "parakeet")); !errors.Is(err, ErrOutput) {
		t.Fatalf("without a latency correction the word is past the end: %v", err)
	}
}

func TestTranscribeSilence(t *testing.T) {
	words, err := fakeAdapter(t, "silent").Transcribe(context.Background(), prepared(t), model(t, "parakeet"))
	if err != nil || len(words) != 0 {
		t.Fatalf("silence: %+v, %v", words, err)
	}
}

func TestTranscribeRejectsBadOutput(t *testing.T) {
	for name, mode := range map[string]string{
		"missing output":     "nothing",
		"symlink":            "symlink",
		"oversized":          "oversized",
		"malformed":          `{"text":`,
		"no words":           `{"text":"hello"}`,
		"no text":            `{"words":[]}`,
		"text without words": `{"text":"hello","words":[]}`,
		"missing word":       `{"text":"a","words":[{"start":1,"end":2}]}`,
		"missing end":        `{"text":"a","words":[{"word":"a","start":1}]}`,
		"negative start":     `{"text":"a","words":[{"word":"a","start":-0.1,"end":2}]}`,
		"inverted":           `{"text":"a","words":[{"word":"a","start":2,"end":1}]}`,
		"past recording":     `{"text":"a","words":[{"word":"a","start":9,"end":11.01}]}`,
		"start past end":     `{"text":"a","words":[{"word":"a","start":10.5,"end":10.6}]}`,
		"huge timestamp":     `{"text":"a","words":[{"word":"a","start":1e300,"end":1e300}]}`,
		"string timestamp":   `{"text":"a","words":[{"word":"a","start":"1","end":2}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := fakeAdapter(t, mode)
			words, err := a.Transcribe(context.Background(), prepared(t), model(t, "parakeet"))
			if words != nil || !errors.Is(err, ErrOutput) {
				t.Fatalf("bad output accepted: %+v, %v", words, err)
			}
			assertEmpty(t, a.TempDir)
		})
	}
}

func TestTranscribeFailures(t *testing.T) {
	a := fakeAdapter(t, "exit")
	if words, err := a.Transcribe(context.Background(), prepared(t), model(t, "parakeet")); words != nil || err == nil || errors.Is(err, ErrOutput) {
		t.Fatalf("native failure: %+v, %v", words, err)
	}
	assertEmpty(t, a.TempDir)

	notReady := fakeAdapter(t, "")
	notReady.ready = func(context.Context) (setup.Installation, error) {
		return setup.Installation{}, &setup.ModelError{Name: "parakeet", Err: os.ErrNotExist}
	}
	if _, err := notReady.Transcribe(context.Background(), prepared(t), model(t, "parakeet")); !errors.Is(err, setup.ErrLocalModel) {
		t.Fatalf("missing model: %v", err)
	}
	noModel := fakeAdapter(t, "")
	noModel.ready = func(context.Context) (setup.Installation, error) {
		return setup.Installation{Runtime: "verified-native"}, nil
	}
	if _, err := noModel.Transcribe(context.Background(), prepared(t), model(t, "parakeet")); !errors.Is(err, setup.ErrLocalModel) {
		t.Fatalf("installation without a local model: %v", err)
	}
	if _, err := a.Transcribe(context.Background(), &media.Prepared{}, model(t, "parakeet")); err == nil {
		t.Fatal("incomplete recording accepted")
	}
}

func TestTranscribeCancellation(t *testing.T) {
	a := fakeAdapter(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	words, err := a.Transcribe(ctx, prepared(t), model(t, "parakeet"))
	if words != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled transcription returned %+v, %v", words, err)
	}
	assertEmpty(t, a.TempDir)
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left behind: %s, %v", strings.Join(names, ", "), err)
	}
}
