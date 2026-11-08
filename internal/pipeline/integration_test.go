package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/jobs"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type localTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (l localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.meta.ai" {
		return nil, errors.New("unexpected integration-test destination")
	}
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host, copy.Host = l.target.Scheme, l.target.Host, l.target.Host
	return l.base.RoundTrip(copy)
}

// Only transport routing changes: real multipart streaming, response validation,
// retry policy and checkpoint callbacks run against the local HTTP server.
func localMeta(t *testing.T, handler http.HandlerFunc) meta.Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	target, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := s.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return meta.Client{HTTP: &http.Client{Transport: localTransport{target, base}}, Timeout: 30 * time.Second}
}

// Valid sparse PCM WAVs exercise real upload sizes without large Go allocations.
// The mock extractor's first four PCM samples encode its original sample offset.
func generatedWAV(path string, samples, offset int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	var header [52]byte
	copy(header[:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+samples*2))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], media.SampleRate)
	binary.LittleEndian.PutUint32(header[28:], media.SampleRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(samples*2))
	binary.LittleEndian.PutUint64(header[44:], uint64(offset))
	_, writeErr := f.Write(header[:])
	return errors.Join(writeErr, f.Truncate(44+samples*2), f.Close())
}

type meetingMedia struct {
	input, dir      string
	pause           bool
	prepare, detect int
	fail            error
}

func (m *meetingMedia) Prepare(ctx context.Context, _ string) (*media.Prepared, error) {
	m.prepare++
	if m.fail != nil {
		return nil, m.fail
	}
	return &media.Prepared{Path: m.input, Samples: 960 * media.SampleRate, Duration: 16 * time.Minute}, ctx.Err()
}
func (m *meetingMedia) DetectSilence(ctx context.Context, _ *media.Prepared) ([]media.Silence, error) {
	m.detect++
	if m.pause {
		return []media.Silence{{Start: 509 * time.Second, End: 511 * time.Second}}, ctx.Err()
	}
	return nil, ctx.Err()
}
func (m *meetingMedia) Extract(ctx context.Context, _ *media.Prepared, start, end int64) (*media.Clip, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(m.dir, fmt.Sprintf("%d.wav", start))
	if err := generatedWAV(path, end-start, start); err != nil {
		return nil, err
	}
	return &media.Clip{Prepared: &media.Prepared{Path: path, Samples: end - start, Duration: time.Duration(end-start) * time.Second / media.SampleRate}, StartSample: start, EndSample: end, Bytes: 44 + (end-start)*2}, nil
}

func meetingResponse(startMS, durationMS int64) *meta.Response {
	endMS := startMS + durationMS
	// These are synthetic contract turns, not a reference-derived ASR result.
	abs := []transcript.RawTurn{
		{StartMS: 1000, EndMS: 2000, Transcript: "First participant."},
		{StartMS: 5000, EndMS: 6000, Transcript: "Second participant."},
		{StartMS: 539600, EndMS: 539900, Transcript: "Shared boundary."},
		{StartMS: 650200, EndMS: 651200, Transcript: "Returning participant."},
		{StartMS: 700000, EndMS: 701000, Transcript: "Second returns."},
		{StartMS: 720000, EndMS: 720000, Transcript: "Zero timing."},
		{StartMS: 721000, EndMS: 721100, Transcript: ""},
		{StartMS: 649900, EndMS: 650100, Transcript: "Transition."},
	}
	response := &meta.Response{SessionID: fmt.Sprintf("session-%d", startMS), AudioDurationMS: durationMS, Turns: []transcript.RawTurn{}}
	for i, turn := range abs {
		if turn.StartMS < startMS || turn.EndMS > endMS {
			continue
		}
		turn.TurnID = int64(i)
		turn.StartMS, turn.EndMS = turn.StartMS-startMS, turn.EndMS-startMS
		// Reuse and swap request-local labels; only the global RTTM defines IDs.
		label := "A"
		if startMS != 0 {
			label = "B"
			if turn.Transcript == "Shared boundary." {
				turn.Transcript = "Shared   boundary.\n"
			}
		}
		turn.Speaker = &label
		response.Turns = append(response.Turns, turn)
		response.Transcript += turn.Transcript + " "
	}
	return response
}

