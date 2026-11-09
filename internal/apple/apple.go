// Package apple transcribes on device with Apple's SpeechTranscriber (macOS 26
// or later). The Speech API is Swift-only, so Turnbeam builds a small helper
// from embedded source with the Xcode Command Line Tools and runs it like the
// native runtime. Apple's speech models are managed by macOS, per language.
package apple

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/asr"
	"github.com/teapot2/turnbeam/internal/limitbuf"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

//go:embed helper.swift
var source []byte

// Model is the --model name of Apple's on-device transcriber.
const Model = "apple"

// Latency is how late the transcriber reports words, removed from their times
// like a pinned model's Latency. Against human word timings on the AMI meeting
// ES2002a its words came a median 50 ms early (start and end), and no shift
// placed more words with the right speaker.
const Latency = 0 * time.Millisecond

const (
	// minimumMajor is the first macOS with SpeechAnalyzer.
	minimumMajor = 26
	// helperPrefix names built helpers; the suffix identifies the source.
	helperPrefix   = "turnbeam-speech-"
	maxStateBytes  = 64 << 10
	maxOutputBytes = 64 << 20
	buildTimeout   = 5 * time.Minute
	stateTimeout   = 10 * time.Minute
)

var (
	// ErrUnavailable means this Mac cannot run Apple's on-device transcriber.
	ErrUnavailable = errors.New("Apple's on-device transcription needs macOS 26 or later on Apple Silicon")
	// ErrBuild means the helper could not be compiled.
	ErrBuild = errors.New("could not build the Apple Speech helper; install the Xcode Command Line Tools with `xcode-select --install` and run setup again")
	// ErrLanguage means Apple's transcriber does not support the language.
	ErrLanguage = errors.New("Apple's on-device transcription does not support this language")
	// ErrNotSetUp identifies a missing helper or speech model; see SetupError.
	ErrNotSetUp = errors.New("Apple Speech is not set up")
	// ErrOutput identifies missing, oversized or malformed helper output.
	ErrOutput = errors.New("invalid Apple Speech output")
)

// SetupError names the command that sets Apple Speech up for a language.
type SetupError struct {
	Language string
}

func (e *SetupError) Error() string {
	command := "turnbeam setup --model apple"
	if e.Language != "" && e.Language != "auto" {
		command += " --language " + e.Language
	}
	return fmt.Sprintf("Apple Speech is not set up for this language; run `%s`", command)
}

func (e *SetupError) Unwrap() error { return ErrNotSetUp }

// State reports Apple's speech model for one language: Status is installed,
// downloading, supported (can be installed) or unsupported.
type State struct {
	Locale string `json:"locale"`
	Status string `json:"status"`
	OS     string `json:"os"`
}

// Result is a transcription with the locale and macOS version that produced it.
type Result struct {
	Locale, OS string
	Words      []asr.Word
}

// Helper builds and runs the Swift helper inside an installation directory.
// Language is a BCP-47 tag such as en-US; empty means the Mac's language.
type Helper struct {
	Dir     string
	TempDir string
	command func(context.Context, string, ...string) *exec.Cmd
}

