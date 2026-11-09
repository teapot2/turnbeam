package cli

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

	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"

	"github.com/spf13/cobra"
)

func invoke(ctx context.Context, args ...string) (int, string) {
	code, _, stderr := invokeStreams(ctx, args...)
	return code, stderr
}

func invokeStreams(ctx context.Context, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(ctx, args, "test-version", &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"help"}, {"setup", "--help"}, {"doctor", "--help"},
		{"key"}, {"key", "set", "--help"}, {"key", "status", "--help"},
		{"key", "remove", "--help"}, {"speakers"}, {"speakers", "rename", "--help"},
		{"export", "--help"}, {"version", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := invokeStreams(context.Background(), args...)
			if code != 0 || !strings.Contains(stdout, "Usage:") || stderr != "" {
				t.Fatalf("help not on stdout: code=%d, stdout=%q, stderr=%q", code, stdout, stderr)
			}
		})
	}
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, stdout, stderr := invokeStreams(context.Background(), args...)
		if code != 0 || stdout != "turnbeam test-version\n" || stderr != "" {
			t.Fatalf("%v: code=%d, stdout=%q, stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestDefaults(t *testing.T) {
	code := 2
	root := newRoot("test-version", &code)
	for name, want := range map[string]string{
		"model": "meta", "mode": "diarization", "format": "markdown", "output": "", "timestamps": "start",
		"concurrency": "3", "resume": "false", "fresh": "false", "quiet": "false", "verbose": "false",
	} {
		flag := root.Flags().Lookup(name)
		if flag == nil {
			flag = root.PersistentFlags().Lookup(name)
		}
		if flag == nil || flag.DefValue != want || flag.Value.String() != want {
			t.Errorf("unexpected default for --%s: %v (want %q)", name, flag, want)
		}
	}
	export, _, err := root.Find([]string{"export"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"format": "markdown", "timestamps": "start", "output": ""} {
		if got := export.Flags().Lookup(name).DefValue; got != want {
			t.Errorf("export --%s=%q, want %q", name, got, want)
		}
	}
}

func TestTranscriptionRoutesSeparateWritersAndOptions(t *testing.T) {
	code := 2
	var stdout, stderr bytes.Buffer
	calls := 0
	root := newRootWithTranscriber("test", &code, &stdout, func(ctx context.Context, path string, o pipeline.Options, out, diagnostics io.Writer) error {
		calls++
		if path != "meeting.wav" || o.Mode != "endpointing" || o.Output != "meeting.md" || o.Render.Format != "text" || o.Render.Timestamps != "none" || o.Concurrency != 5 || !o.Resume || !o.Progress.Quiet || !o.Progress.Verbose || o.InstallDir != "/opt/turnbeam" {
			t.Fatal("CLI options not forwarded")
		}
		io.WriteString(out, "requested transcript\n")
		io.WriteString(diagnostics, "safe progress\n")
		return nil
	})
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs([]string{"meeting.wav", "--output=meeting.md", "--mode=endpointing", "--format=text", "--timestamps=none", "--concurrency=5", "--resume", "--quiet", "--verbose", "--install-dir=/opt/turnbeam"})
	if err := root.ExecuteContext(context.Background()); err != nil || calls != 1 || stdout.String() != "requested transcript\n" || stderr.String() != "safe progress\n" {
		t.Fatal("command mixed stdout/stderr", err)
	}
}

func TestModelIsForwardedAndDefaultsFromEnvironment(t *testing.T) {
	run := func(args ...string) (pipeline.Options, error) {
		code := 2
		var got pipeline.Options
		root := newRootWithTranscriber("test", &code, io.Discard, func(_ context.Context, _ string, o pipeline.Options, _, _ io.Writer) error {
			got = o
			return nil
		})
		root.SetArgs(args)
		root.SetErr(io.Discard)
		return got, root.ExecuteContext(context.Background())
	}
	if got, err := run("meeting.wav", "--model", "parakeet", "-o", "meeting.md"); err != nil || got.Model != "parakeet" || got.Resume || got.Output != "meeting.md" {
		t.Fatalf("local model not forwarded: %+v, %v", got, err)
	}
	t.Setenv(modelEnv, "nemotron-3.5")
	if got, err := run("meeting.wav"); err != nil || got.Model != "nemotron-3.5" {
		t.Fatalf("%s default not used: %+v, %v", modelEnv, got, err)
	}
	if got, err := run("meeting.wav", "--model", "meta", "--resume"); err != nil || got.Model != "meta" || !got.Resume {
		t.Fatalf("--model must override %s: %+v, %v", modelEnv, got, err)
	}
	t.Setenv(modelEnv, "whisper")
	if _, err := run("meeting.wav"); err == nil || !strings.Contains(err.Error(), modelEnv+" must be one of") {
		t.Fatalf("invalid %s: %v", modelEnv, err)
	}
}

func TestModelsCommandListsEveryModel(t *testing.T) {
	code := 2
	root := newRoot("test", &code)
	for _, command := range root.Commands() {
		if command.Name() == "models" {
			root.RemoveCommand(command)
		}
	}
	service := &fakeSetup{}
	root.AddCommand(newModelsCmd(&code, func(string, []string, func(setup.Progress)) setupService { return service }, storedKey))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"models"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	for _, want := range []string{"MODEL", "meta (default)", "ready (key in Keychain)", "parakeet", "714 MB", "CC-BY-4.0", "installed", "nemotron-3.5", "not installed; run `turnbeam setup --model nemotron-3.5`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("models output lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(stderr.String(), modelEnv) || strings.Contains(got, modelEnv) {
		t.Fatalf("hint must go to stderr: %q", stderr.String())
	}
}

func TestFlagOverrides(t *testing.T) {
	var got transcribeOptions
	cmd := &cobra.Command{}
	got.addFlags(cmd)
	if err := cmd.ParseFlags([]string{
		"--mode=endpointing", "--format=json", "--output=meeting.json", "--timestamps=none",
		"--concurrency=5", "--resume", "--fresh=false",
	}); err != nil {
		t.Fatal(err)
	}
	if err := got.validate(cmd); err != nil {
		t.Fatal(err)
	}
	want := transcribeOptions{
		outputOptions: outputOptions{format: "json", output: "meeting.json", timestamps: "none"},
		model:         "meta", mode: "endpointing", concurrency: 5, resume: true,
	}
	if got != want {
		t.Fatalf("options=%+v, want %+v", got, want)
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--resume"}, "missing <recording>"},
		{[]string{"one.wav", "two.wav"}, "accepts 1 arg"},
		{[]string{" "}, "input path must not be empty"},
		{[]string{"meeting.wav", "--mode", "invalid"}, "--mode"},
		{[]string{"meeting.wav", "--format", "invalid"}, "--format"},
		{[]string{"meeting.wav", "--timestamps", "invalid"}, "--timestamps"},
		{[]string{"meeting.wav", "--concurrency", "0"}, "--concurrency"},
		{[]string{"meeting.wav", "--concurrency", "-1"}, "--concurrency"},
		{[]string{"meeting.wav", "--concurrency", "no"}, "invalid argument"},
		{[]string{"meeting.wav", "--concurrency", "999999999999999999999"}, "invalid argument"},
		{[]string{"meeting.wav", "--resume", "--fresh"}, "mutually exclusive"},
		{[]string{"meeting.wav", "--model", "whisper"}, "--model must be one of: meta, parakeet, nemotron-3.5"},
		{[]string{"meeting.wav", "--model", "parakeet", "--resume"}, "--resume applies only to --model meta"},
		{[]string{"meeting.wav", "--model", "parakeet", "--fresh"}, "--fresh applies only to --model meta"},
		{[]string{"meeting.wav", "--model", "nemotron-3.5", "--mode", "diarization"}, "--mode applies only to --model meta"},
		{[]string{"meeting.wav", "--model", "parakeet", "--concurrency", "2"}, "--concurrency applies only to --model meta"},
		{[]string{"setup", "--model", "meta"}, "--model meta needs no download"},
		{[]string{"setup", "--model", "whisper"}, "--model must be one of: parakeet, nemotron-3.5"},
		{[]string{"doctor", "--model", "parakeet"}, "unknown flag"},
		{[]string{"models", "extra"}, "unknown command"},
		{[]string{"meeting.wav", "--output="}, "--output must not be empty"},
		{[]string{"meeting.wav", "--output", " "}, "--output must not be empty"},
		{[]string{"meeting.wav", "--unknown"}, "unknown flag"},
		{[]string{"meeting.wav", "--mode"}, "flag needs an argument"},
		{[]string{"meeting.wav", "--quiet=invalid"}, "invalid argument"},
		{[]string{"setup", "extra"}, "unknown command"},
		{[]string{"doctor", "extra"}, "unknown command"},
		{[]string{"doctor", "--mode", "endpointing"}, "unknown flag"},
		{[]string{"setup", "--install-dir="}, "--install-dir"},
		{[]string{"doctor", "--install-dir", " "}, "--install-dir"},
		{[]string{"key", "invalid"}, "unknown command"},
		{[]string{"key", "set", "extra"}, "unknown command"},
		{[]string{"key", "status", "extra"}, "unknown command"},
		{[]string{"key", "remove", "extra"}, "unknown command"},
		{[]string{"speakers", "invalid"}, "unknown command"},
		{[]string{"speakers", "rename"}, "accepts 1 arg"},
		{[]string{"speakers", "rename", "a.json", "b.json"}, "accepts 1 arg"},
		{[]string{"speakers", "rename", "a.json"}, "at least one --speaker"},
		{[]string{"speakers", "rename", "a.json", "--speaker", "Alice"}, "positive INDEX=NAME"},
		{[]string{"speakers", "rename", "a.json", "--speaker", "0=Alice"}, "positive INDEX=NAME"},
		{[]string{"speakers", "rename", "a.json", "--speaker", "-1=Alice"}, "positive INDEX=NAME"},
		{[]string{"speakers", "rename", "a.json", "--speaker", "1= "}, "positive INDEX=NAME"},
		{[]string{"speakers", "rename", "a.json", "--speaker", "1=Alice", "--speaker", "1=Bob"}, "repeats index"},
		{[]string{"export"}, "accepts 1 arg"},
		{[]string{"export", "a.json", "b.json"}, "accepts 1 arg"},
		{[]string{"export", "a.json", "--format", "invalid"}, "--format"},
		{[]string{"export", "a.json", "--timestamps", "invalid"}, "--timestamps"},
		{[]string{"export", "a.json", "--output="}, "--output must not be empty"},
		{[]string{"export", "a.json", "--resume"}, "unknown flag"},
		{[]string{"version", "extra"}, "unknown command"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			code, diagnostic := invoke(context.Background(), test.args...)
			if code != 2 || !strings.Contains(diagnostic, test.want) {
				t.Fatalf("code=%d, stderr=%q; want usage error containing %q", code, diagnostic, test.want)
			}
			if strings.Count(diagnostic, "Error:") != 1 || strings.Contains(diagnostic, "Usage:") {
				t.Fatalf("expected one concise error, got %q", diagnostic)
			}
		})
	}
}

func TestMissingTranscript(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "meeting.json")
	for _, args := range [][]string{
		{"speakers", "rename", missing, "--speaker", "1=Alice", "--speaker", "2=Bob, Jr."},
		{"export", missing},
		{"export", missing, "--format", "json", "--timestamps", "none"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, diagnostic := invoke(context.Background(), args...)
			if code != 1 || !strings.Contains(diagnostic, "inspect transcript") {
				t.Fatalf("code=%d, stderr=%q", code, diagnostic)
			}
		})
	}
}

