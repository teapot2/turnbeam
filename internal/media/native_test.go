package media

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/testutil"
)

func TestNativeMedia(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_MEDIA") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_MEDIA=1 to validate installed FFmpeg/FFprobe")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "stereo 48k.wav")
	silent := filepath.Join(dir, "silent.wav")
	writeWAV(t, wav, 48000, 2, false)
	writeWAV(t, silent, 48000, 2, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	convert := func(name string, args ...string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		args = append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
		args = append(args, path)
		if output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("create synthetic %s: %v\n%s", name, err, output)
		}
		return path
	}
	paths := []string{
		wav,
		convert("audio.mp3", "-i", wav, "-c:a", "libmp3lame", "-q:a", "4"),
		convert("audio.m4a", "-i", wav, "-c:a", "aac"),
		convert("gap.mkv", "-i", wav, "-af", "aselect='not(between(t,0.5,1.5))'", "-c:a", "pcm_s16le"),
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			p := Processor{TempDir: t.TempDir()}
			prepared, err := p.Prepare(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			stat, err := os.Stat(prepared.Path)
			if err != nil || stat.Mode().Perm() != 0600 {
				t.Fatalf("native normalization changed private permissions: %v", err)
			}
			pcm := readPCM(t, prepared.Path)
			if int64(len(pcm)) != prepared.Samples || prepared.Samples < 48000 || prepared.Samples > 50000 {
				t.Fatalf("wrong decoded duration: %d samples", prepared.Samples)
			}
			assertQuiet(t, pcm, 0.7, 1.3)
			assertActive(t, pcm, 0.1, 0.3)
			assertActive(t, pcm, 1.7, 1.9)
		})
	}

	video := convert("delayed.mp4", "-f", "lavfi", "-i", "color=c=black:s=32x32:r=10:d=3", "-itsoffset", "0.5", "-i", wav,
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "mpeg4", "-c:a", "aac", "-t", "3")
	shifted := convert("nonzero-origin.ts", "-i", video, "-map", "0:v", "-map", "0:a", "-c:v", "mpeg2video", "-c:a", "mp2", "-muxdelay", "0", "-muxpreload", "0", "-output_ts_offset", "5")
	for _, path := range []string{video, shifted} {
		t.Run(filepath.Base(path)+" delay and trailing silence", func(t *testing.T) {
			prepared, err := (Processor{TempDir: t.TempDir()}).Prepare(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			pcm := readPCM(t, prepared.Path)
			if prepared.Samples != 72000 || prepared.Source.AudioOffset < 450*time.Millisecond {
				t.Fatalf("lost recording-relative delay: %+v", prepared)
			}
			assertQuiet(t, pcm, 0.05, 0.4)
			assertActive(t, pcm, 0.6, 0.8)
			assertQuiet(t, pcm, 1.2, 1.8)
			assertQuiet(t, pcm, 2.7, 2.9)
		})
	}

	multi := convert("tracks.mkv", "-i", wav, "-i", silent, "-map", "0:a", "-map", "1:a", "-c:a", "pcm_s16le", "-disposition:a:0", "0", "-disposition:a:1", "default")
	t.Run("default track is not mixed", func(t *testing.T) {
		prepared, err := (Processor{TempDir: t.TempDir()}).Prepare(ctx, multi)
		if err != nil {
			t.Fatal(err)
		}
		defer prepared.Close()
		if prepared.Source.StreamIndex != 1 {
			t.Fatalf("wrong track: %+v", prepared.Source)
		}
		assertQuiet(t, readPCM(t, prepared.Path), 0, 2)
	})

	videoOnly := convert("video-only.mp4", "-f", "lavfi", "-i", "color=c=black:s=32x32:r=10:d=1", "-c:v", "mpeg4")
	t.Run("missing audio", func(t *testing.T) {
		if _, err := (Processor{}).Prepare(ctx, videoOnly); !errors.Is(err, ErrNoAudio) {
			t.Fatalf("no-audio input accepted: %v", err)
		}
	})
	t.Run("corrupt media", func(t *testing.T) {
		if _, err := (Processor{}).Prepare(ctx, inputFile(t)); err == nil {
			t.Fatal("corrupt media accepted")
		}
	})
}

func writeWAV(t *testing.T, path string, rate, channels int, silent bool) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	header := testutil.WAVHeader(rate, channels, int64(rate*2))
	if _, err := f.Write(header); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, channels*2)
	for i := 0; i < rate*2; i++ {
		var sample int16
		if !silent && (i < rate/2 || i >= rate*3/2) {
			sample = int16(5000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		}
		for c := 0; c < channels; c++ {
			binary.LittleEndian.PutUint16(frame[c*2:], uint16(sample))
		}
		if _, err := f.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
}

func readPCM(t *testing.T, path string) []int16 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(12, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var header [8]byte
	for {
		if _, err := io.ReadFull(f, header[:]); err != nil {
			t.Fatal(err)
		}
		size := binary.LittleEndian.Uint32(header[4:])
		if string(header[:4]) == "data" {
			if size > 1<<20 || size%2 != 0 {
				t.Fatal("invalid synthetic PCM size")
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(f, data); err != nil {
				t.Fatal(err)
			}
			pcm := make([]int16, size/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
			}
			return pcm
		}
		if _, err := f.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
}

func rms(pcm []int16, start, end float64) float64 {
	var total float64
	for _, value := range pcm[int(start*SampleRate):int(end*SampleRate)] {
		total += float64(value) * float64(value)
	}
	return math.Sqrt(total / ((end - start) * SampleRate))
}

func assertQuiet(t *testing.T, pcm []int16, start, end float64) {
	t.Helper()
	if value := rms(pcm, start, end); value > 5 {
		t.Fatalf("silence moved/disappeared at %.1f-%.1f: RMS=%.2f", start, end, value)
	}
}

func assertActive(t *testing.T, pcm []int16, start, end float64) {
	t.Helper()
	if value := rms(pcm, start, end); value < 1000 {
		t.Fatalf("speech marker moved/disappeared at %.1f-%.1f: RMS=%.2f", start, end, value)
	}
}