func (h Helper) run(ctx context.Context, name string, args ...string) *exec.Cmd {
	if h.command != nil {
		return h.command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (h Helper) directory() (string, error) {
	dir := h.Dir
	if dir == "" {
		var err error
		if dir, err = setup.DefaultDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "apple"), nil
}

// Path is where the helper for this Turnbeam's source is built.
func (h Helper) Path() (string, error) {
	dir, err := h.directory()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(source)
	return filepath.Join(dir, helperPrefix+hex.EncodeToString(sum[:8])), nil
}

// Built reports whether the helper for this source is present.
func (h Helper) Built() bool {
	path, err := h.Path()
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0100 != 0
}

// Build compiles the helper unless it is already built, then removes helpers
// built from older sources. The Mac must run macOS 26 or later.
func (h Helper) Build(ctx context.Context) (err error) {
	if err := h.supported(ctx); err != nil {
		return err
	}
	if h.Built() {
		return nil
	}
	path, err := h.Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create Apple Speech directory: %w", err)
	}
	work, err := os.MkdirTemp(dir, ".build-")
	if err != nil {
		return fmt.Errorf("create Apple Speech build directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	src, binary := filepath.Join(work, "helper.swift"), filepath.Join(work, "turnbeam-speech")
	if err := os.WriteFile(src, source, 0600); err != nil {
		return err
	}
	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := h.run(buildCtx, "xcrun", "swiftc", "-parse-as-library", "-O", "-target", "arm64-apple-macos26.0", "-module-cache-path", filepath.Join(work, "cache"), "-o", binary, src)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := subprocess.Run(buildCtx, cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrBuild, err)
	}
	if err := os.Chmod(binary, 0700); err != nil {
		return err
	}
	if err := os.Rename(binary, path); err != nil {
		return err
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, helperPrefix) && name != filepath.Base(path) {
			os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}

// supported checks the macOS version, since the helper needs SpeechAnalyzer.
func (h Helper) supported(ctx context.Context) error {
	cmd := h.run(ctx, "sw_vers", "-productVersion")
	out := limitbuf.Buffer{Limit: 256}
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err := subprocess.Run(ctx, cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrUnavailable, err)
	}
	major, _, _ := strings.Cut(strings.TrimSpace(string(out.Bytes())), ".")
	if n, err := strconv.Atoi(major); err != nil || n < minimumMajor {
		return ErrUnavailable
	}
	return nil
}

// Status reports Apple's speech model for a language without changing it.
func (h Helper) Status(ctx context.Context, language string) (State, error) {
	return h.state(ctx, "status", language)
}

// Install asks macOS to download and reserve Apple's speech model for a
// language, for this helper. It returns the resulting state.
func (h Helper) Install(ctx context.Context, language string) (State, error) {
	return h.state(ctx, "install", language)
}

func (h Helper) state(ctx context.Context, command, language string) (State, error) {
	path, err := h.ready(language)
	if err != nil {
		return State{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	cmd := h.run(ctx, path, command, "--locale", tag(language))
	out := limitbuf.Buffer{Limit: maxStateBytes}
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err := h.exec(ctx, cmd, language); err != nil {
		return State{}, err
	}
	var state State
	if out.Overflow || json.Unmarshal(bytes.TrimSpace(out.Bytes()), &state) != nil || state.Locale == "" || state.Status == "" {
		return State{}, fmt.Errorf("%w: malformed %s result", ErrOutput, command)
	}
	return state, nil
}

// Transcribe recognizes a prepared WAV in a language, which must be installed.
// Word times move earlier by Latency; words are sorted and bounded like the
// native runtime's.
func (h Helper) Transcribe(ctx context.Context, recording *media.Prepared, language string) (_ Result, err error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !recording.ValidTimeline() || recording.Path == "" {
		return Result{}, errors.New("Apple transcription requires a complete normalized recording")
	}
	path, err := h.ready(language)
	if err != nil {
		return Result{}, err
	}
	input, err := filepath.Abs(recording.Path)
	if err != nil {
		return Result{}, err
	}
	work, err := os.MkdirTemp(h.TempDir, "turnbeam-apple-")
	if err != nil {
		return Result{}, fmt.Errorf("create private transcription directory: %w", err)
	}
	defer func() {
		if cleanup := os.RemoveAll(work); cleanup != nil {
			err = errors.Join(err, asr.ErrCleanup, cleanup)
		}
	}()
	output := filepath.Join(work, "words.json")
	cmd := h.run(ctx, path, "transcribe", "--locale", tag(language), "--input", input, "--output", output)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := h.exec(ctx, cmd, language); err != nil {
		return Result{}, err
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxOutputBytes {
		return Result{}, fmt.Errorf("%w: missing, nonregular or oversized output", ErrOutput)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrOutput, err)
	}
	var meta struct {
		Locale string `json:"locale"`
		OS     string `json:"os"`
	}
	if err := json.Unmarshal(data, &meta); err != nil || meta.Locale == "" || meta.OS == "" {
		return Result{}, fmt.Errorf("%w: locale and macOS version are required", ErrOutput)
	}
	words, err := asr.ParseWords(data, recording.Duration, Latency)
	if err != nil {
		return Result{}, errors.Join(ErrOutput, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Locale: meta.Locale, OS: meta.OS, Words: words}, nil
}

func (h Helper) ready(language string) (string, error) {
	if !h.Built() {
		return "", &SetupError{Language: language}
	}
	return h.Path()
}

// exec runs the helper and maps its exit codes to errors.
func (h Helper) exec(ctx context.Context, cmd *exec.Cmd, language string) error {
	err := subprocess.Run(ctx, cmd)
	if err == nil || ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 3:
			return ErrUnavailable
		case 4:
			return fmt.Errorf("%w: %s", ErrLanguage, tag(language))
		case 5:
			return &SetupError{Language: language}
		}
	}
	return fmt.Errorf("Apple Speech helper failed: %w", err)
}

var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

// ValidLanguage reports whether s looks like a BCP-47 language tag, such as
// en, en-US or zh-Hans-CN.
func ValidLanguage(s string) bool { return languageTag.MatchString(s) }

func tag(language string) string {
	if language == "" {
		return "auto"
	}
	return language
}