func TestNoFileSideEffects(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "meeting.md")
	for _, args := range [][]string{
		{"missing.wav", "--output", output},
		{"missing.wav", "--output", output, "--mode", "invalid"},
		{"export", "missing.json", "--output", output},
	} {
		invoke(context.Background(), args...)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("unexpected output artifacts for %v: %v, %v", args, entries, err)
		}
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{{"meeting.wav"}, {"setup"}, {"doctor"}, {"key", "status"}, {"export", "a.json"}, {"speakers", "rename", "a.json", "--speaker", "1=Alice"}} {
		code, diagnostic := invoke(ctx, args...)
		if code != 130 || diagnostic != "Error: canceled\n" {
			t.Fatalf("%v: code=%d, stderr=%q", args, code, diagnostic)
		}
	}
}

func TestExpiredDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	code, diagnostic := invoke(ctx, "doctor")
	if code != 130 || diagnostic != "Error: context deadline exceeded\n" {
		t.Fatalf("code=%d, stderr=%q", code, diagnostic)
	}
}

func TestCommandContextAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := 2
	root := newRoot("test-version", &code)
	root.SetArgs([]string{"meeting.wav"})
	root.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Context() != ctx {
			t.Fatal("command did not receive invocation context")
		}
		cancel()
		return nil
	}
	if err := root.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped cancellation, got %v", err)
	}
}

