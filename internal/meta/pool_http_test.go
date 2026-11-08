package meta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/media"
)

func TestPoolWithStreamingHTTPRetries(t *testing.T) {
	p, recording, plan := poolFixture(t, 3)
	base := p.Extractor
	p.Extractor = extractFunc(func(ctx context.Context, r *media.Prepared, start, end int64) (*media.Clip, error) {
		clip, err := base.Extract(ctx, r, start, end)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(clip.Path, os.O_WRONLY, 0600)
		if err != nil {
			return clip, err
		}
		_, err = f.WriteAt([]byte{byte(start / media.SampleRate)}, 44)
		f.Close()
		return clip, err
	})
	var active, peak, waits atomic.Int64
	var mu sync.Mutex
	counts, hashes := map[int]int{}, map[int]string{}
	started := make(chan int, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if _, err := mr.NextPart(); err != nil {
			t.Error(err)
			return
		}
		audio, err := mr.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		var prefix [45]byte
		if _, err := io.ReadFull(audio, prefix[:]); err != nil {
			t.Error(err)
			return
		}
		id := int(prefix[44])
		h := sha256.New()
		h.Write(prefix[:])
		if _, err := io.Copy(h, audio); err != nil {
			t.Error(err)
			return
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Error("multipart not complete")
			return
		}
		mu.Lock()
		counts[id]++
		attempt := counts[id]
		sum := hex.EncodeToString(h.Sum(nil))
		if old := hashes[id]; old != "" && old != sum {
			t.Error("retry did not replay the same WAV")
		}
		hashes[id] = sum
		mu.Unlock()
		if attempt == 1 {
			started <- id
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			if id == 0 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(429)
				return
			}
			if id == 1 {
				w.WriteHeader(500)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, strings.Replace(success, "local-session", fmt.Sprintf("chunk-%d", id), 1))
	})
	c.Timeout = 10 * time.Second
	p.Transcriber = c
	p.backoff = func(int) time.Duration { return time.Second }
	p.wait = func(ctx context.Context, delay time.Duration) error {
		if delay != time.Second && delay != 2*time.Second {
			t.Error("wrong retry delay")
		}
		waits.Add(1)
		return ctx.Err()
	}
	type answer struct {
		results []Result
		err     error
	}
	done := make(chan answer, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		results, err := p.Run(ctx, recording, plan, "test-key", Diarization)
		done <- answer{results, err}
	}()
	for range 3 {
		receive(t, started)
	}
	if peak.Load() != 3 {
		t.Error("default HTTP concurrency was not three")
	}
	releaseOnce.Do(func() { close(release) })
	got := receive(t, done)
	if got.err != nil || len(got.results) != 3 || waits.Load() != 2 || active.Load() != 0 {
		t.Fatalf("HTTP pool did not finish: %v", got.err)
	}
	for i, r := range got.results {
		wantAttempts := 2
		if i == 2 {
			wantAttempts = 1
		}
		if r.Chunk != plan[i] || r.Response.SessionID != fmt.Sprintf("chunk-%d", i) || r.Attempts != wantAttempts || *r.Response.Turns[0].Speaker != "A" {
			t.Fatal("response/order/local label association changed")
		}
	}
}
