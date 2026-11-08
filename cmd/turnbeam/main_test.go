package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "turnbeam")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-ldflags=-X main.version=test-build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, test := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--help"}, 0, "turnbeam <recording>"},
		{[]string{"version"}, 0, "turnbeam test-build\n"},
		{[]string{"--version"}, 0, "turnbeam test-build\n"},
		{[]string{"recording.wav"}, 1, "inspect recording"},
		{[]string{"doctor", "--install-dir", filepath.Join(t.TempDir(), "missing-install")}, 1, "Error:"},
		{[]string{"recording.wav", "--concurrency=0"}, 2, "--concurrency"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			cmd := exec.CommandContext(ctx, binary, test.args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			// Requested output (help, version) goes to stdout; errors to stderr.
			want, other := stderr, stdout
			if test.code == 0 {
				want, other = stdout, stderr
			}
			if code != test.code || !strings.Contains(want.String(), test.want) || other.Len() != 0 {
				t.Fatalf("code=%d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
	t.Run("cached transcription outputs", func(t *testing.T) { testCachedOutputs(t, ctx, binary) })
	t.Run("offline rename and export", func(t *testing.T) { testOfflineCommands(t, ctx, binary) })
	t.Run("signal cancellation", func(t *testing.T) { testSignals(t, ctx, binary) })
}

func testOfflineCommands(t *testing.T, ctx context.Context, binary string) {
	dir, home := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "meeting.transcript.json")
	doc := &transcript.Transcript{SchemaVersion: 1,
		Recording:    transcript.Recording{Name: "missing.wav", DurationMS: 1000, SHA256: strings.Repeat("a", 64)},
		Pipeline:     transcript.Pipeline{MetaModel: "test", MetaMode: "DIARIZATION", DiarizationModel: "test", DiarizationRevision: "test", RuntimeVersion: "test"},
		Speakers:     []transcript.Speaker{{ID: "global-one", Index: 1}, {ID: "global-two", Index: 2}},
		Turns:        []transcript.Turn{{ID: "turn", StartMS: 123, EndMS: 987, SpeakerID: new("global-one"), Text: "Hello.", Attribution: transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}, Source: transcript.Source{ChunkID: "chunk"}}},
		SourceChunks: []transcript.SourceChunk{{ID: "chunk", EndMS: 1000, MetaSessionID: "session", RawTurns: []transcript.RawTurn{{StartMS: 123, EndMS: 987, Transcript: "Hello."}}}},
	}
	data, err := transcript.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, string, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + filepath.Join(home, "no-tools"), "META_API_KEY= invalid key must not be read "}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := invoke("speakers", "rename", path, "--speaker", "2=Bob, Jr.", "--speaker", "1=Alice=A")
	if err != nil || stdout != "" || stderr != "Updated 2 speaker names in the JSON sidecar. Run `turnbeam export` to write a transcript with them.\n" {
		t.Fatal("offline rename required external resources", err, stdout, stderr)
	}
	stdout, stderr, err = invoke("export", path, "--format=text", "--timestamps=range")
	if err != nil || stderr != "" || stdout != "[00:00:00.123 - 00:00:00.987] Alice=A: Hello.\n" {
		t.Fatal("offline stdout export failed", err, stdout, stderr)
	}
	output := filepath.Join(dir, "meeting.md")
	stdout, stderr, err = invoke("export", path, "--output", output, "--timestamps=none")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatal("offline paired export failed", err, stdout, stderr)
	}
	data, _ = os.ReadFile(path)
	got, err := transcript.Decode(data)
	if err != nil || got.Turns[0].StartMS != 123 || got.Turns[0].EndMS != 987 || *got.Speakers[0].Name != "Alice=A" || *got.Speakers[1].Name != "Bob, Jr." || got.Speakers[0].ID != "global-one" {
		t.Fatal("offline command lost canonical data", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("offline commands created a cache or installed resources", err)
	}
	stdout, stderr, err = invoke("export", path, "--output", output)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout != "" || !strings.Contains(stderr, "already exists") {
		t.Fatal("offline conflict was not reported correctly", err)
	}
}

