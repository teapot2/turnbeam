package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/teapot2/turnbeam/internal/asr"
	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type errorClass string

const (
	configuration errorClass = "configuration"
	dependency    errorClass = "dependency"
	remote        errorClass = "remote"
	processing    errorClass = "processing"
	storage       errorClass = "storage"
)

type rule struct {
	target error
	class  errorClass
}

// cleanupFailures may leave partial data behind, so they are reported first
// and still mentioned when the run was canceled.
var cleanupFailures = []rule{
	{output.ErrCleanup, storage}, {output.ErrCommit, storage},
	{media.ErrCleanup, storage}, {diarization.ErrCleanup, storage}, {asr.ErrCleanup, storage}, {setup.ErrCleanup, storage}, {subprocess.ErrCleanup, processing},
}

// Classification uses typed causes, never upstream Error() text. Keep detailed
// causes in processing packages for errors.Is/As, not in terminal diagnostics.
func classify(err error) (errorClass, string) {
	for _, rule := range cleanupFailures {
		if errors.Is(err, rule.target) {
			return rule.class, rule.target.Error()
		}
	}
	// The message names only a pinned model and the command that installs it.
	// Doctor's own summary wins: its checks already name every model to fix.
	if model := (*setup.ModelError)(nil); !errors.Is(err, setup.ErrDoctorFailed) && errors.As(err, &model) {
		return dependency, model.Error()
	}
	for _, rule := range append(slices.Clone(cleanupFailures), []rule{
		{output.ErrConflict, storage}, {output.ErrNoFreeName, storage}, {output.ErrSourceChanged, storage},
		{jobs.ErrCorrupt, configuration}, {jobs.ErrMismatch, configuration}, {jobs.ErrMissing, configuration}, {jobs.ErrBusy, configuration},
		{credentials.ErrMissing, configuration}, {credentials.ErrInvalid, configuration}, {credentials.ErrTerminal, configuration},
		{credentials.ErrNoKeyEntered, configuration}, {credentials.ErrKeychain, dependency},
		{credentials.ErrUnavailable, dependency}, {setup.ErrUnsupportedPlatform, dependency}, {setup.ErrDoctorFailed, dependency},
		{setup.ErrDownload, remote}, {setup.ErrUnsafeDirectory, configuration}, {setup.ErrLocalModel, dependency}, {setup.ErrUnknownModel, configuration},
		{setup.ErrIntegrity, dependency}, {setup.ErrBusy, dependency}, {setup.ErrNotReady, dependency}, {subprocess.ErrStart, dependency},
		{pipeline.ErrInput, configuration},
		{transcript.ErrUnsupportedVersion, configuration}, {transcript.ErrSpeakerMapping, configuration},
		{transcript.ErrInvalid, configuration}, {transcript.ErrRenderOptions, configuration}, {jobs.ErrOptions, configuration},
		{output.ErrPublishLocked, storage}, {filelock.ErrBusy, storage},
		{output.ErrSource, configuration}, {output.ErrPath, configuration},
		{output.ErrStorage, storage},
		{media.ErrNoAudio, configuration}, {media.ErrInvalidMedia, processing}, {media.ErrChunkSize, processing},
		{diarization.ErrRTTM, processing}, {asr.ErrOutput, processing}, {chunking.ErrPlan, processing},
		{meta.ErrRetryLater, remote},
	}...) {
		if errors.Is(err, rule.target) {
			return rule.class, rule.target.Error()
		}
	}
	if errors.Is(err, meta.ErrHTTP) {
		var e *meta.Error
		if errors.As(err, &e) {
			message := fmt.Sprintf("transcription service returned HTTP %d", e.StatusCode)
			if e.StatusCode == 401 || e.StatusCode == 403 {
				message += "; check your API key and access"
			}
			return remote, message
		}
	}
	if errors.Is(err, meta.ErrTransport) {
		return remote, "transcription connection failed or request timed out; valid checkpoints are retained"
	}
	if errors.Is(err, meta.ErrResponse) {
		return remote, meta.ErrResponse.Error()
	}
	if errors.Is(err, meta.ErrRequest) {
		return configuration, meta.ErrRequest.Error()
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return storage, "file operation failed; check permissions, available space and destination"
	}
	return processing, "operation failed; check the input and run doctor to verify local dependencies"
}

func reportFailure(ctx context.Context, err error, code int, command string) (int, string) {
	if ctx.Err() != nil {
		message := canceledMessage(ctx, command)
		// Cancellation must not hide an incomplete rollback or uncertain commit.
		if slices.ContainsFunc(cleanupFailures, func(r rule) bool { return errors.Is(err, r.target) }) {
			_, detail := classify(err)
			message += "; " + detail
		}
		return exitCanceled, message
	}
	if code == exitUsage {
		return exitUsage, safeUsage(err.Error())
	}
	class, message := classify(err)
	return exitFailed, string(class) + ": " + message
}

func safeUsage(message string) string {
	if key := os.Getenv(credentials.EnvName); key != "" {
		quoted := strconv.Quote(key)
		message = strings.ReplaceAll(message, quoted[1:len(quoted)-1], "[redacted]")
		message = strings.ReplaceAll(message, key, "[redacted]")
	}
	var safe strings.Builder
	for _, r := range message {
		if safe.Len() >= 1024 {
			safe.WriteString("...")
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			fmt.Fprintf(&safe, "\\u%04x", r)
		} else {
			safe.WriteRune(r)
		}
	}
	return safe.String()
}

// canceledMessage tells people how to continue after Ctrl-C. An expired
// deadline keeps its own wording, since nobody pressed anything.
func canceledMessage(ctx context.Context, command string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err().Error()
	}
	switch command {
	case "turnbeam":
		return "canceled; completed chunks are saved, so run the same command with --resume to continue"
	case "turnbeam (local model)":
		return "canceled; local runs save nothing, so run the same command again to start over"
	case "setup":
		return "canceled; run `turnbeam setup` again to resume the download"
	}
	return "canceled"
}