type meetingHTTP struct {
	mu     sync.Mutex
	counts map[int64]int
	hashes map[int64]string
	fault  string
	mode   string
	wait   <-chan struct{}
}

func (s *meetingHTTP) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fake-private-key" || r.ContentLength <= 0 || r.ContentLength >= media.MaxRequestBytes {
		t.Error("incorrect HTTP contract or upload limit")
	}
	mr, err := r.MultipartReader()
	if err != nil {
		t.Error(err)
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		t.Error(err)
		return
	}
	var request struct{ Model, Mode, AudioEncoding string }
	if err := json.NewDecoder(io.LimitReader(part, 1024)).Decode(&request); err != nil || request.Model != meta.Model || request.Mode != s.mode || request.AudioEncoding != "WAV" {
		t.Error("request settings not preserved", err)
	}
	part, err = mr.NextPart()
	if err != nil {
		t.Error(err)
		return
	}
	var prefix [52]byte
	if _, err := io.ReadFull(part, prefix[:]); err != nil {
		t.Error(err)
		return
	}
	if part.FileName() != "chunk.wav" || string(prefix[:4]) != "RIFF" || binary.LittleEndian.Uint32(prefix[24:]) != media.SampleRate {
		t.Error("invalid generated WAV upload")
	}
	start := int64(binary.LittleEndian.Uint64(prefix[44:])) / 24
	duration := int64(binary.LittleEndian.Uint32(prefix[40:])) / 48
	if duration <= 0 || duration > 540000 {
		t.Error("chunk duration exceeded safe request bound")
	}
	hash := sha256.New()
	hash.Write(prefix[:])
	n, err := io.Copy(hash, part)
	if err != nil || n+52 != 44+duration*48 {
		t.Error("upload truncated or extended", err)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Error("multipart was incomplete", err)
	}
	s.mu.Lock()
	s.counts[start]++
	attempt, fault := s.counts[start], s.fault
	sum := hex.EncodeToString(hash.Sum(nil))
	if old := s.hashes[start]; old != "" && old != sum {
		t.Error("retry/resume uploaded changed audio")
	}
	s.hashes[start] = sum
	s.mu.Unlock()
	if start != 0 && attempt == 1 {
		switch fault {
		case "429":
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		case "413":
			w.WriteHeader(413)
			fmt.Fprint(w, `{"error":{"message":"fake-private-key private error body"}}`)
			return
		case "network":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
	}
	if start == 0 && s.wait != nil {
		select {
		case <-s.wait:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	response := meetingResponse(start, duration)
	if s.mode == meta.Endpointing {
		for i := range response.Turns {
			response.Turns[i].Speaker = nil
		}
	}
	json.NewEncoder(w).Encode(response)
}

type meetingFixture struct {
	runner Runner
	input  string
	media  *meetingMedia
	native *fakeDiarizer
	auth   *fakeCredentials
	http   *meetingHTTP
}

func longMeeting(t *testing.T, pause bool, fault string, outOfOrder bool) *meetingFixture {
	t.Helper()
	input := filepath.Join(t.TempDir(), "generated.wav")
	if err := generatedWAV(input, 960*media.SampleRate, 0); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "timeline.rttm")
	text := "SPEAKER recording 1 0.000 3.000 <NA> <NA> native_a <NA> <NA>\n" +
		"SPEAKER recording 1 4.000 505.000 <NA> <NA> native_b <NA> <NA>\n" +
		"SPEAKER recording 1 511.000 27.000 <NA> <NA> native_b <NA> <NA>\n" +
		"SPEAKER recording 1 539.500 0.500 <NA> <NA> native_a <NA> <NA>\n" +
		"SPEAKER recording 1 540.000 110.000 <NA> <NA> native_b <NA> <NA>\n" +
		"SPEAKER recording 1 650.000 10.000 <NA> <NA> native_a <NA> <NA>\n" +
		"SPEAKER recording 1 660.000 300.000 <NA> <NA> native_b <NA> <NA>\n"
	if err := os.WriteFile(raw, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	f := &meetingFixture{input: input, media: &meetingMedia{input: input, dir: t.TempDir(), pause: pause}, native: &fakeDiarizer{path: raw}, auth: &fakeCredentials{}, http: &meetingHTTP{counts: map[int64]int{}, hashes: map[int64]string{}, fault: fault, mode: meta.Diarization}}
	if pause {
		f.http.mode = meta.Endpointing
	}
	f.runner = Runner{Media: f.media, Diarizer: f.native, Credentials: f.auth, CacheDir: filepath.Join(t.TempDir(), "jobs")}
	if outOfOrder {
		savedSecond := make(chan struct{})
		var once sync.Once
		f.http.wait = savedSecond
		f.runner.Pool.OnComplete = func(_ context.Context, r meta.Result) error {
			if r.Chunk.StartSample == 0 {
				select {
				case <-savedSecond:
				default:
					t.Error("completion was not out of order")
				}
			} else {
				once.Do(func() { close(savedSecond) })
			}
			return nil
		}
	}
	f.runner.Pool.Transcriber = localMeta(t, func(w http.ResponseWriter, r *http.Request) { f.http.serve(t, w, r) })
	return f
}

func readCanonical(t *testing.T, path string) *transcript.Transcript {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := transcript.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func verifyMeeting(t *testing.T, doc *transcript.Transcript, pause bool) {
	t.Helper()
	if doc.Recording.DurationMS != 960000 || len(doc.SourceChunks) != 2 || len(doc.Speakers) != 2 || len(doc.Turns) != 7 {
		t.Fatal("long meeting lost canonical data")
	}
	byText := map[string]transcript.Turn{}
	for _, turn := range doc.Turns {
		byText[strings.Join(strings.Fields(turn.Text), " ")] = turn
	}
	a, b := byText["First participant."], byText["Returning participant."]
	if a.SpeakerID == nil || b.SpeakerID == nil || *a.SpeakerID != *b.SpeakerID || b.StartMS != 650200 || b.EndMS != 651200 {
		t.Fatal("returning global speaker or absolute timing changed")
	}
	for _, key := range []string{"Transition.", "Zero timing."} {
		if byText[key].SpeakerID != nil || byText[key].Attribution.Status != transcript.Unknown {
			t.Fatal("ambiguous/zero-duration text was assigned")
		}
	}
	if byText["Zero timing."].Attribution.Reason != transcript.InsufficientTiming {
		t.Fatal("zero-duration text lost its explicit timing limitation")
	}
	shared := byText["Shared boundary."]
	expectedChunk := doc.SourceChunks[0].ID
	expectedText := "Shared boundary."
	if pause {
		expectedChunk = doc.SourceChunks[1].ID
		expectedText = "Shared   boundary.\n"
	}
	if shared.Source.ChunkID != expectedChunk || shared.Text != expectedText {
		t.Fatal("overlap cleanup did not retain the earlier complete turn")
	}
	raw := len(doc.SourceChunks[0].RawTurns) + len(doc.SourceChunks[1].RawTurns)
	wantRaw, cut := 9, int64(540000)
	if pause {
		wantRaw, cut = 8, 510000
	}
	secondStart := cut
	if !pause {
		secondStart -= 500
	}
	if raw != wantRaw || doc.SourceChunks[0].EndMS != cut || doc.SourceChunks[1].StartMS != secondStart {
		t.Fatal("chunk boundary, overlap or raw provenance changed")
	}
}

func TestLongMeetingHTTPIntegration(t *testing.T) {
	for _, fault := range []string{"", "429", "network", "pause"} {
		name := fault
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			pause := fault == "pause"
			f := longMeeting(t, pause, fault, true)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			if err := f.runner.Run(ctx, f.input, Options{Mode: f.http.mode, Output: filepath.Join(dir, "meeting.md")}, &stdout, &stderr); err != nil || stdout.Len() != 0 {
				t.Fatal("long HTTP pipeline failed", err)
			}
			doc := readCanonical(t, filepath.Join(dir, "meeting.transcript.json"))
			verifyMeeting(t, doc, pause)
			if doc.Pipeline.MetaMode != f.http.mode {
				t.Fatal("canonical inference mode changed")
			}
			if f.native.calls != 1 || f.media.prepare != 1 || f.media.detect != 1 || strings.Contains(stderr.String(), "fake-private-key") || strings.Contains(stderr.String(), "participant") {
				t.Fatal("pipeline repeated native work or leaked private fields")
			}
			f.http.mu.Lock()
			first, second := f.http.counts[0], f.http.counts[doc.SourceChunks[1].StartMS]
			f.http.mu.Unlock()
			wantSecond := 1
			if fault == "429" || fault == "network" {
				wantSecond = 2
			}
			if first != 1 || second != wantSecond {
				t.Fatal("retry counts exceeded the expected attempts", first, second)
			}
			if err := output.Rename(ctx, filepath.Join(dir, "meeting.transcript.json"), map[int]string{1: "Example One", 2: "Example Two"}); err != nil {
				t.Fatal(err)
			}
			if err := output.Export(ctx, filepath.Join(dir, "meeting.transcript.json"), filepath.Join(dir, "meeting.txt"), transcript.RenderOptions{Format: "text", Timestamps: "range"}, nil); err != nil {
				t.Fatal(err)
			}
			renamed := readCanonical(t, filepath.Join(dir, "meeting.transcript.json"))
			doc.Speakers = renamed.Speakers
			if !reflect.DeepEqual(renamed, doc) {
				t.Fatal("offline editing changed IDs, raw data or original timing")
			}
		})
	}
}

func TestLongMeetingFailureAndResume(t *testing.T) {
	f := longMeeting(t, false, "413", false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	opts := Options{Concurrency: 1, Output: filepath.Join(dir, "meeting.md")}
	var stdout bytes.Buffer
	err := f.runner.Run(ctx, f.input, opts, &stdout, io.Discard)
	var remote *meta.Error
	if !errors.As(err, &remote) || remote.StatusCode != 413 || stdout.Len() != 0 {
		t.Fatal("permanent failure was lost or emitted partial transcript", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("failed meeting published files", entries, err)
	}
	f.http.mu.Lock()
	f.http.fault = ""
	f.http.mu.Unlock()
	opts.Resume = true
	if err := f.runner.Run(ctx, f.input, opts, &stdout, io.Discard); err != nil {
		t.Fatal("resume failed", err)
	}
	doc := readCanonical(t, filepath.Join(dir, "meeting.transcript.json"))
	verifyMeeting(t, doc, false)
	f.http.mu.Lock()
	first, second := f.http.counts[0], f.http.counts[539500]
	f.http.mu.Unlock()
	if first != 1 || second != 2 || f.native.calls != 1 || f.media.prepare != 2 || f.media.detect != 1 {
		t.Fatal("resume retried saved work or reran global analysis", first, second)
	}
	f.auth.fail, f.media.fail, f.native.fail = errors.New("offline guard"), errors.New("offline guard"), errors.New("offline guard")
	stdout.Reset()
	if err := f.runner.Run(ctx, f.input, Options{Resume: true, Render: transcript.RenderOptions{Format: "json"}}, &stdout, io.Discard); err != nil {
		t.Fatal("complete recovery needed inference", err)
	}
	again, err := transcript.Decode(stdout.Bytes())
	if err != nil || !reflect.DeepEqual(doc, again) {
		t.Fatal("resume changed the completed canonical data", err)
	}
	if err := os.WriteFile(f.input, []byte("changed input"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Output = filepath.Join(t.TempDir(), "changed.md")
	if err := f.runner.Run(ctx, f.input, opts, io.Discard, io.Discard); !errors.Is(err, jobs.ErrMismatch) {
		t.Fatal("changed recording reused a checkpoint", err)
	}
}
