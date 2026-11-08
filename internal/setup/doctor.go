package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/limitbuf"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

// Check is one offline dependency or installation diagnostic. Fix, set only on
// failure, is the action that resolves it.
type Check struct {
	Name, Message, Fix string
	OK                 bool
}

// ErrDoctorFailed indicates an environment that is not ready for transcription.
var ErrDoctorFailed = errors.New("Turnbeam is not ready; fix the failed checks above")

type doctorError struct{ causes []error }

func (e doctorError) Error() string { return ErrDoctorFailed.Error() }
func (e doctorError) Unwrap() []error {
	return append([]error{ErrDoctorFailed}, e.causes...)
}

// Doctor is offline: it verifies the entire pinned installation before executing
// its runtime, then checks Metal availability and local model-header compatibility.
// It does not claim to perform inference or automatically repair anything.
func (m Manager) Doctor(ctx context.Context) ([]Check, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.platform(); err != nil {
		return []Check{{Name: "Platform", Message: "unsupported", Fix: "use a Mac with Apple Silicon"}}, err
	}
	checks := []Check{{Name: "Platform", Message: "macOS on Apple Silicon", OK: true}}
	var failures []error
	for _, tool := range [...]struct{ command, name string }{{"ffmpeg", "FFmpeg"}, {"ffprobe", "FFprobe"}} {
		data, err := m.run(ctx, tool.command, "-version")
		version, ok := toolVersion(data, tool.command)
		if err == nil && !ok {
			err = errors.New("unexpected dependency version response")
		}
		check := Check{Name: tool.name, OK: err == nil, Message: tool.command + " " + version}
		if err != nil {
			check.Message = "not found or not working"
			check.Fix = "install FFmpeg (for example `brew install ffmpeg`) and make sure it is on PATH"
			failures = append(failures, err)
		}
		checks = append(checks, check)
		if err := ctx.Err(); err != nil {
			return checks, err
		}
	}
	installed, err := m.Verify(ctx)
	checks = append(checks, Check{Name: "Installation", OK: err == nil, Message: "pinned runtime and model files verified"})
	if err != nil {
		checks[len(checks)-1].Message = "missing or damaged"
		checks[len(checks)-1].Fix = "run `turnbeam setup` (with the same --install-dir, if you used one)"
		failures = append(failures, err)
	} else {
		device, err := m.nativeChecks(ctx, installed)
		check := Check{Name: "Speaker model", OK: err == nil, Message: "NeMo-Speech.cpp v" + RuntimeVersion + " with Nemotron 3 Q8_0 on " + device}
		if err != nil {
			check.Message = "runtime or model compatibility check failed"
			check.Fix = "run `turnbeam setup` again; Metal needs a logged-in macOS session with GPU access"
			failures = append(failures, err)
		}
		checks = append(checks, check)
	}
	if err := ctx.Err(); err != nil {
		return checks, err
	}
	if len(failures) != 0 {
		return checks, doctorError{failures}
	}
	return checks, nil
}

func (m Manager) nativeChecks(ctx context.Context, installed Installation) (string, error) {
	data, err := m.run(ctx, installed.Runtime, "--json", "doctor")
	if err != nil {
		return "", fmt.Errorf("native runtime failed; rerun setup or check macOS compatibility: %w", err)
	}
	var result struct {
		Version              string `json:"version"`
		AcceleratorAvailable bool   `json:"accelerator_available"`
		AcceleratorCompiled  bool   `json:"accelerator_compiled"`
		DriverCompatible     bool   `json:"driver_runtime_compatible"`
		Features             struct {
			Metal       bool `json:"backend_metal"`
			Diarization bool `json:"diarization"`
		} `json:"features"`
		Devices []struct {
			Name, Description, Type string
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("invalid native doctor response: %w", err)
	}
	if result.Version != RuntimeVersion || !result.Features.Diarization || !result.Features.Metal || !result.DriverCompatible || !result.AcceleratorAvailable || !result.AcceleratorCompiled {
		return "", errors.New("runtime/Metal compatibility failed; use supported macOS on Apple Silicon and rerun setup")
	}
	device := ""
	for _, d := range result.Devices {
		if d.Type == "gpu" && strings.HasPrefix(d.Name, "MTL") && d.Description != "" {
			device = "Metal"
			// Show only Apple chip names; other native metadata is never echoed.
			if appleChip.MatchString(d.Description) {
				device += " (" + d.Description + ")"
			}
			break
		}
	}
	if device == "" {
		return "", errors.New("no usable Metal GPU detected; run in a macOS session with GPU access")
	}
	data, err = m.run(ctx, installed.Runtime, "--json", "model", "info", installed.Model)
	if err != nil {
		return "", fmt.Errorf("model inspection failed; rerun setup: %w", err)
	}
	var model struct {
		Architecture      string   `json:"architecture"`
		Role              string   `json:"role"`
		RuntimeCompatible bool     `json:"runtime_compatible"`
		Errors            []string `json:"errors"`
	}
	if err := json.Unmarshal(data, &model); err != nil {
		return "", fmt.Errorf("invalid model inspection response: %w", err)
	}
	if !model.RuntimeCompatible || model.Architecture != "sortformer" || model.Role != "diarization" || len(model.Errors) != 0 {
		return "", errors.New("model/runtime mismatch; rerun setup for the pinned Nemotron 3 model")
	}
	return device, nil
}

func (m Manager) run(ctx context.Context, tool string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := m.command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, tool, args...)
	output := limitbuf.Buffer{Limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := subprocess.Run(ctx, cmd); err != nil {
		return nil, err
	}
	if output.Overflow {
		return nil, errors.New("native diagnostic output exceeded 64 KiB")
	}
	return output.Bytes(), nil
}

var appleChip = regexp.MustCompile(`^Apple M[0-9]{1,2}( (Pro|Max|Ultra))?$`)

// toolVersion extracts the version from the first line of `<tool> -version`.
func toolVersion(data []byte, tool string) (string, bool) {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	rest, ok := bytes.CutPrefix(line, []byte(tool+" version "))
	if !ok {
		return "", false
	}
	version, _, _ := bytes.Cut(rest, []byte(" "))
	return displayText(string(version), 40)
}

// displayText accepts short printable ASCII from native tools for display.
func displayText(s string, limit int) (string, bool) {
	if s == "" || len(s) > limit {
		return "", false
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return "", false
		}
	}
	return s, true
}
