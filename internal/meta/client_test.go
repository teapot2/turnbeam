package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mockClient(t *testing.T, handler http.HandlerFunc) Client {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	return Client{HTTP: s.Client(), endpoint: s.URL, Timeout: 2 * time.Second}
}

func TestHTTPContractBothModes(t *testing.T) {
	for _, mode := range []string{"", Endpointing} {
		t.Run("mode="+mode, func(t *testing.T) {
			clip := clipFile(t, 24000)
			var attempts atomic.Int64
			wantMode := mode
			if wantMode == "" {
				wantMode = Diarization
			}
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer test-key" || r.ContentLength <= clip.Bytes {
					t.Error("incorrect HTTP contract")
				}
				verifyMultipart(t, r.Body, r.Header.Get("Content-Type"), "custom-model", wantMode, clip.Bytes)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				fmt.Fprint(w, success)
			})
			c.Model = "custom-model"
			r, err := c.Transcribe(context.Background(), clip, "test-key", mode)
			if err != nil || r.AudioDurationMS != 1040 || *r.Turns[0].Speaker != "A" || attempts.Load() != 1 {
				t.Fatalf("contract failed: %+v, %v", r, err)
			}
			if c.HTTP.CheckRedirect != nil {
				t.Fatal("caller HTTP client was mutated")
			}
		})
	}
}

func TestHTTPFailuresAreTypedSanitizedAndNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 413, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int64
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"sessionId":"session-1","error":{"type":"authentication_error","code":"invalid_api_key","param":"audio","message":"test-key and private speech"}}`)
			})
			r, err := c.Transcribe(context.Background(), clipFile(t, 24000), "test-key", Diarization)
			var remote *Error
			if r != nil || !errors.As(err, &remote) || !errors.Is(err, ErrHTTP) || remote.StatusCode != status || remote.RetryAfter != 2*time.Second || remote.Type != "authentication_error" || remote.Code != "invalid_api_key" || remote.Param != "audio" || remote.SessionID != "session-1" || attempts.Load() != 1 {
				t.Fatalf("wrong error classification: %v", err)
			}
			if strings.Contains(fmt.Sprintf("%v %+v", err, err), "test-key") || strings.Contains(err.Error(), "private speech") {
				t.Fatal("remote content leaked")
			}
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var hits atomic.Int64
			destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			defer destination.Close()
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
			})
			_, err := c.Transcribe(context.Background(), clipFile(t, 24000), "test-key", Diarization)
			var remote *Error
			if !errors.As(err, &remote) || remote.StatusCode != status || hits.Load() != 0 {
				t.Fatalf("redirect followed or status lost: %v", err)
			}
		})
	}
}

func TestBadSuccessBodiesAndSecretEchoes(t *testing.T) {
	for _, tc := range []struct{ body, contentType string }{
		{success, "text/plain"}, {"{}", "application/json"}, {strings.Repeat("x", maxResponseBytes+1), "application/json"},
		{strings.ReplaceAll(success, "Hello.", `test-\u006bey`), "application/json"},
	} {
		c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", tc.contentType)
			fmt.Fprint(w, tc.body)
		})
		r, err := c.Transcribe(context.Background(), clipFile(t, 24000), "test-key", Diarization)
		if r != nil || !errors.Is(err, ErrResponse) || strings.Contains(err.Error(), "test-key") {
			t.Fatalf("bad success response accepted: %v", err)
		}
	}
}

func TestMalformedErrorBodiesStillExposeStatus(t *testing.T) {
	for _, body := range []string{"<html>private speech</html>", strings.Repeat("x", 65537), `{"sessionId":"test-key","error":{"type":"test-key","code":"test-key","param":"/private/path","message":"private speech"}}`} {
		c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(503)
			fmt.Fprint(w, body)
		})
		_, err := c.Transcribe(context.Background(), clipFile(t, 24000), "test-key", Diarization)
		var remote *Error
		if !errors.As(err, &remote) || remote.StatusCode != 503 || remote.SessionID != "" || remote.Type != "" || remote.Code != "" || remote.Param != "" {
			t.Fatalf("unsanitized taxonomy or missing status: %v", err)
		}
	}
}

func TestIncompleteResponsePreservesReadError(t *testing.T) {
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(success)+10))
		fmt.Fprint(w, success)
	})
	if r, err := c.Transcribe(context.Background(), clipFile(t, 24000), "test-key", Diarization); r != nil || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, ErrTransport) {
		t.Fatalf("incomplete response accepted: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestValidationAndTransportCauses(t *testing.T) {
	clip := clipFile(t, 24000)
	c := Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid input performed I/O"); return nil, nil })}}
	for _, tc := range []struct{ key, mode string }{{"", Diarization}, {"bad\nkey", Diarization}, {"test-key", "invalid"}} {
		if r, err := c.Transcribe(context.Background(), clip, tc.key, tc.mode); r != nil || !errors.Is(err, ErrRequest) || strings.Contains(err.Error(), tc.key) && tc.key != "" {
			t.Fatal("invalid options accepted/leaked")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := c.Transcribe(ctx, clip, "test-key", Diarization); r != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancellation lost")
	}
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("test-key private URL")}
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Minute {
			t.Error("missing default deadline")
		}
		return nil, cause
	})
	_, err := c.Transcribe(context.Background(), clip, "test-key", Diarization)
	var op *net.OpError
	if !errors.Is(err, ErrTransport) || !errors.As(err, &op) || op != cause || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("transport cause lost/leaked: %v", err)
	}
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(success))}, nil
	})
	if _, err := c.Transcribe(context.Background(), clip, "test-key", Diarization); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("early success without upload accepted")
	}
}

func TestCancellationAndTimeoutDuringRequests(t *testing.T) {
	for _, uploading := range []bool{false, true} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("upload=%t/timeout=%t", uploading, timeout), func(t *testing.T) {
				ready := make(chan struct{})
				release := make(chan struct{})
				defer close(release)
				c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
					if uploading {
						var b [1]byte
						r.Body.Read(b[:])
					} else {
						io.Copy(io.Discard, r.Body)
					}
					close(ready)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				})
				clip := clipFile(t, 24000)
				if uploading {
					clip = clipFile(t, 9*60*24000)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := context.Canceled
				if timeout {
					c.Timeout = 200 * time.Millisecond
					want = context.DeadlineExceeded
				}
				done := make(chan error, 1)
				go func() { _, err := c.Transcribe(ctx, clip, "test-key", Diarization); done <- err }()
				select {
				case <-ready:
				case <-time.After(time.Second):
					t.Fatal("request never reached mock")
				}
				if !timeout {
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("request was not stopped: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("request did not stop")
				}
			})
		}
	}
}

func TestClientConcurrentReuse(t *testing.T) {
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, success)
	})
	clip := clipFile(t, 24000)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Transcribe(context.Background(), clip, "test-key", Diarization); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{{"2", 2 * time.Second}, {"-1", 0}, {"9999999999999999999", 0}, {"nonsense", 0}, {now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second}, {now.Add(-time.Second).Format(http.TimeFormat), 0}} {
		if got := retryAfter(tc.header, now); got != tc.want {
			t.Fatalf("Retry-After %q: %v", tc.header, got)
		}
	}
}
