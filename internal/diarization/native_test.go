package diarization

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

func TestNativeDiarization(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_DIARIZATION") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_DIARIZATION=1 to use the installed native runtime and FFmpeg")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	input := os.Getenv("TURNBEAM_NATIVE_AUDIO")
	if input == "" {
		input = filepath.Join(t.TempDir(), "silence.wav")
		writeSilence(t, input)
	}
	recording, err := (media.Processor{TempDir: t.TempDir()}).Prepare(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	defer recording.Close()
	a := Adapter{InstallDir: os.Getenv("TURNBEAM_NATIVE_INSTALL_DIR"), TempDir: t.TempDir()}
	if path := os.Getenv("TURNBEAM_NATIVE_TIME"); path != "" {
		if runtime.GOOS != "darwin" {
			t.Fatal("native resource measurement requires macOS /usr/bin/time")
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		// Measure the native process itself, not the Go test parent or an
		// aggregate falsely labeled as total unified-memory usage.
		a.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/usr/bin/time", append([]string{"-l", "-o", path, name}, args...)...)
		}
	}
	result, err := a.Diarize(ctx, recording)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	data, err := os.ReadFile(result.RTTMPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseRTTM(ctx, strings.NewReader(string(data)), recordingID, recording.Duration)
	if err != nil || !reflect.DeepEqual(again, result.Timeline) {
		t.Fatalf("raw output did not reproduce the timeline: %v", err)
	}
	for _, segment := range result.Timeline.Segments {
		if segment.Start < 0 || segment.Start >= segment.End || segment.End > recording.Duration {
			t.Fatal("native interval outside the recording")
		}
	}
	stat, err := os.Stat(result.RTTMPath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("native output is not private: %v", err)
	}
	if os.Getenv("TURNBEAM_NATIVE_AUDIO") == "" && len(result.Timeline.Segments) != 0 {
		t.Fatal("digital silence produced native speaker activity")
	}
	// This checks recording-wide ID continuity, not real-world identity accuracy.
	first := map[string]time.Duration{}
	last := map[string]time.Duration{}
	for _, segment := range result.Timeline.Segments {
		if _, exists := first[segment.SpeakerID]; !exists {
			first[segment.SpeakerID] = segment.Start
		}
		last[segment.SpeakerID] = segment.End
	}
	spansRemoteBoundaries := 0
	for id, start := range first {
		if last[id]-start > 9*time.Minute {
			spansRemoteBoundaries++
		}
	}
	t.Logf("duration=%v, speakers=%d, intervals=%d, explicit end adjustments=%d, labels spanning >9m=%d",
		recording.Duration, len(result.Timeline.Speakers), len(result.Timeline.Segments), len(result.Timeline.Adjustments), spansRemoteBoundaries)
}

func writeSilence(t *testing.T, path string) {
	t.Helper()
	const samples = 2 * media.SampleRate
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], media.SampleRate)
	binary.LittleEndian.PutUint32(data[28:], media.SampleRate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], samples*2)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
