//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/testutil"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type signalExtractor struct{ path string }

func (s signalExtractor) Extract(_ context.Context, _ *media.Prepared, start, end int64) (*media.Clip, error) {
	return &media.Clip{Prepared: &media.Prepared{Path: s.path, Samples: end - start, Duration: time.Second}, StartSample: start, EndSample: end}, nil
}

type signalTranscriber struct{ starts []int64 }

func (s *signalTranscriber) Transcribe(_ context.Context, c *media.Clip, _, _ string) (*meta.Response, error) {
	s.starts = append(s.starts, c.StartSample)
	return &meta.Response{SessionID: "remaining-session", Transcript: "Remaining.", AudioDurationMS: 1000, Turns: []transcript.RawTurn{{StartMS: 200, EndMS: 800, Transcript: "Remaining."}}}, nil
}

// Test-only dispatch supplies local tools and a local HTTP service. Production
// has no endpoint/environment override. The app helper executes the real run().
func TestMain(m *testing.M) {
	if os.Getenv("TURNBEAM_SIGNAL_HELPER") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "ffprobe", "ffmpeg":
			os.Exit(signalTool())
		}
		if target := os.Getenv("TURNBEAM_SIGNAL_HTTP"); target != "" {
			endpoint, err := url.Parse(target)
			if err != nil {
				os.Exit(9)
			}
			transport := http.DefaultTransport
			http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.meta.ai" {
					return nil, errors.New("unexpected test destination")
				}
				copy := r.Clone(r.Context())
				copy.URL.Scheme, copy.URL.Host = endpoint.Scheme, endpoint.Host
				copy.Host = endpoint.Host
				return transport.RoundTrip(copy)
			})
		}
		os.Exit(run())
	}
	os.Exit(m.Run())
}

