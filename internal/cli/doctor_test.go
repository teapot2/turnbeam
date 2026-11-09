package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/setup"
)

func TestPrintChecksAlignsAndShowsFixes(t *testing.T) {
	checks := []setup.Check{
		{Name: "FFmpeg", Message: "ffmpeg 9.0.2", OK: true},
		{Name: "Installation", Message: "missing or damaged", Fix: "run `turnbeam setup`"},
	}
	var plain bytes.Buffer
	if err := printChecks(&plain, checks, false, false); err != nil {
		t.Fatal(err)
	}
	want := "  OK   FFmpeg        ffmpeg 9.0.2\n" +
		"  FAIL Installation  missing or damaged\n" +
		"                     -> run `turnbeam setup`\n"
	if plain.String() != want {
		t.Fatalf("plain checklist:\n%q\nwant\n%q", plain.String(), want)
	}
	var colored bytes.Buffer
	if err := printChecks(&colored, checks, true, true); err != nil {
		t.Fatal(err)
	}
	if got := colored.String(); strings.Contains(got, "FFmpeg") || !strings.Contains(got, ansiRed+"✗"+ansiReset) || !strings.Contains(got, "→"+ansiReset+" run `turnbeam setup`") {
		t.Fatalf("quiet colored checklist wrong: %q", got)
	}
}

func TestKeyCheck(t *testing.T) {
	for _, tc := range []struct {
		source    credentials.Source
		available bool
		err       error
		ok        bool
		fix       string
	}{
		{credentials.KeychainSource, true, nil, true, ""},
		{credentials.Environment, true, nil, true, ""},
		{"", false, nil, false, "turnbeam key set"},
		{"", false, errors.New("locked"), false, "unlock your login keychain"},
	} {
		check := keyCheck(context.Background(), func(context.Context) (credentials.Source, bool, error) {
			return tc.source, tc.available, tc.err
		}, false)
		if check.OK != tc.ok || !strings.Contains(check.Fix, tc.fix) || check.Name != "Meta API key" {
			t.Fatalf("key check for %+v = %+v", tc, check)
		}
	}
	// With a local model installed, Turnbeam can transcribe without a key.
	missing := func(context.Context) (credentials.Source, bool, error) { return "", false, nil }
	if check := keyCheck(context.Background(), missing, true); !check.OK || !strings.Contains(check.Message, "only --model meta") {
		t.Fatalf("missing key failed doctor despite a local model: %+v", check)
	}
	locked := func(context.Context) (credentials.Source, bool, error) { return "", false, errors.New("locked") }
	if check := keyCheck(context.Background(), locked, true); check.OK {
		t.Fatal("an unreadable keychain must still fail")
	}
}

func TestDoctorFailsWithoutAPIKey(t *testing.T) {
	code := 2
	root := newRoot("test", &code)
	for _, command := range root.Commands() {
		if command.Name() == "setup" || command.Name() == "doctor" {
			root.RemoveCommand(command)
		}
	}
	service := &fakeSetup{checks: []setup.Check{{Name: "Platform", Message: "macOS on Apple Silicon", OK: true}}}
	missing := func(context.Context) (credentials.Source, bool, error) { return "", false, nil }
	addSetupCommands(root, &code, func(string, []string, func(setup.Progress)) setupService { return service }, missing)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetArgs([]string{"doctor"})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, setup.ErrDoctorFailed) || code != exitFailed {
		t.Fatalf("missing key did not fail doctor: %v (code %d)", err, code)
	}
	got := stderr.String()
	if !strings.Contains(got, "FAIL Meta API key") || !strings.Contains(got, "-> run `turnbeam key set`") || strings.Contains(got, "Ready to transcribe") {
		t.Fatalf("doctor output: %q", got)
	}

	service.checks = append(service.checks, setup.Check{Name: "Local model", Message: "parakeet: Parakeet TDT v3 Q8_0", OK: true, LocalModel: "parakeet"})
	stderr.Reset()
	if err := root.ExecuteContext(context.Background()); err != nil || !strings.Contains(stderr.String(), "Ready to transcribe") {
		t.Fatalf("doctor with a local model and no key: %v, %q", err, stderr.String())
	}
}
