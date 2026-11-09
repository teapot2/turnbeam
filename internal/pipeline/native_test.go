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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/asr"
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

// nativeSpeech is synthesized with macOS `say`, so the test needs no recording.
const nativeSpeech = "Thanks for joining the meeting. Today we will review the budget and the schedule for next quarter."

// This opt-in runs every installed local model through the real pipeline:
// media, transcription and diarization all run on device and nothing is mocked.
func TestNativeLocalModels(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_PIPELINE") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_PIPELINE=1 to run installed local models on synthesized speech")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	installDir := os.Getenv("TURNBEAM_NATIVE_INSTALL_DIR")
	statuses, err := (setup.Manager{Dir: installDir}).LocalStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := os.Getenv("TURNBEAM_NATIVE_AUDIO")
	if input == "" {
		input = filepath.Join(t.TempDir(), "speech.aiff")
		if err := subprocess.Run(ctx, exec.CommandContext(ctx, "say", "-o", input, nativeSpeech)); err != nil {
			t.Fatal("synthesize speech with say", err)
		}
	}
	ran := 0
	for _, status := range statuses {
		if !status.Installed {
			t.Logf("%s: not installed; run `turnbeam setup --model %s` to include it", status.Model.Name, status.Model.Name)
			continue
		}
		ran++
		t.Run(status.Model.Name, func(t *testing.T) {
			model := status.Model
			mediaDir, asrDir, diarDir, dir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			cache := filepath.Join(t.TempDir(), "jobs")
			guard := errors.New("local models must not need a Meta key")
			runner := Runner{
				Media:       media.Processor{TempDir: mediaDir},
				Transcriber: asr.Adapter{InstallDir: installDir, TempDir: asrDir},
				Diarizer:    diarization.Adapter{InstallDir: installDir, TempDir: diarDir},
				Credentials: &fakeCredentials{fail: guard},
				CacheDir:    cache,
			}
			var stdout, stderr bytes.Buffer
			start := time.Now()
			if err := runner.Run(ctx, input, Options{Model: model.Name, Output: filepath.Join(dir, "meeting.md"), InstallDir: installDir}, &stdout, &stderr); err != nil {
				t.Fatal(err, stderr.String())
			}
			elapsed := time.Since(start)
			doc := readCanonical(t, filepath.Join(dir, "meeting.transcript.json"))
			p := doc.Pipeline
			if stdout.Len() != 0 || p.Engine != transcript.EngineLocal || p.ASRModel != model.File || p.ASRRevision != model.Revision || p.MetaModel != "" || len(doc.SourceChunks) != 0 {
				t.Fatalf("local provenance or output contract broken: %+v", p)
			}
			var text []string
			for _, turn := range doc.Turns {
				text = append(text, turn.Text)
			}
			spoken := strings.ToLower(strings.Join(text, " "))
			heard := 0
			for _, word := range []string{"meeting", "budget", "schedule", "quarter"} {
				if strings.Contains(spoken, word) {
					heard++
				}
			}
			// Synthesized speech is clear; most key words should come through.
			if heard < 3 {
				t.Fatalf("recognized too little of the synthesized speech: %q", spoken)
			}
			if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("local run wrote a checkpoint")
			}
			for _, path := range []string{mediaDir, asrDir, diarDir} {
				if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
					t.Fatal("local run left owned temporary data", entries, err)
				}
			}
			if len(doc.Speakers) > 0 {
				if err := output.Rename(ctx, filepath.Join(dir, "meeting.transcript.json"), map[int]string{1: "Example One"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := output.Export(ctx, filepath.Join(dir, "meeting.transcript.json"), filepath.Join(dir, "meeting.srt"), transcript.RenderOptions{Format: "srt"}, nil); err != nil {
				t.Fatal(err)
			}
			unknown := 0
			for _, turn := range doc.Turns {
				if turn.SpeakerID == nil {
					unknown++
				}
			}
			t.Logf("real local model %s (latency correction %s): audio_ms=%d wall=%s turns=%d unknown=%d speakers=%d text=%q",
				model.Name, model.Latency, doc.Recording.DurationMS, elapsed, len(doc.Turns), unknown, len(doc.Speakers), spoken)
		})
	}
	if ran == 0 {
		t.Fatal("no local model is installed; run `turnbeam setup --model parakeet`")
	}
}
