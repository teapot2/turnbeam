package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

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

// Classification uses typed causes, never upstream Error() text. Keep detailed
// causes in processing packages for errors.Is/As, not in terminal diagnostics.
func classify(err error) (errorClass, string) {
	for _, rule := range []struct {
		target error
		class  errorClass
	}{
		{output.ErrCleanup, storage}, {output.ErrCommit, storage},
		{media.ErrCleanup, storage}, {diarization.ErrCleanup, storage}, {setup.ErrCleanup, storage}, {subprocess.ErrCleanup, processing},
		{output.ErrConflict, storage}, {output.ErrSourceChanged, storage},
		{jobs.ErrCorrupt, configuration}, {jobs.ErrMismatch, configuration}, {jobs.ErrMissing, configuration}, {jobs.ErrBusy, configuration},
		{credentials.ErrMissing, configuration}, {credentials.ErrInvalid, configuration}, {credentials.ErrTerminal, configuration},
		{credentials.ErrUnavailable, dependency}, {setup.ErrUnsupportedPlatform, dependency}, {setup.ErrDoctorFailed, dependency},
		{setup.ErrDownload, remote}, {setup.ErrUnsafeDirectory, configuration},
		{setup.ErrIntegrity, dependency}, {setup.ErrBusy, dependency}, {setup.ErrNotReady, dependency}, {subprocess.ErrStart, dependency},
		{pipeline.ErrInput, configuration},
		{transcript.ErrUnsupportedVersion, configuration}, {transcript.ErrSpeakerMapping, configuration},
		{transcript.ErrInvalid, configuration}, {transcript.ErrRenderOptions, configuration}, {jobs.ErrOptions, configuration},
		{output.ErrPublishLocked, storage}, {filelock.ErrBusy, storage},
		{output.ErrSource, configuration}, {output.ErrPath, configuration},
		{output.ErrStorage, storage},
		{media.ErrNoAudio, configuration}, {media.ErrInvalidMedia, processing}, {media.ErrChunkSize, processing},
		{diarization.ErrRTTM, processing}, {chunking.ErrPlan, processing},
	} {
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

func reportFailure(ctx context.Context, err error, code int) (int, string) {
	if ctx.Err() != nil {
		message := ctx.Err().Error()
		// Cancellation must not hide an incomplete rollback or uncertain commit.
		if errors.Is(err, output.ErrCleanup) || errors.Is(err, output.ErrCommit) || errors.Is(err, media.ErrCleanup) || errors.Is(err, diarization.ErrCleanup) || errors.Is(err, setup.ErrCleanup) || errors.Is(err, subprocess.ErrCleanup) {
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