func signalTool() int {
	if os.Getenv("META_API_KEY") != "" {
		return 8
	}
	last := os.Args[len(os.Args)-1]
	fmt.Fprintln(os.Stderr, "private native diagnostic text")
	if filepath.Base(os.Args[0]) == "ffprobe" {
		stat, err := os.Stat(last)
		if err != nil {
			return 7
		}
		samples := (stat.Size() - 44) / 2
		fmt.Printf(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"pcm_s16le","sample_rate":"24000","channels":1,"bits_per_sample":16,"sample_fmt":"s16","time_base":"1/24000","duration_ts":%d,"start_time":"0","duration":"%.6f"}],"format":{"format_name":"wav","start_time":"0","duration":"%.6f"}}`, samples, float64(samples)/24000, float64(samples)/24000)
		return 0
	}
	samples := int64(48000)
	for i, arg := range os.Args {
		if arg == "-af" && i+1 < len(os.Args) && strings.HasPrefix(os.Args[i+1], "atrim=") {
			var start, end int64
			if _, err := fmt.Sscanf(os.Args[i+1], "atrim=start_sample=%d:end_sample=%d,", &start, &end); err != nil {
				return 6
			}
			samples = end - start
		}
	}
	if err := os.WriteFile(last, make([]byte, 44+samples*2), 0600); err != nil {
		return 5
	}
	if os.Getenv("TURNBEAM_SIGNAL_STAGE") == "media" {
		if err := os.WriteFile(os.Getenv("TURNBEAM_SIGNAL_READY"), []byte("ready"), 0600); err != nil {
			return 4
		}
		time.Sleep(time.Hour)
	}
	return 0
}

func testSignals(t *testing.T, parent context.Context, binary string) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		for _, stage := range []string{"media", "http"} {
			t.Run(fmt.Sprint(signal, "/", stage), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(parent, 20*time.Second)
				defer cancel()
				dir, home, temp, out := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				input := filepath.Join(dir, "recording.wav")
				if err := os.WriteFile(input, make([]byte, 44+48000*2), 0600); err != nil {
					t.Fatal(err)
				}
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				for _, tool := range []string{"ffmpeg", "ffprobe"} {
					if err := os.Symlink(self, filepath.Join(bin, tool)); err != nil {
						t.Fatal(err)
					}
				}
				cache := filepath.Join(home, "cache", "turnbeam", "jobs")
				if runtime.GOOS == "darwin" {
					cache = filepath.Join(home, "Library", "Caches", "turnbeam", "jobs")
				}
				ready := filepath.Join(dir, "ready")
				args := []string{input, "--output", filepath.Join(out, "meeting.md")}
				app := binary
				serverURL := ""
				var calls atomic.Int32
				stopped := make(chan struct{})
				if stage == "http" {
					seedSignalCheckpoint(t, input, cache)
					args = append(args, "--resume")
					app = self
					release := make(chan struct{})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						io.Copy(io.Discard, r.Body)
						os.WriteFile(ready, []byte("ready"), 0600)
						select {
						case <-r.Context().Done():
						case <-release:
						}
						close(stopped)
					}))
					defer server.Close()
					defer close(release)
					serverURL = server.URL
				}
				cmd := exec.CommandContext(ctx, app, args...)
				cmd.Env = []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "TMPDIR=" + temp, "PATH=" + bin,
					"META_API_KEY=fake-private-signal-key", "TURNBEAM_SIGNAL_HELPER=1", "TURNBEAM_SIGNAL_STAGE=" + stage,
					"TURNBEAM_SIGNAL_READY=" + ready, "TURNBEAM_SIGNAL_HTTP=" + serverURL, "GORACE=atexit_sleep_ms=0"}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				testutil.WaitReady(t, ready, done, cancel)
				if err := cmd.Process.Signal(signal); err != nil {
					t.Fatal(err)
				}
				err = testutil.WaitError(t, done)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 130 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Error: context canceled") || strings.Contains(stderr.String(), "fake-private-signal-key") || strings.Contains(stderr.String(), "private native") {
					t.Fatal("signal did not cancel safely", err, stderr.String())
				}
				for _, path := range []string{temp, out} {
					entries, err := os.ReadDir(path)
					if err != nil || len(entries) != 0 {
						t.Fatal("signal left temporary media or completed-looking output", entries, err)
					}
				}
				if stage == "http" {
					select {
					case <-stopped:
					case <-ctx.Done():
						t.Fatal("HTTP request did not cancel")
					}
					if calls.Load() != 1 {
						t.Fatal("cancellation retried the request")
					}
				}
				j, err := jobs.Open(ctx, input, jobs.DefaultSettings(meta.Diarization), jobs.Options{Dir: cache, Resume: true})
				if err != nil {
					t.Fatal("signal damaged recovery", err)
				}
				defer j.Close()
				if stage == "http" {
					clip := filepath.Join(t.TempDir(), "remaining.wav")
					if err := os.WriteFile(clip, make([]byte, 44+24000*2), 0600); err != nil {
						t.Fatal(err)
					}
					transcriber := &signalTranscriber{}
					rows, err := j.Transcribe(ctx, meta.Pool{Extractor: signalExtractor{clip}, Transcriber: transcriber}, &media.Prepared{Path: input, Samples: 48000, Duration: 2 * time.Second}, "fake-private-signal-key")
					if err != nil || len(rows) != 2 || len(transcriber.starts) != 1 || transcriber.starts[0] != 24000 || rows[0].Response.Transcript != "Saved." {
						t.Fatal("signal lost or fabricated a committed chunk", err)
					}
				}
			})
		}
	}
}

func seedSignalCheckpoint(t *testing.T, input, cache string) {
	t.Helper()
	ctx := context.Background()
	j, err := jobs.Open(ctx, input, jobs.DefaultSettings(meta.Diarization), jobs.Options{Dir: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.CheckMedia(ctx, &media.Prepared{Path: input, Samples: 48000, Duration: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "timeline.rttm")
	if err = os.WriteFile(raw, []byte("SPEAKER recording 1 0.000 2.000 <NA> <NA> A <NA> <NA>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = j.SaveDiarization(ctx, &diarization.Result{RTTMPath: raw, RuntimeVersion: setup.RuntimeVersion, ModelRevision: setup.ModelRevision}); err != nil {
		t.Fatal(err)
	}
	plan := []chunking.Chunk{{ID: "one", EndSample: 24000, Boundary: chunking.Pause}, {ID: "two", StartSample: 24000, EndSample: 48000, Boundary: chunking.End}}
	if err = j.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(make([]byte, 44+24000*2))
	if err = j.SaveResult(ctx, meta.Result{Chunk: plan[0], Attempts: 1, AudioSHA256: hex.EncodeToString(hash[:]), Response: &meta.Response{SessionID: "saved-session", Transcript: "Saved.", AudioDurationMS: 1000, Turns: []transcript.RawTurn{{StartMS: 200, EndMS: 800, Transcript: "Saved."}}}}); err != nil {
		t.Fatal(err)
	}
}
