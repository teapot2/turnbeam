// Package asr transcribes a complete recording on device with the pinned local
// speech recognition model, keeping each word's timing on the recording clock.
package asr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

// Word is one recognized word. Text may carry the model's punctuation.
type Word struct {
	Text       string
	Start, End time.Duration
}

var (
	// ErrOutput identifies missing, oversized or malformed native output.
	ErrOutput = errors.New("invalid local transcription output")
	// ErrCleanup reports temporary transcript data that could not be removed.
	ErrCleanup = errors.New("temporary transcription cleanup failed; inspect the temporary directory")
)

const (
	// maxOutputBytes bounds the native JSON; a 21-minute meeting needs ~0.2 MiB.
	maxOutputBytes = 64 << 20
	// maxOverrun is how far past the recording end a word may be reported, as
	// native frame padding, before the output is rejected instead of clipped.
	maxOverrun = time.Second
)

// Adapter runs exactly one recognition pass per complete prepared WAV. The
// caller's context supplies cancellation. Zero values use the default managed
// installation, which must include the chosen model, and the system temp directory.
type Adapter struct {
	InstallDir string
	TempDir    string
	ready      func(context.Context) (setup.Installation, error)
	command    func(context.Context, string, ...string) *exec.Cmd
}

// Transcribe recognizes a WAV from media.Processor.Prepare with one installed
// local model, without owning or modifying the WAV. It never downloads models
// and returns no words after a failure. Word times are moved earlier by the
// model's Latency, or only as far as the recording start allows, keeping each
// word's length; words are returned in start-time order, and ones reported
// slightly past the end are clipped to it.
func (a Adapter) Transcribe(ctx context.Context, recording *media.Prepared, model setup.LocalModel) (_ []Word, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !recording.ValidTimeline() || recording.Path == "" {
		return nil, errors.New("local transcription requires a complete normalized recording")
	}
	input, err := filepath.Abs(recording.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve normalized recording: %w", err)
	}
	ready := a.ready
	if ready == nil {
		ready = (setup.Manager{Dir: a.InstallDir, Models: []string{model.Name}}).Ready
	}
	installed, err := ready(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare local transcription: %w", err)
	}
	path := installed.ASRModels[model.Name]
	if path == "" {
		return nil, &setup.ModelError{Name: model.Name, Err: errors.New("not in the installation")}
	}
	dir, err := os.MkdirTemp(a.TempDir, "turnbeam-transcription-")
	if err != nil {
		return nil, fmt.Errorf("create private transcription directory: %w", err)
	}
	defer func() {
		if cleanup := os.RemoveAll(dir); cleanup != nil {
			err = errors.Join(err, ErrCleanup, cleanup)
		}
	}()
	output := filepath.Join(dir, "words.json")
	command := a.command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, installed.Runtime, "transcribe", input, "--model", path,
		"--word-times", "--format", "json", "--device", "metal", "--quiet", "--output", output, "--force")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := subprocess.Run(ctx, cmd); err != nil {
		return nil, fmt.Errorf("transcribe recording: %w", err)
	}
	data, err := readOutput(output)
	if err != nil {
		return nil, err
	}
	words, err := parse(data, recording.Duration, model.Latency)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return words, nil
}

func readOutput(path string) ([]byte, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOutput, err)
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxOutputBytes {
		return nil, fmt.Errorf("%w: nonregular or oversized output", ErrOutput)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOutput, err)
	}
	return data, nil
}

// parse validates the native JSON. Unknown fields are ignored so a newer
// runtime can add some; confidence and native speaker labels are not used.
func parse(data []byte, duration, latency time.Duration) ([]Word, error) {
	var doc struct {
		Text  *string `json:"text"`
		Words *[]struct {
			Word  *string  `json:"word"`
			Start *float64 `json:"start"`
			End   *float64 `json:"end"`
		} `json:"words"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOutput, err)
	}
	if doc.Text == nil || doc.Words == nil {
		return nil, fmt.Errorf("%w: text and word timings are required", ErrOutput)
	}
	words := make([]Word, 0, len(*doc.Words))
	for i, raw := range *doc.Words {
		start, end := seconds(raw.Start), seconds(raw.End)
		if raw.Word == nil || !utf8.ValidString(*raw.Word) || start < 0 || end < start {
			return nil, fmt.Errorf("%w: word %d has invalid text or timing", ErrOutput, i)
		}
		// Bounds apply to the corrected times: a late model reports the last
		// words past the end of the audio although they were spoken within it.
		// Near the start, the shift stops at zero so no word loses its length.
		shift := min(latency, start)
		start, end = start-shift, end-shift
		if start > duration || end > duration+maxOverrun {
			return nil, fmt.Errorf("%w: word %d is past the end of the recording", ErrOutput, i)
		}
		if text := strings.TrimSpace(*raw.Word); text != "" {
			words = append(words, Word{Text: text, Start: start, End: min(end, duration)})
		}
	}
	if len(words) == 0 && strings.TrimSpace(*doc.Text) != "" {
		return nil, fmt.Errorf("%w: text without word timings", ErrOutput)
	}
	sort.SliceStable(words, func(i, j int) bool { return words[i].Start < words[j].Start })
	return words, nil
}

// seconds converts a native timestamp; missing or non-finite values are -1.
func seconds(value *float64) time.Duration {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > math.MaxInt64/float64(time.Second) {
		return -1
	}
	return time.Duration(math.Round(*value * float64(time.Second)))
}
