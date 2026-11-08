package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/media"
)

func TestNativePoolClipOwnership(t *testing.T) {
	if os.Getenv("TURNBEAM_TEST_NATIVE_MEDIA") != "1" {
		t.Skip("set TURNBEAM_TEST_NATIVE_MEDIA=1 for generated-media extraction with local HTTPS mocks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	input := filepath.Join(t.TempDir(), "source.wav")
	if output, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-f", "lavfi", "-i", "anoisesrc=r=24000:d=4:seed=123", "-c:a", "pcm_s16le", input).CombinedOutput(); err != nil {
		t.Fatalf("generate audio: %v\n%s", err, output)
	}
	recording, err := (media.Processor{TempDir: t.TempDir()}).Prepare(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	defer recording.Close()
	plan := make([]chunking.Chunk, 4)
	for i := range plan {
		plan[i] = chunking.Chunk{ID: fmt.Sprint("chunk_", i), StartSample: int64(i) * media.SampleRate, EndSample: int64(i+1) * media.SampleRate}
	}
	for _, status := range []int{200, 413} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			dir := t.TempDir()
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) > 2 {
					t.Error("temporary audio exceeded worker bound")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, success)
				}
			})
			p := Pool{Concurrency: 2, Extractor: media.Processor{TempDir: dir}, Transcriber: c}
			results, err := p.Run(ctx, recording, plan, "test-key", Diarization)
			if status == 200 && (err != nil || len(results) != 4) || status != 200 && (results != nil || !errors.Is(err, ErrHTTP)) {
				t.Fatalf("unexpected run: %v", err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatal("owned clips remain after worker exit")
			}
			if _, err := os.Stat(recording.Path); err != nil {
				t.Fatal("worker removed original prepared recording")
			}
		})
	}
}
