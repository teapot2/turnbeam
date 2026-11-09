package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/setup"
)

type fakeSetup struct {
	ctx                   context.Context
	installs, doctors     int
	installErr, doctorErr error
	checks                []setup.Check
}

type failedDiagnostics struct{}

func storedKey(context.Context) (credentials.Source, bool, error) {
	return credentials.KeychainSource, true, nil
}

func (failedDiagnostics) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSetupDiagnosticFailuresDoNotLookSuccessful(t *testing.T) {
	for _, name := range []string{"setup", "doctor"} {
		code := 2
		root := newRoot("test", &code)
		for _, command := range root.Commands() {
			if command.Name() == "setup" || command.Name() == "doctor" {
				root.RemoveCommand(command)
			}
		}
		service := &fakeSetup{checks: []setup.Check{{Name: "platform", OK: true, Message: "supported"}}}
		addSetupCommands(root, &code, func(string, []string, func(setup.Progress)) setupService { return service }, storedKey)
		root.SetErr(failedDiagnostics{})
		root.SetArgs([]string{name})
		if err := root.ExecuteContext(context.Background()); !errors.Is(err, io.ErrClosedPipe) || code != 1 {
			t.Fatal("diagnostic failure looked successful", err)
		}
		if name == "setup" && service.installs != 0 {
			t.Fatal("installation started despite failed initial diagnostics")
		}
	}
}

func (f *fakeSetup) Install(ctx context.Context) (setup.Installation, error) {
	f.ctx, f.installs = ctx, f.installs+1
	return setup.Installation{Directory: "fixture-install"}, f.installErr
}

func (f *fakeSetup) Doctor(ctx context.Context) ([]setup.Check, error) {
	f.ctx, f.doctors = ctx, f.doctors+1
	return f.checks, f.doctorErr
}

func (f *fakeSetup) LocalStatus(ctx context.Context) ([]setup.ModelStatus, error) {
	f.ctx = ctx
	var statuses []setup.ModelStatus
	for i, model := range setup.LocalModels() {
		statuses = append(statuses, setup.ModelStatus{Model: model, Installed: i == 0})
	}
	return statuses, f.doctorErr
}

func TestSetupAndDoctorCommands(t *testing.T) {
	for _, test := range []struct {
		args []string
		fail bool
	}{
		{[]string{"setup"}, false},
		{[]string{"setup", "--install-dir", "custom-install", "--verbose"}, false},
		{[]string{"setup", "--quiet"}, false},
		{[]string{"setup", "--quiet"}, true},
		{[]string{"setup", "--model", "parakeet"}, false},
		{[]string{"setup", "--model", "parakeet,nemotron-3.5"}, false},
		{[]string{"doctor"}, false},
		{[]string{"doctor", "--install-dir", "custom-install", "--quiet"}, true},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			service := &fakeSetup{checks: []setup.Check{{Name: "platform", OK: true, Message: "supported"}}}
			if test.fail {
				service.installErr = errors.New("download unavailable")
				service.doctorErr = setup.ErrDoctorFailed
				service.checks = append(service.checks, setup.Check{Name: "model", Message: "run setup"})
			}
			code := 2
			root := newRoot("test-version", &code)
			for _, command := range root.Commands() {
				if command.Name() == "setup" || command.Name() == "doctor" {
					root.RemoveCommand(command)
				}
			}
			selectedDir, selectedModels := "", []string(nil)
			addSetupCommands(root, &code, func(dir string, models []string, _ func(setup.Progress)) setupService {
				selectedDir, selectedModels = dir, models
				return service
			}, storedKey)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(test.args)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := root.ExecuteContext(ctx)
			if (err != nil) != test.fail || code != 1 || service.ctx != ctx || stdout.Len() != 0 {
				t.Fatalf("wrong execution/output: code=%d stdout=%q stderr=%q err=%v", code, stdout.String(), stderr.String(), err)
			}
			if i := slices.Index(test.args, "--model"); i >= 0 && strings.Join(selectedModels, ",") != test.args[i+1] {
				t.Fatalf("--model forwarded as %v", selectedModels)
			}
			if test.args[0] == "setup" && !test.fail && !strings.Contains(strings.Join(test.args, " "), "--quiet") && strings.Contains(stderr.String(), "--model parakeet`") != (len(selectedModels) > 0) {
				t.Fatalf("setup message ignores --model: %q", stderr.String())
			}
			if strings.Contains(strings.Join(test.args, " "), "--install-dir") && selectedDir != "custom-install" {
				t.Fatalf("install directory not forwarded: %q", selectedDir)
			}
			if test.args[0] == "setup" && (service.installs != 1 || service.doctors != 0) {
				t.Fatal("wrong setup service method")
			}
			if test.args[0] == "doctor" && (service.doctors != 1 || service.installs != 0) {
				t.Fatal("doctor attempted installation")
			}
			if strings.Contains(strings.Join(test.args, " "), "--quiet") {
				if strings.Contains(stderr.String(), "OK ") || strings.Contains(stderr.String(), "Installing the speaker runtime") {
					t.Fatal("quiet mode printed progress")
				}
				if test.fail && test.args[0] == "doctor" && !strings.Contains(stderr.String(), "FAIL model") {
					t.Fatal("quiet mode hid a failed check")
				}
			}
		})
	}
}

type progressSetup struct {
	fakeSetup
	report func(setup.Progress)
}

func (p *progressSetup) Install(ctx context.Context) (setup.Installation, error) {
	model := setup.Progress{Step: setup.StepDownload, Title: "Nemotron 3 diarization model", Source: "https://huggingface.co/nvidia/model.gguf", Total: 107_012_128}
	p.report(model)
	model.Done = model.Total
	p.report(model)
	p.report(setup.Progress{Step: setup.StepVerify})
	return p.fakeSetup.Install(ctx)
}

func TestSetupShowsProgressUnlessQuiet(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		code := 2
		root := newRoot("test", &code)
		for _, command := range root.Commands() {
			if command.Name() == "setup" || command.Name() == "doctor" {
				root.RemoveCommand(command)
			}
		}
		service := &progressSetup{}
		addSetupCommands(root, &code, func(_ string, _ []string, report func(setup.Progress)) setupService {
			service.report = report
			return service
		}, storedKey)
		var stderr bytes.Buffer
		root.SetErr(&stderr)
		args := []string{"setup"}
		if quiet {
			args = append(args, "--quiet")
		}
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		shown := strings.Contains(stderr.String(), "==> Nemotron 3 diarization model\n    https://huggingface.co/nvidia/model.gguf\n    Downloaded 107.0 MB\n==> Verifying installation\n")
		if shown == quiet {
			t.Fatalf("quiet=%v, progress shown=%v: %q", quiet, shown, stderr.String())
		}
	}
}
