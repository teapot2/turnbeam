package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
)

func TestErrorClassesDoNotPrintUntrustedCauses(t *testing.T) {
	secret := "fake-private-token"
	for _, test := range []struct {
		err   error
		class errorClass
	}{
		{credentials.ErrMissing, configuration}, {pipeline.ErrInput, configuration}, {jobs.ErrCorrupt, configuration},
		{setup.ErrNotReady, dependency}, {subprocess.ErrStart, dependency},
		{setup.ErrDownload, remote}, {setup.ErrUnsafeDirectory, configuration},
		{errors.Join(credentials.ErrNoKeyEntered, io.EOF), configuration},
		{fmt.Errorf("%w (OSStatus -25293)", credentials.ErrKeychain), dependency},
		{errors.Join(meta.ErrRetryLater, &meta.Error{Kind: meta.ErrHTTP, StatusCode: 429}), remote},
		{errors.Join(output.ErrSource, filelock.ErrBusy), storage},
		{&meta.Error{Kind: meta.ErrHTTP, StatusCode: 401, Code: secret, SessionID: secret}, remote},
		{errors.Join(meta.ErrTransport, context.DeadlineExceeded), remote},
		{media.ErrInvalidMedia, processing}, {output.ErrConflict, storage}, {output.ErrCommit, storage},
		{&os.PathError{Op: "open", Path: secret, Err: os.ErrPermission}, storage},
		{errors.New(secret), processing},
	} {
		wrapped := fmt.Errorf("Bearer %s\nprivate recording text\x1b[31m: %w", secret, test.err)
		class, message := classify(wrapped)
		code, diagnostic := reportFailure(context.Background(), wrapped, 1, "")
		if class != test.class || code != 1 || strings.Contains(message+diagnostic, secret) || strings.Contains(diagnostic, "private recording") || strings.ContainsAny(diagnostic, "\r\n\x1b") {
			t.Fatal("classification or redaction failed", class, diagnostic)
		}
		if !errors.Is(wrapped, test.err) {
			t.Fatal("classification altered the error chain")
		}
	}
}

func TestInvocationCancellationDiffersFromAttemptTimeout(t *testing.T) {
	if code, _ := reportFailure(context.Background(), errors.Join(meta.ErrTransport, context.DeadlineExceeded), 1, ""); code != 1 {
		t.Fatal("request timeout was reported as CLI cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, message := reportFailure(ctx, context.Canceled, 1, ""); code != 130 || message != "canceled" {
		t.Fatal("invocation cancellation not recognized")
	}
	for _, cleanup := range []error{output.ErrCleanup, output.ErrCommit, media.ErrCleanup, setup.ErrCleanup, subprocess.ErrCleanup} {
		if code, message := reportFailure(ctx, errors.Join(context.Canceled, cleanup), 1, ""); code != 130 || !strings.Contains(message, cleanup.Error()) {
			t.Fatal("cancellation hid cleanup/commit failure", message)
		}
	}
}

func TestUsageRedactionAndSingleLineDiagnostics(t *testing.T) {
	for _, key := range []string{"fake-secret-key", "fake-secret-key\"\\value", "fake-secret-key\nvalue"} {
		t.Setenv(credentials.EnvName, key)
		code, diagnostic := invoke(context.Background(), "meeting.wav", "--concurrency="+key+"\x1b[31m\n")
		if code != 2 || strings.Contains(diagnostic, "fake-secret-key") || strings.Contains(diagnostic, "\x1b") || strings.Count(diagnostic, "\n") != 1 {
			t.Fatal("usage error exposed a key or terminal controls", diagnostic)
		}
	}
}

func TestCancellationTellsHowToContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for command, want := range map[string]string{
		"turnbeam": "run the same command with --resume to continue",
		"setup":    "run `turnbeam setup` again to resume the download",
		"export":   "canceled",
	} {
		if code, message := reportFailure(ctx, context.Canceled, 1, command); code != exitCanceled || !strings.HasSuffix(message, want) {
			t.Fatalf("%s: code %d, %q", command, code, message)
		}
	}
}
