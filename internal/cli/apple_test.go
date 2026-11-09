package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
)

type fakeAppleService struct {
	built            bool
	builds, installs int
	language         string
	status           string
}

func (f *fakeAppleService) Built() bool { return f.built }
func (f *fakeAppleService) Build(context.Context) error {
	f.builds++
	f.built = true
	return nil
}
func (f *fakeAppleService) Status(_ context.Context, language string) (apple.State, error) {
	return apple.State{Locale: "en-US", Status: f.status, OS: "test"}, nil
}
func (f *fakeAppleService) Install(_ context.Context, language string) (apple.State, error) {
	f.installs++
	f.language = language
	return apple.State{Locale: "es-MX", Status: f.status, OS: "test"}, nil
}

func useFakeApple(t *testing.T, fake *fakeAppleService) {
	t.Helper()
	previous := appleHelper
	appleHelper = func(string) appleService { return fake }
	t.Cleanup(func() { appleHelper = previous })
}

func runSetup(t *testing.T, service *fakeSetup, args ...string) (string, error) {
	t.Helper()
	code := 2
	root := newRoot("test", &code)
	for _, command := range root.Commands() {
		if command.Name() == "setup" || command.Name() == "doctor" || command.Name() == "models" {
			root.RemoveCommand(command)
		}
	}
	var models []string
	addSetupCommands(root, &code, func(_ string, m []string, _ func(setup.Progress)) setupService {
		models = m
		return service
	}, storedKey)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(io.Discard)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	if len(models) != 0 {
		t.Fatalf("apple was passed to the pinned installer: %v", models)
	}
	return stderr.String(), err
}

func TestSetupApple(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeAppleService{status: "installed"}
	useFakeApple(t, fake)
	service := &fakeSetup{}
	stderr, err := runSetup(t, service, "setup", "--model", "apple", "--language", "es-MX")
	if err != nil || service.installs != 1 || fake.builds != 1 || fake.installs != 1 || fake.language != "es-MX" ||
		!strings.Contains(stderr, "es-MX ready") || !strings.Contains(stderr, "--model apple") {
		t.Fatalf("setup: %q, %v, %+v", stderr, err, fake)
	}
	fake.status = "downloading"
	if _, err := runSetup(t, &fakeSetup{}, "setup", "--model", "apple"); err == nil || !strings.Contains(err.Error(), "downloading") {
		t.Fatalf("unfinished download looked installed: %v", err)
	}
	for _, args := range [][]string{{"setup", "--language", "es-MX"}, {"setup", "--model", "parakeet", "--language", "es-MX"}, {"setup", "--model", "apple", "--language", "es MX"}} {
		if _, err := runSetup(t, &fakeSetup{}, args...); err == nil || !strings.Contains(err.Error(), "--language") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestAppleModelOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeAppleService{built: true, status: "installed"}
	useFakeApple(t, fake)
	empty := t.TempDir()
	// With no pinned model installed, a built helper is the automatic default.
	if name, source, err := defaultModel(context.Background(), empty); err != nil || name != apple.Model || source != fromInstalled {
		t.Fatalf("default: %s %s %v", name, source, err)
	}
	got, _, _, err := runCLI(t, "meeting.wav", "--install-dir", empty, "--language", "es-MX")
	if err != nil || got.Model != apple.Model || got.Language != "es-MX" {
		t.Fatalf("language not forwarded: %+v, %v", got, err)
	}
	if _, _, _, err := runCLI(t, "meeting.wav", "--model", "apple", "--resume"); err == nil || !strings.Contains(err.Error(), "applies only to --model meta") {
		t.Fatalf("resume with apple: %v", err)
	}
	if _, _, _, err := runCLI(t, "meeting.wav", "--model", "parakeet", "--language", "es-MX"); err == nil || !strings.Contains(err.Error(), "--language applies only to --model apple") {
		t.Fatalf("language with parakeet: %v", err)
	}
	_, stdout, _, err := runCLI(t, "models")
	if err != nil || !strings.Contains(stdout, "apple") || !strings.Contains(stdout, "installed (en-US)") {
		t.Fatalf("models: %q, %v", stdout, err)
	}
	fake.built = false
	if _, stdout, _, _ = runCLI(t, "models"); !strings.Contains(stdout, "not set up; run `turnbeam setup --model apple`") {
		t.Fatalf("unbuilt helper: %q", stdout)
	}
	if !strings.Contains(strings.Join(modelNames(), ","), pipeline.MetaModel+",parakeet") || modelNames()[len(modelNames())-1] != apple.Model {
		t.Fatalf("model names: %v", modelNames())
	}
}
