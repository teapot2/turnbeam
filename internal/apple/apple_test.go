package apple

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

	"github.com/teapot2/turnbeam/internal/asr"
	"github.com/teapot2/turnbeam/internal/media"
)

// TestHelperProcess stands in for sw_vers, xcrun and the built helper.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_APPLE_HELPER") != "1" {
		return
	}
	args := os.Args
	for args[0] != "--" {
		args = args[1:]
	}
	name, args := filepath.Base(args[1]), args[2:]
	mode := os.Getenv("TURNBEAM_APPLE_MODE")
	value := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	switch {
	case name == "sw_vers":
		os.Stdout.WriteString(os.Getenv("TURNBEAM_APPLE_OS") + "\n")
	case name == "xcrun":
		if mode == "nobuild" || args[0] != "swiftc" || !slices.Contains(args, "-parse-as-library") {
			os.Exit(1)
		}
		os.WriteFile(value("-o"), []byte("helper"), 0600)
	case strings.HasPrefix(name, helperPrefix):
		if code := os.Getenv("TURNBEAM_APPLE_EXIT"); code != "" {
			switch code {
			case "3":
				os.Exit(3)
			case "4":
				os.Exit(4)
			case "5":
				os.Exit(5)
			}
			os.Exit(1)
		}
		locale := value("--locale")
		if locale == "auto" {
			locale = "en-US"
		}
		switch args[0] {
		case "status", "install":
			status := "supported"
			if args[0] == "install" {
				status = "installed"
			}
			os.Stdout.WriteString(`{"locale":"` + locale + `","status":"` + status + `","os":"Version 27.0 (Build test)"}` + "\n")
		case "transcribe":
			data := `{"locale":"` + locale + `","os":"Version 27.0 (Build test)","text":"Hola, mundo.","words":[` +
				`{"word":"mundo.","start":0.5,"end":0.9},{"word":"Hola,","start":0.1,"end":0.5}]}`
			if mode != "" {
				data = mode
			}
			os.WriteFile(value("--output"), []byte(data), 0600)
		}
	}
	os.Exit(0)
}

func fakeHelper(t *testing.T, env ...string) Helper {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Helper{Dir: t.TempDir(), TempDir: t.TempDir(), command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestHelperProcess$", "--", name}, args...)...)
		cmd.Env = append(append(os.Environ(), "TURNBEAM_APPLE_HELPER=1", "TURNBEAM_APPLE_OS=27.0.1"), env...)
		return cmd
	}}
}

func built(t *testing.T, h Helper) Helper {
	t.Helper()
	path, err := h.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("helper"), 0700); err != nil {
		t.Fatal(err)
	}
	return h
}

func prepared(t *testing.T) *media.Prepared {
	t.Helper()
	path := filepath.Join(t.TempDir(), "normalized.wav")
	if err := os.WriteFile(path, make([]byte, 44+media.SampleRate*2), 0600); err != nil {
		t.Fatal(err)
	}
	return &media.Prepared{Path: path, Samples: media.SampleRate, Duration: time.Second}
}

func TestBuild(t *testing.T) {
	h := fakeHelper(t)
	if h.Built() {
		t.Fatal("built before building")
	}
	path, _ := h.Path()
	stale := filepath.Join(filepath.Dir(path), helperPrefix+"0000000000000000")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(stale, []byte("old"), 0700)
	if err := h.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.Built() || !strings.HasPrefix(filepath.Base(path), helperPrefix) || len(filepath.Base(path)) != len(helperPrefix)+16 {
		t.Fatalf("not built at %s", path)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper from an older source was kept")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("build left files behind: %v", entries)
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0700 {
		t.Fatal("helper directory is not private")
	}
}

func TestBuildFailures(t *testing.T) {
	old := fakeHelper(t, "TURNBEAM_APPLE_OS=15.6")
	if err := old.Build(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("macOS 15: %v", err)
	}
	broken := fakeHelper(t, "TURNBEAM_APPLE_MODE=nobuild")
	if err := broken.Build(context.Background()); !errors.Is(err, ErrBuild) || broken.Built() {
		t.Fatalf("failed compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fakeHelper(t).Build(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled build: %v", err)
	}
}

func TestStatusAndInstall(t *testing.T) {
	h := built(t, fakeHelper(t))
	if state, err := h.Status(context.Background(), ""); err != nil || state != (State{Locale: "en-US", Status: "supported", OS: "Version 27.0 (Build test)"}) {
		t.Fatalf("status: %+v, %v", state, err)
	}
	if state, err := h.Install(context.Background(), "es-MX"); err != nil || state.Locale != "es-MX" || state.Status != "installed" {
		t.Fatalf("install: %+v, %v", state, err)
	}
	var missing *SetupError
	if _, err := fakeHelper(t).Status(context.Background(), "es-MX"); !errors.As(err, &missing) || !errors.Is(err, ErrNotSetUp) || !strings.Contains(err.Error(), "--language es-MX") {
		t.Fatalf("unbuilt helper: %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	for code, want := range map[string]error{"3": ErrUnavailable, "4": ErrLanguage, "5": ErrNotSetUp} {
		h := built(t, fakeHelper(t, "TURNBEAM_APPLE_EXIT="+code))
		if _, err := h.Status(context.Background(), "fr-FR"); !errors.Is(err, want) {
			t.Errorf("exit %s: %v", code, err)
		}
	}
	h := built(t, fakeHelper(t, "TURNBEAM_APPLE_EXIT=1"))
	if _, err := h.Transcribe(context.Background(), prepared(t), ""); err == nil || errors.Is(err, ErrNotSetUp) {
		t.Fatalf("helper failure: %v", err)
	}
}

func TestTranscribe(t *testing.T) {
	h := built(t, fakeHelper(t))
	result, err := h.Transcribe(context.Background(), prepared(t), "es-MX")
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	want := []asr.Word{{Text: "Hola,", Start: ms(100), End: ms(500)}, {Text: "mundo.", Start: ms(500), End: ms(900)}}
	if err != nil || result.Locale != "es-MX" || result.OS != "Version 27.0 (Build test)" || !reflect.DeepEqual(result.Words, want) {
		t.Fatalf("transcribe: %+v, %v", result, err)
	}
	if entries, _ := os.ReadDir(h.TempDir); len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	for name, data := range map[string]string{
		"malformed":    "{",
		"no locale":    `{"os":"x","text":"","words":[]}`,
		"no words":     `{"locale":"en-US","os":"x","text":"hi"}`,
		"bad timing":   `{"locale":"en-US","os":"x","text":"hi","words":[{"word":"hi","start":0.5,"end":0.1}]}`,
		"past the end": `{"locale":"en-US","os":"x","text":"hi","words":[{"word":"hi","start":5,"end":6}]}`,
	} {
		bad := built(t, fakeHelper(t, "TURNBEAM_APPLE_MODE="+data))
		if _, err := bad.Transcribe(context.Background(), prepared(t), ""); !errors.Is(err, ErrOutput) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := fakeHelper(t).Transcribe(context.Background(), prepared(t), ""); !errors.Is(err, ErrNotSetUp) {
		t.Fatalf("unbuilt helper: %v", err)
	}
}

func TestValidLanguage(t *testing.T) {
	for _, tag := range []string{"en", "en-US", "es-MX", "zh-Hans-CN", "yue-HK"} {
		if !ValidLanguage(tag) {
			t.Errorf("%s rejected", tag)
		}
	}
	for _, tag := range []string{"", "e", "en US", "en_US", "--locale", "en-US;rm", "english-language-tag-too"} {
		if ValidLanguage(tag) {
			t.Errorf("%q accepted", tag)
		}
	}
}
