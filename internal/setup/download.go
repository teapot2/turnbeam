package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ErrIntegrity identifies artifacts that do not match the trusted pins.
var ErrIntegrity = errors.New("installation integrity check failed")

// ErrDownload identifies a failed or rejected artifact download.
var ErrDownload = errors.New("download failed; check network access and rerun setup")

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func digest(ctx context.Context, r io.Reader, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx, r}, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("%w: unexpected artifact size", ErrIntegrity)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyAsset(ctx context.Context, root *os.Root, a artifact) error {
	return verifyFile(ctx, root, a, 0600)
}

func verifyFile(ctx context.Context, root *os.Root, a artifact, mode os.FileMode) error {
	stat, err := root.Lstat(a.name)
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() != a.size || stat.Mode().Perm() != mode {
		return fmt.Errorf("%w: invalid %s file", ErrIntegrity, a.name)
	}
	f, err := root.Open(a.name)
	if err != nil {
		return err
	}
	defer f.Close()
	sum, err := digest(ctx, f, a.size)
	if err != nil {
		return err
	}
	if sum != a.sha256 {
		return fmt.Errorf("%w: %s checksum mismatch", ErrIntegrity, a.name)
	}
	return nil
}

func saveAsset(ctx context.Context, root *os.Root, a artifact, input io.Reader) (err error) {
	f, err := root.OpenFile(a.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	sum, err := digest(ctx, io.TeeReader(input, f), a.size)
	if err != nil {
		return err
	}
	if sum != a.sha256 {
		return fmt.Errorf("%w: %s checksum mismatch", ErrIntegrity, a.name)
	}
	return f.Sync()
}

func secureURL(u *url.URL) bool {
	return u.Scheme == "https" && u.Host != "" && u.User == nil
}

// Network errors can contain signed CDN URLs; retain their cause without printing it.
type downloadError struct{ cause error }

func (e downloadError) Error() string        { return ErrDownload.Error() }
func (e downloadError) Unwrap() error        { return e.cause }
func (e downloadError) Is(target error) bool { return target == ErrDownload }

// transient marks a download failure worth retrying.
type transient struct{ error }

func (t transient) Unwrap() error { return t.error }

// DownloadAttempts bounds tries per artifact, including the first.
const DownloadAttempts = 4

const (
	// stallTimeout fails a download only when no bytes arrive for this long,
	// so slow connections can finish large files.
	stallTimeout = 60 * time.Second
	partSuffix   = ".part"
)

var (
	errStalled        = errors.New("download stalled")
	errUnsafeRedirect = errors.New("unsafe or excessive download redirect")
)

// download leaves a verified copy of a in root. A partial download is resumed
// with an HTTP range request; transient failures are retried with backoff.
// The partial file is kept on failure so a later run can resume it.
func (m Manager) download(ctx context.Context, root *os.Root, a artifact) error {
	if verifyAsset(ctx, root, a) == nil {
		m.report(Progress{Step: StepDownload, Title: assetTitle(a.name), Source: a.url, Done: a.size, Total: a.size})
		return nil
	}
	if err := root.Remove(a.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := m.downloadOnce(ctx, root, a, attempt)
		var retry transient
		if err == nil || !errors.As(err, &retry) || attempt == DownloadAttempts || ctx.Err() != nil {
			return err
		}
		delay := time.Duration(1<<(attempt-1)) * time.Second
		if m.retryDelay != nil {
			delay = m.retryDelay(attempt)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m Manager) downloadOnce(ctx context.Context, root *os.Root, a artifact, attempt int) error {
	part := a.name + partSuffix
	offset := int64(0)
	if stat, err := root.Lstat(part); err == nil && stat.Mode().IsRegular() && stat.Size() < a.size {
		offset = stat.Size()
	} else if err == nil {
		if err := root.Remove(part); err != nil {
			return err
		}
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := stallTimeout
	if m.stallTimeout > 0 {
		idle = m.stallTimeout
	}
	timer := time.AfterFunc(idle, func() { cancel(errStalled) })
	defer timer.Stop()
	stalled := func(err error) error {
		if ctx.Err() == nil && errors.Is(context.Cause(reqCtx), errStalled) {
			return transient{downloadError{errStalled}}
		}
		return err
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, a.url, nil)
	if err != nil {
		return downloadError{err}
	}
	if !secureURL(req.URL) {
		return errors.New("artifact URL must use HTTPS without credentials")
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	client := http.Client{}
	if m.Client != nil {
		client = *m.Client
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !secureURL(req.URL) || len(via) >= 10 {
			return errUnsafeRedirect
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errUnsafeRedirect) {
			return downloadError{err}
		}
		return stalled(transient{downloadError{err}})
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusPartialContent && offset > 0 && response.Header.Get("Content-Range") == fmt.Sprintf("bytes %d-%d/%d", offset, a.size-1, a.size):
	case response.StatusCode == http.StatusOK:
		offset = 0 // The server ignored the range request; start over.
	case response.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		root.Remove(part)
		return transient{fmt.Errorf("%w: partial download no longer matches the artifact", ErrDownload)}
	default:
		err := fmt.Errorf("%w: artifact download returned HTTP %d", ErrDownload, response.StatusCode)
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout {
			return transient{err}
		}
		return err
	}
	if response.ContentLength >= 0 && response.ContentLength != a.size-offset {
		root.Remove(part)
		return transient{fmt.Errorf("%w: unexpected download length", ErrIntegrity)}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := root.OpenFile(part, flags, 0600)
	if err != nil {
		return err
	}
	var body io.Reader = stallReader{response.Body, timer, idle}
	if m.OnProgress != nil {
		event := Progress{Step: StepDownload, Title: assetTitle(a.name), Source: a.url, Done: offset, Total: a.size, Attempt: attempt}
		m.OnProgress(event)
		body = &countingReader{r: body, done: offset, event: event, emit: m.OnProgress}
	}
	written, copyErr := io.Copy(f, io.LimitReader(contextReader{ctx, body}, a.size-offset+1))
	if err := errors.Join(copyErr, f.Sync(), f.Close()); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return stalled(transient{downloadError{err}})
	}
	switch got := offset + written; {
	case got > a.size:
		root.Remove(part)
		return transient{fmt.Errorf("%w: unexpected artifact size", ErrIntegrity)}
	case got < a.size:
		return transient{downloadError{io.ErrUnexpectedEOF}}
	}
	if err := verifyFile(ctx, root, artifact{name: part, sha256: a.sha256, size: a.size}, 0600); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		root.Remove(part)
		return transient{fmt.Errorf("save %s: %w", a.name, err)}
	}
	return root.Rename(part, a.name)
}

// stallReader postpones the stall timeout whenever bytes arrive.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (s stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.idle)
	}
	return n, err
}