func TestResolveVersionKeepsLinkedVersion(t *testing.T) {
	if got := resolveVersion("v1.2.3"); got != "v1.2.3" {
		t.Fatalf("linked version replaced: %q", got)
	}
	if got := resolveVersion("dev"); got == "" || got == "(devel)" {
		t.Fatalf("unusable development version %q", got)
	}
}

func TestMistypedCommandsGetSuggestions(t *testing.T) {
	t.Chdir(t.TempDir())
	for typed, want := range map[string]string{"doctr": "doctor", "setp": "setup", "expor": "export", "speakrs": "speakers"} {
		code, diagnostic := invoke(context.Background(), typed)
		if code != exitUsage || !strings.Contains(diagnostic, `unknown command "`+typed+`"; did you mean "`+want+`"?`) {
			t.Fatalf("%s: code %d, %q", typed, code, diagnostic)
		}
	}
	// Unrelated names and real files are still treated as recordings.
	if code, diagnostic := invoke(context.Background(), "meeting.mp4"); code != exitFailed || !strings.Contains(diagnostic, "inspect recording") {
		t.Fatalf("missing recording: code %d, %q", code, diagnostic)
	}
	// Check the argument validator directly: running the command would start a
	// real transcription with the production cache and tools.
	if err := os.WriteFile("doctr", []byte("not media"), 0600); err != nil {
		t.Fatal(err)
	}
	code := exitUsage
	if err := recordingOrCommand(newRoot("test", &code), []string{"doctr"}); err != nil {
		t.Fatal("existing file was treated as a typo:", err)
	}
}

