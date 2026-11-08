// Package meta transcribes verified audio clips without assigning global speakers.
package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

const (
	Model            = "muse-voice-transcribe-1.0"
	Diarization      = "DIARIZATION"
	Endpointing      = "ENDPOINTING"
	endpoint         = "https://api.meta.ai/v1/asr/transcribe"
	maxResponseBytes = 4 << 20
)

var (
	ErrRequest   = errors.New("invalid transcription request")
	ErrResponse  = errors.New("invalid transcription response")
	ErrTransport = errors.New("transcription connection or deadline failed")
	ErrHTTP      = errors.New("transcription service rejected the request")
)

// Error exposes status, bounded taxonomy and retry guidance, never an upstream
// message/body, authorization header, URL or local path. Causes remain inspectable.
type Error struct {
	Kind                         error
	StatusCode                   int
	SessionID, Type, Code, Param string
	RetryAfter                   time.Duration
	cause                        error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s (HTTP %d)", ErrHTTP, e.StatusCode)
	}
	return fmt.Sprintf("Meta: %v", e.Kind)
}
func (e *Error) Unwrap() error { return errors.Join(e.Kind, e.cause) }

// Client is reusable across workers when its configuration is not mutated.
// Zero values select the pinned model and a five-minute per-attempt deadline.
// HTTP supplies a trusted transport; redirects and cookies are always disabled.
type Client struct {
	HTTP     *http.Client
	Model    string
	Timeout  time.Duration
	endpoint string
}

// Transcribe makes one attempt. The caller owns clip lifetime and must not modify
// it during upload. Credentials are injected per call and never retained in Client.
func (c Client) Transcribe(ctx context.Context, clip *media.Clip, apiKey, mode string) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode == "" {
		mode = Diarization
	}
	model := c.Model
	if model == "" {
		model = Model
	}
	if mode != Diarization && mode != Endpointing || !token(apiKey, 8192) || !token(model, 256) || c.Timeout < 0 {
		return nil, &Error{Kind: ErrRequest}
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, contentType, length, err := upload(clip, model, mode)
	if err != nil {
		return nil, &Error{Kind: ErrRequest, cause: err}
	}
	defer body.Close()
	url := c.endpoint
	if url == "" {
		url = endpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, &Error{Kind: ErrRequest, cause: err}
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrTransport, cause: errors.Join(ctx.Err(), err)}
	}
	defer res.Body.Close()
	limit := int64(maxResponseBytes)
	if res.StatusCode != http.StatusOK {
		limit = 64 << 10
	}
	data, readErr := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if res.StatusCode != http.StatusOK {
		e := &Error{Kind: ErrHTTP, StatusCode: res.StatusCode, RetryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now()), cause: errors.Join(ctx.Err(), readErr)}
		if readErr == nil && int64(len(data)) <= limit {
			e.readTaxonomy(data, apiKey)
		}
		return nil, e
	}
	if readErr != nil {
		return nil, &Error{Kind: ErrTransport, cause: errors.Join(ctx.Err(), readErr)}
	}
	if body.read.Load() != length {
		return nil, &Error{Kind: ErrTransport, cause: io.ErrUnexpectedEOF}
	}
	mediaType, _, mimeErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if int64(len(data)) > limit || mimeErr != nil || mediaType != "application/json" {
		return nil, &Error{Kind: ErrResponse}
	}
	response, err := decodeResponse(data, clip.Duration.Milliseconds())
	if err != nil {
		return nil, &Error{Kind: ErrResponse, cause: err}
	}
	if response.hasSecret(apiKey) {
		return nil, &Error{Kind: ErrResponse}
	}
	if err := ctx.Err(); err != nil {
		return nil, &Error{Kind: ErrTransport, cause: err}
	}
	return response, nil
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds >= 0 && seconds <= (1<<63-1)/int64(time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}

func token(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
