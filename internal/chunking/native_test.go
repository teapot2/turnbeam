package chunking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
)

func TestNativeChunkPlanning(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_MEDIA") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_MEDIA=1 to validate installed FFmpeg/FFprobe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, mode := range []string{"silent", "continuous", "pause"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recording.wav")
			// A periodic tone could hide an incorrect seek by a whole cycle.
			source := "anoisesrc=color=white:sample_rate=24000:duration=610:seed=12345:amplitude=0.1"
			if mode == "silent" {
				source = "anullsrc=r=24000:cl=mono:d=610"
			}
			args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", "lavfi", "-i", source}
			if mode == "pause" {
				args = append(args, "-af", "volume=enable='between(t,509,511)':volume=0")
			}
			args = append(args, "-c:a", "pcm_s16le", path)
			if output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("generate known waveform: %v\n%s", err, output)
			}
			p := media.Processor{TempDir: t.TempDir()}
			recording, err := p.Prepare(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer recording.Close()
			quiet, err := p.DetectSilence(ctx, recording)
			if err != nil {
				t.Fatal(err)
			}
			timeline := diarization.Timeline{Duration: recording.Duration}
			if mode == "pause" {
				timeline.Segments = []diarization.Segment{speech(0, 509*time.Second), speech(511*time.Second, recording.Duration)}
			}
			if mode == "continuous" {
				timeline.Segments = []diarization.Segment{speech(0, recording.Duration)}
			}
			chunks, err := Plan(ctx, recording.Samples, timeline, quiet)
			if err != nil {
				t.Fatal(err)
			}
			assertCoverage(t, chunks, recording.Samples)
			want, end := Pause, int64(TargetSamples)
			if mode == "continuous" {
				want, end = Forced, MaxChunkSamples
			}
			if len(chunks) != 2 || chunks[0].Boundary != want || chunks[0].EndSample != end {
				t.Fatalf("unexpected known-waveform plan: %+v", chunks)
			}
			verifyExtractedPCM(t, ctx, p, recording, chunks)
			t.Logf("%s: %d silence intervals, first boundary=%dms (%s), next overlap=%d samples", mode, len(quiet), chunks[0].EndMS(), chunks[0].Boundary, chunks[1].OverlapSamples)
		})
	}
}

func verifyExtractedPCM(t *testing.T, ctx context.Context, p media.Processor, recording *media.Prepared, chunks []Chunk) {
	t.Helper()
	// Decode once to a disk-backed reference. SectionReader offsets are independent
	// of the extractor's FFmpeg filter and expose dropped/repeated/shifted samples.
	path := filepath.Join(t.TempDir(), "reference.pcm")
	if output, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", recording.Path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-f", "s16le", path).CombinedOutput(); err != nil {
		t.Fatalf("decode reference: %v\n%s", err, output)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range chunks {
		clip, err := p.Extract(ctx, recording, c.StartSample, c.EndSample)
		if err != nil {
			t.Fatal(err)
		}
		defer clip.Close()
		if clip.StartSample != c.StartSample || clip.EndSample != c.EndSample || clip.Samples != c.EndSample-c.StartSample || clip.Bytes+media.RequestHeadroom >= media.MaxRequestBytes {
			t.Fatalf("extracted metadata differs from plan: %+v", clip)
		}
		want, got := sha256.New(), sha256.New()
		if _, err := io.Copy(want, io.NewSectionReader(f, c.StartSample*2, clip.Samples*2)); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", clip.Path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-f", "s16le", "-")
		cmd.Stdout = got
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want.Sum(nil), got.Sum(nil)) {
			t.Fatalf("PCM lost, shifted or duplicated outside the planned range: %s", c.ID)
		}
		stat, err := os.Stat(clip.Path)
		if err != nil || stat.Size() != clip.Bytes || stat.Mode().Perm() != 0600 {
			t.Fatal("incorrect file size or permissions")
		}
	}
}
