package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/subprocess"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type measuredDiarizer struct {
	adapter                         diarization.Adapter
	elapsed                         time.Duration
	segments, speakers, adjustments int
}

func (m *measuredDiarizer) Diarize(ctx context.Context, input *media.Prepared) (*diarization.Result, error) {
	start := time.Now()
	result, err := m.adapter.Diarize(ctx, input)
	m.elapsed = time.Since(start)
	if result != nil {
		m.segments, m.speakers, m.adjustments = len(result.Timeline.Segments), len(result.Timeline.Speakers), len(result.Timeline.Adjustments)
	}
	return result, err
}

// This opt-in runs real local media/Metal work but always uses a local HTTP mock.
// No API credentials, network transcription or private fixture are required.
func TestNativePipeline(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_PIPELINE") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_PIPELINE=1 for local media/Metal with mocked transcription")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	input := os.Getenv("TURNBEAM_NATIVE_AUDIO")
	if input == "" {
		wav := filepath.Join(t.TempDir(), "silence.wav")
		if err := generatedWAV(wav, 2*media.SampleRate, 0); err != nil {
			t.Fatal(err)
		}
		input = filepath.Join(t.TempDir(), "short.mp4")
		cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=32x32:r=10:d=2", "-i", wav,
			"-map", "0:v:0", "-map", "1:a:0", "-c:v", "mpeg4", "-c:a", "aac", "-t", "2", input)
		if err := subprocess.Run(ctx, cmd); err != nil {
			t.Fatal("generate short MP4", err)
		}
	}
	mediaDir, nativeDir, dir, cache := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "jobs")
	native := &measuredDiarizer{adapter: diarization.Adapter{InstallDir: os.Getenv("TURNBEAM_NATIVE_INSTALL_DIR"), TempDir: nativeDir}}
	var requests, uploaded atomic.Int64
	client := localMeta(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n, err := io.Copy(io.Discard, r.Body)
		uploaded.Add(n)
		if err != nil || n != r.ContentLength || n >= media.MaxRequestBytes {
			t.Error("invalid native upload size", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// Known synthetic text with no invented timing or speaker identity. These
		// fixture turns measure integration only, never ASR/attribution accuracy.
		json.NewEncoder(w).Encode(meta.Response{SessionID: "local-native-fixture", Transcript: "Mock service text.", Turns: []transcript.RawTurn{{Transcript: "Mock service text."}}})
	})
	runner := Runner{Media: media.Processor{TempDir: mediaDir}, Diarizer: native, Credentials: &fakeCredentials{}, Pool: meta.Pool{Transcriber: client}, CacheDir: cache}
	var stdout, stderr bytes.Buffer
	start := time.Now()
	if err := runner.Run(ctx, input, Options{Output: filepath.Join(dir, "meeting.md")}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	doc := readCanonical(t, filepath.Join(dir, "meeting.transcript.json"))
	if stdout.Len() != 0 || len(doc.SourceChunks) == 0 || int64(len(doc.SourceChunks)) != requests.Load() || len(doc.Turns) != len(doc.SourceChunks) || doc.Pipeline.RuntimeVersion != setup.RuntimeVersion || doc.Pipeline.DiarizationRevision != setup.ModelRevision {
		t.Fatal("native pipeline lost output/provenance or repeated a request")
	}
	for _, turn := range doc.Turns {
		if turn.SpeakerID != nil || turn.StartMS != turn.EndMS || turn.Attribution.Reason != transcript.InsufficientTiming {
			t.Fatal("fixture's unavailable timing acquired invented attribution")
		}
	}
	for _, path := range []string{mediaDir, nativeDir} {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatal("native pipeline left owned temporary data", entries, err)
		}
	}
	guard := errors.New("completed resume must not invoke inference or credentials")
	cached := Runner{Media: &meetingMedia{fail: guard}, Diarizer: &fakeDiarizer{fail: guard}, Credentials: &fakeCredentials{fail: guard}, CacheDir: cache}
	stdout.Reset()
	resumeStart := time.Now()
	if err := cached.Run(ctx, input, Options{Resume: true, Render: transcript.RenderOptions{Format: "json"}}, &stdout, io.Discard); err != nil {
		t.Fatal("native resume repeated processing", err)
	}
	resumeElapsed := time.Since(resumeStart)
	again, err := transcript.Decode(stdout.Bytes())
	if err != nil || !reflect.DeepEqual(doc, again) {
		t.Fatal("native recovery changed canonical data", err)
	}
	if len(doc.Speakers) > 0 {
		if err := output.Rename(ctx, filepath.Join(dir, "meeting.transcript.json"), map[int]string{1: "Example One"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Export(ctx, filepath.Join(dir, "meeting.transcript.json"), filepath.Join(dir, "meeting.txt"), transcript.RenderOptions{Format: "text"}, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != int64(len(doc.SourceChunks)) {
		t.Fatal("offline operations triggered another request")
	}
	t.Logf("real local media/Metal; MOCK Meta only: audio_ms=%d wall=%s native_stage=%s real_time_factor=%.6f resume=%s chunks=%d uploaded_bytes=%d speakers=%d intervals=%d end_adjustments=%d runtime=%s model_revision=%s",
		doc.Recording.DurationMS, elapsed, native.elapsed, elapsed.Seconds()/(float64(doc.Recording.DurationMS)/1000), resumeElapsed, len(doc.SourceChunks), uploaded.Load(), native.speakers, native.segments, native.adjustments, setup.RuntimeVersion, setup.ModelRevision)
}
