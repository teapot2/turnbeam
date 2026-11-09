package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/settings"
)

// fakeInstall lays out an installation holding the named model files.
func fakeInstall(t *testing.T, files ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "install")
	generation := filepath.Join(dir, "install-1")
	if err := os.MkdirAll(generation, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte(`{"schemaVersion":1,"generation":"install-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(generation, name), []byte("gguf"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func saveDefault(t *testing.T, model string) {
	t.Helper()
	path, err := settings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(path, settings.Settings{DefaultModel: model}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultModelOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	check := func(dir, want, wantSource string) {
		t.Helper()
		if got, source, err := defaultModel(ctx, dir); err != nil || got != want || source != wantSource {
			t.Fatalf("default: %q from %q (%v), want %q from %q", got, source, err, want, wantSource)
		}
	}
	empty := filepath.Join(t.TempDir(), "none")
	check(empty, pipeline.MetaModel, fromBuiltIn)
	// Catalog order picks among installed models: nemotron-3.5 precedes nemotron-en.
	installed := fakeInstall(t, "asr-nemotron-en.gguf", "asr-nemotron-3.5.gguf")
	check(installed, "nemotron-3.5", fromInstalled)
	saveDefault(t, "nemotron-en")
	check(installed, "nemotron-en", fromSaved)
	t.Setenv(modelEnv, "parakeet")
	check(installed, "parakeet", fromEnv)
	t.Setenv(modelEnv, "")

	// A broken settings file or installation is an error, never a quiet switch to Meta.
	saveDefault(t, "")
	broken := fakeInstall(t)
	os.Chmod(filepath.Join(broken, "current.json"), 0644)
	if _, _, err := defaultModel(ctx, broken); err == nil {
		t.Fatal("unreadable installation fell back to a default")
	}
	path, _ := settings.Path()
	os.WriteFile(path, []byte("{"), 0600)
	if _, _, err := defaultModel(ctx, installed); err == nil {
		t.Fatal("invalid settings fell back to a default")
	}
}

func runCLI(t *testing.T, args ...string) (pipeline.Options, string, string, error) {
	t.Helper()
	code := 2
	var got pipeline.Options
	var stdout, stderr bytes.Buffer
	root := newRootWithTranscriber("test", &code, &stdout, func(_ context.Context, _ string, o pipeline.Options, _, _ io.Writer) error {
		got = o
		return nil
	})
	addCommands(root, "test", &code, &stdout, nil)
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(context.Background())
	return got, stdout.String(), stderr.String(), err
}

func TestTranscriptionUsesTheDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := fakeInstall(t, "asr-parakeet.gguf")
	if got, _, _, err := runCLI(t, "meeting.wav", "--install-dir", dir); err != nil || got.Model != "parakeet" {
		t.Fatalf("installed default: %+v, %v", got, err)
	}
	if got, _, _, err := runCLI(t, "meeting.wav", "--install-dir", dir, "--model", "meta", "--resume"); err != nil || got.Model != "meta" {
		t.Fatalf("--model must override the default: %+v, %v", got, err)
	}
	if _, _, _, err := runCLI(t, "meeting.wav", "--install-dir", dir, "--resume"); err == nil || !strings.Contains(err.Error(), "the default model is parakeet, "+fromInstalled) {
		t.Fatalf("Meta-only flag with a local default: %v", err)
	}
	saveDefault(t, "whisper")
	if _, _, _, err := runCLI(t, "meeting.wav", "--install-dir", dir); err == nil || !strings.Contains(err.Error(), "turnbeam models default NAME") {
		t.Fatalf("unavailable saved default: %v", err)
	}
}

func TestModelsDefaultCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := fakeInstall(t, "asr-parakeet.gguf")
	_, stdout, stderr, err := runCLI(t, "models", "default", "--install-dir", dir)
	if err != nil || stdout != "parakeet\n" || !strings.Contains(stderr, fromInstalled) {
		t.Fatalf("show: %q %q %v", stdout, stderr, err)
	}
	_, stdout, stderr, err = runCLI(t, "models", "default", "nemotron-en", "--install-dir", dir)
	if err != nil || stdout != "" || !strings.Contains(stderr, "Saved nemotron-en") || !strings.Contains(stderr, "turnbeam setup --model nemotron-en") {
		t.Fatalf("save: %q %q %v", stdout, stderr, err)
	}
	if name, source, _ := defaultModel(context.Background(), dir); name != "nemotron-en" || source != fromSaved {
		t.Fatalf("saved default not used: %s %s", name, source)
	}
	if _, stdout, _, err = runCLI(t, "models", "default", "--install-dir", dir); err != nil || stdout != "nemotron-en\n" {
		t.Fatalf("show saved: %q %v", stdout, err)
	}
	if _, _, stderr, err = runCLI(t, "models", "default", "meta", "--install-dir", dir); err != nil || strings.Contains(stderr, "not installed") {
		t.Fatalf("save meta: %q %v", stderr, err)
	}
	t.Setenv(modelEnv, "parakeet")
	if _, _, stderr, err = runCLI(t, "models", "default", "meta"); err != nil || !strings.Contains(stderr, modelEnv+"=parakeet is set") {
		t.Fatalf("env precedence not noted: %q %v", stderr, err)
	}
	t.Setenv(modelEnv, "")
	if _, _, stderr, err = runCLI(t, "models", "default", "auto", "--install-dir", dir); err != nil || !strings.Contains(stderr, "now use parakeet ("+fromInstalled+")") {
		t.Fatalf("auto: %q %v", stderr, err)
	}

	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(real, "mine.gguf")
	os.WriteFile(model, []byte("gguf"), 0600)
	t.Chdir(real)
	if _, _, _, err = runCLI(t, "models", "default", "mine.gguf"); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := defaultModel(context.Background(), dir); name != model {
		t.Fatalf("model file not saved by absolute path: %s", name)
	}
	for _, bad := range []string{"whisper", "missing.gguf"} {
		if _, _, _, err = runCLI(t, "models", "default", bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if name, _, _ := defaultModel(context.Background(), dir); name != model {
		t.Fatalf("a rejected default changed the saved one: %s", name)
	}
}