func TestBareCommandShowsHelp(t *testing.T) {
	code, stdout, stderr := invokeStreams(context.Background())
	for _, want := range []string{"Usage:", "Examples:", "Work with transcripts:", "Setup and diagnostics:", "-o, --output"} {
		if code != exitOK || !strings.Contains(stdout, want) || stderr != "" {
			t.Fatalf("bare command missing %q: code %d, stdout %q, stderr %q", want, code, stdout, stderr)
		}
	}
}

func TestShellCompletion(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "fish"} {
		code, stdout, stderr := invokeStreams(context.Background(), "completion", shell)
		if code != 0 || !strings.Contains(stdout, "turnbeam") || stderr != "" {
			t.Fatalf("%s completion script: code %d, stderr %q", shell, code, stderr)
		}
	}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"__complete", "--format", ""}, []string{"markdown", "text", "json"}},
		{[]string{"__complete", "--timestamps", ""}, []string{"start", "range", "none"}},
		{[]string{"__complete", "--mode", ""}, []string{"diarization", "endpointing"}},
		{[]string{"__complete", "doc"}, []string{"doctor"}},
		{[]string{"__complete", "key", ""}, []string{"set", "status", "remove"}},
	} {
		code, stdout, _ := invokeStreams(context.Background(), tc.args...)
		for _, want := range tc.want {
			if code != 0 || !strings.Contains(stdout, want+"\n") && !strings.Contains(stdout, want+"\t") {
				t.Fatalf("%v: missing %q in %q", tc.args, want, stdout)
			}
		}
	}
}

func newRoot(version string, code *int) *cobra.Command {
	return newRootWithTranscriber(version, code, os.Stdout, (pipeline.Runner{}).Run)
}

func TestOutputExtensionSelectsFormat(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want transcript.Format
	}{
		{[]string{"meeting.wav", "-o", "meeting.srt"}, transcript.FormatSRT},
		{[]string{"meeting.wav", "-o", "meeting.vtt"}, transcript.FormatVTT},
		{[]string{"meeting.wav", "-o", "notes.MD"}, transcript.FormatMarkdown},
		{[]string{"meeting.wav", "-o", "meeting.transcript"}, transcript.FormatMarkdown},
		{[]string{"meeting.wav", "-o", "meeting.srt", "-f", "text"}, transcript.FormatText},
	} {
		code := 2
		var got transcript.Format
		root := newRootWithTranscriber("test", &code, io.Discard, func(_ context.Context, _ string, o pipeline.Options, _, _ io.Writer) error {
			got = o.Render.Format
			return nil
		})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(tc.args)
		if err := root.ExecuteContext(context.Background()); err != nil || got != tc.want {
			t.Fatalf("%v: format %q, want %q (%v)", tc.args, got, tc.want, err)
		}
	}
}
