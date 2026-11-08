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

func (m Manager) download(ctx context.Context, root *os.Root, a artifact) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return downloadError{err}
	}
	if !secureURL(req.URL) {
		return errors.New("artifact URL must use HTTPS without credentials")
	}
	client := http.Client{Timeout: 15 * time.Minute}
	if m.Client != nil {
		client = *m.Client
	}
	if client.Timeout == 0 {
		client.Timeout = 15 * time.Minute
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !secureURL(req.URL) || len(via) >= 10 {
			return errors.New("unsafe or excessive download redirect")
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return downloadError{err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: artifact download returned HTTP %d", ErrDownload, response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != a.size {
		return fmt.Errorf("%w: unexpected download length", ErrIntegrity)
	}
	var body io.Reader = response.Body
	if m.OnProgress != nil {
		event := Progress{Step: StepDownload, Title: assetTitle(a.name), Source: a.url, Total: a.size}
		m.OnProgress(event)
		body = &countingReader{r: body, event: event, emit: m.OnProgress}
	}
	if err := saveAsset(ctx, root, a, body); err != nil {
		return fmt.Errorf("save %s: %w", a.name, err)
	}
	return nil
}