func testCachedOutputs(t *testing.T, ctx context.Context, binary string) {
	dir, home := t.TempDir(), t.TempDir()
	cache := filepath.Join(home, "cache", "turnbeam", "jobs")
	if runtime.GOOS == "darwin" {
		cache = filepath.Join(home, "Library", "Caches", "turnbeam", "jobs")
	} else if runtime.GOOS != "linux" {
		t.Skip("cached CLI fixture uses supported macOS/Linux cache paths")
	}
	input, normal, raw := filepath.Join(dir, "source.wav"), filepath.Join(dir, "normalized.wav"), filepath.Join(dir, "native.rttm")
	for path, data := range map[string]string{input: "original fixture", normal: "normalized fixture", raw: "SPEAKER recording 1 0.000 1.000 <NA> <NA> A <NA> <NA>\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	j, err := jobs.Open(ctx, input, jobs.DefaultSettings(meta.Diarization), jobs.Options{Dir: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.CheckMedia(ctx, &media.Prepared{Path: normal, Samples: media.SampleRate, Duration: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err = j.SaveDiarization(ctx, &diarization.Result{RTTMPath: raw, RuntimeVersion: setup.RuntimeVersion, ModelRevision: setup.ModelRevision}); err != nil {
		t.Fatal(err)
	}
	chunk := chunking.Chunk{ID: "chunk_0001", EndSample: media.SampleRate, Boundary: chunking.End}
	if err = j.SavePlan(ctx, []chunking.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	hash, err := media.FileSHA256(ctx, normal)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.SaveResult(ctx, meta.Result{Chunk: chunk, AudioSHA256: hash, Attempts: 1, Response: &meta.Response{SessionID: "fixture", Transcript: "Hello.", AudioDurationMS: 1000, Turns: []transcript.RawTurn{{TurnID: 0, StartMS: 200, EndMS: 800, Transcript: "Hello."}}}}); err != nil {
		t.Fatal(err)
	}
	if err = j.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, string, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{input, "--resume"}, args...)...)
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if name != "HOME" && name != "XDG_CACHE_HOME" && name != "META_API_KEY" && name != "PATH" {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+home, "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "PATH="+filepath.Join(home, "no-tools"), "META_API_KEY= invalid key must not be read ")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := invoke()
	if err != nil || stdout != "[00:00:00.200] **Speaker 1:** Hello.\n" || strings.Contains(stderr, "Preparing audio") || strings.Contains(stderr, "Transcribing with") {
		t.Fatal("cached CLI required tools/credentials or contaminated stdout", err, stdout, stderr)
	}
	outDir := t.TempDir()
	output := filepath.Join(outDir, "meeting.md")
	stdout, stderr, err = invoke("--output", output, "--timestamps=none")
	if err != nil || stdout != "" {
		t.Fatal("file invocation failed", err, stdout, stderr)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "**Speaker 1:** Hello.\n" {
		t.Fatal("chosen output missing", err)
	}
	data, err = os.ReadFile(filepath.Join(outDir, "meeting.transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transcript.Decode(data)
	if err != nil || canonical.Turns[0].StartMS != 200 || canonical.Turns[0].EndMS != 800 {
		t.Fatal("sidecar altered canonical timing", err)
	}
	stdout, stderr, err = invoke("--output", output)
	if err == nil || stdout != "" || !strings.Contains(stderr, "already exists") {
		t.Fatal("conflicting files overwritten")
	}
	single := filepath.Join(outDir, "single.transcript.json")
	stdout, stderr, err = invoke("--format=json", "--output", single, "--timestamps=none")
	if err != nil || stdout != "" {
		t.Fatal("single canonical JSON output failed", err, stderr)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 3 {
		t.Fatal("temporary or redundant JSON output survived", err)
	}
}
