package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNativeExtraction(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_MEDIA") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_MEDIA=1 to validate installed FFmpeg/FFprobe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	input := filepath.Join(t.TempDir(), "source.wav")
	writeWAV(t, input, SampleRate, 1, false)
	p := Processor{TempDir: t.TempDir()}
	source, err := p.Prepare(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	original := readPCM(t, source.Path)
	for _, r := range [][2]int64{{0, 1}, {1, 24001}, {24001, source.Samples}, {source.Samples - 1, source.Samples}, {12000, 36001}} {
		clip, err := p.Extract(ctx, source, r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(readPCM(t, clip.Path), original[r[0]:r[1]]) || clip.StartSample != r[0] || clip.EndSample != r[1] {
			t.Fatalf("PCM or exact offsets changed for %v", r)
		}
		if clip.Bytes >= MaxRequestBytes-RequestHeadroom {
			t.Fatal("clip exceeds reserved upload budget")
		}
		if err := clip.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(readPCM(t, source.Path), original) {
		t.Fatal("source modified by extraction")
	}
}
