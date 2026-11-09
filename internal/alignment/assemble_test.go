package alignment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type fixture struct {
	recording transcript.Recording
	pipeline  transcript.Pipeline
	plan      []chunking.Chunk
	results   []meta.Result
}

func newFixture(forced bool, cut int64) fixture {
	start, boundary, overlap := cut, chunking.Pause, int64(0)
	if forced {
		start, boundary, overlap = cut-500, chunking.Forced, chunking.OverlapSamples
	}
	const ms = media.SampleRate / 1000
	plan := []chunking.Chunk{
		{ID: "first", EndSample: cut * ms, Boundary: boundary},
		{ID: "second", StartSample: start * ms, EndSample: (cut + 1000) * ms, OverlapSamples: overlap, Boundary: chunking.End},
	}
	results := []meta.Result{}
	for i, c := range plan {
		results = append(results, meta.Result{Chunk: c, Response: &meta.Response{SessionID: fmt.Sprintf("request-%d", i), AudioDurationMS: c.EndMS() - c.StartMS(), Turns: []transcript.RawTurn{}}})
	}
	return fixture{
		recording: transcript.Recording{Name: "public.wav", DurationMS: cut + 1000, SHA256: strings.Repeat("a", 64)},
		pipeline:  transcript.Pipeline{Engine: transcript.EngineMeta, MetaModel: meta.Model, MetaMode: meta.Diarization, DiarizationModel: "local", DiarizationRevision: "pinned", RuntimeVersion: "pinned"},
		plan:      plan, results: results,
	}
}

func raw(id, start, end int64, text string) transcript.RawTurn {
	return transcript.RawTurn{TurnID: id, StartMS: start, EndMS: end, Transcript: text}
}

func (f fixture) assemble(t *testing.T) *transcript.Transcript {
	t.Helper()
	got, err := Assemble(context.Background(), f.recording, f.pipeline, f.plan, f.results)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Validate(got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAssemblyChronologyAndProvenance(t *testing.T) {
	f := newFixture(true, 1000)
	label := "A"
	f.results[0].Response.AudioDurationMS += 40 // Metadata never drives offsets.
	f.results[0].Response.Turns = []transcript.RawTurn{
		raw(8, 600, 800, "  Hello\nworld.  "), raw(1, 100, 200, "Before."), raw(9, 500, 500, "Yes"), raw(2, 400, 450, " \t"),
	}
	f.results[0].Response.Turns[0].Speaker = &label
	f.results[1].Response.Turns = []transcript.RawTurn{raw(0, 100, 300, "Hello world."), raw(2, 700, 900, "After."), raw(3, 0, 0, "Yes")}
	f.results[1].Response.Turns[0].Speaker = &label // Same letter is not global identity.
	got := f.assemble(t)
	if len(got.Turns) != 5 || len(got.SourceChunks[0].RawTurns) != 4 || len(got.SourceChunks[1].RawTurns) != 3 {
		t.Fatal("canonical filtering changed raw provenance")
	}
	wantSources := []transcript.Source{{ChunkID: "first", MetaTurnID: 1}, {ChunkID: "first", MetaTurnID: 9}, {ChunkID: "second", MetaTurnID: 3}, {ChunkID: "first", MetaTurnID: 8}, {ChunkID: "second", MetaTurnID: 2}}
	for i, turn := range got.Turns {
		if turn.Source != wantSources[i] || turn.ID != fmt.Sprintf("turn_%06d", i+1) || turn.SpeakerID != nil || turn.Attribution.Status != transcript.Unknown {
			t.Fatal("order, identity or source changed")
		}
		reason := transcript.NotAligned
		if turn.StartMS == turn.EndMS {
			reason = transcript.InsufficientTiming
		}
		if turn.Attribution.Reason != reason {
			t.Fatal("unknown timing reason changed")
		}
	}
	if got.Turns[3].Text != "  Hello\nworld.  " || got.Turns[3].StartMS != 600 || got.Turns[3].EndMS != 800 {
		t.Fatal("kept turn was rewritten or retimed")
	}
	f.results[0], f.results[1] = f.results[1], f.results[0]
	if shuffled := f.assemble(t); !reflect.DeepEqual(shuffled, got) {
		t.Fatal("completion order changed assembled output")
	}
	data, err := transcript.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := transcript.Decode(data)
	if err != nil || !reflect.DeepEqual(restored, got) {
		t.Fatal("canonical round trip lost assembly data")
	}
	label = "changed by caller"
	f.results[1].Response.Turns[0].Transcript = "changed by caller"
	if *got.SourceChunks[0].RawTurns[0].Speaker != "A" || got.SourceChunks[0].RawTurns[0].Transcript != "  Hello\nworld.  " {
		t.Fatal("returned raw data aliases input")
	}
	*got.SourceChunks[1].RawTurns[0].Speaker = "changed output"
	if label != "changed by caller" {
		t.Fatal("returned labels alias input")
	}
}

func TestConservativeOverlapPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right []transcript.RawTurn
		forced      bool
		want        int
	}{
		{"exact whole turn", []transcript.RawTurn{raw(0, 600, 900, "Hello.")}, []transcript.RawTurn{raw(0, 100, 400, "Hello.")}, true, 1},
		{"entire overlap", []transcript.RawTurn{raw(0, 500, 1000, "Hello.")}, []transcript.RawTurn{raw(0, 0, 500, "Hello.")}, true, 1},
		{"whitespace only", []transcript.RawTurn{raw(0, 600, 900, " Hello.\n")}, []transcript.RawTurn{raw(0, 100, 400, "\tHello.")}, true, 1},
		{"case difference", []transcript.RawTurn{raw(0, 600, 900, "Hello.")}, []transcript.RawTurn{raw(0, 100, 400, "hello.")}, true, 2},
		{"punctuation difference", []transcript.RawTurn{raw(0, 600, 900, "Hello.")}, []transcript.RawTurn{raw(0, 100, 400, "Hello!")}, true, 2},
		{"timing jitter", []transcript.RawTurn{raw(0, 600, 900, "Hello.")}, []transcript.RawTurn{raw(0, 101, 400, "Hello.")}, true, 2},
		{"partial matching text", []transcript.RawTurn{raw(0, 600, 900, "Hello there.")}, []transcript.RawTurn{raw(0, 100, 400, "there.")}, true, 2},
		{"straddling overlap", []transcript.RawTurn{raw(0, 400, 900, "Hello.")}, []transcript.RawTurn{raw(0, 0, 400, "Hello.")}, true, 2},
		{"outside overlap", []transcript.RawTurn{raw(0, 100, 400, "Yes.")}, []transcript.RawTurn{raw(0, 600, 900, "Yes.")}, true, 2},
		{"intentional repetition", []transcript.RawTurn{raw(0, 550, 650, "Yes.")}, []transcript.RawTurn{raw(0, 200, 300, "Yes.")}, true, 2},
		{"many to one", []transcript.RawTurn{raw(0, 600, 900, "Yes."), raw(1, 600, 900, "Yes.")}, []transcript.RawTurn{raw(0, 100, 400, "Yes.")}, true, 3},
		{"one to many", []transcript.RawTurn{raw(0, 600, 900, "Yes.")}, []transcript.RawTurn{raw(0, 100, 400, "Yes."), raw(1, 100, 400, "Yes.")}, true, 3},
		{"zero duration", []transcript.RawTurn{raw(0, 600, 600, "Yes.")}, []transcript.RawTurn{raw(0, 100, 100, "Yes.")}, true, 2},
		{"clean pause repetition", []transcript.RawTurn{raw(0, 800, 1000, "Yes.")}, []transcript.RawTurn{raw(0, 0, 200, "Yes.")}, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(tc.forced, 1000)
			f.results[0].Response.Turns, f.results[1].Response.Turns = tc.left, tc.right
			got := f.assemble(t)
			if len(got.Turns) != tc.want || !reflect.DeepEqual(got.SourceChunks[0].RawTurns, tc.left) || !reflect.DeepEqual(got.SourceChunks[1].RawTurns, tc.right) {
				t.Fatalf("unsafe overlap edit: got %d turns, want %d", len(got.Turns), tc.want)
			}
		})
	}
}

func TestAssemblyRejectsIncompleteOrMalformedInput(t *testing.T) {
	for name, damage := range map[string]func(*fixture){
		"missing result":    func(f *fixture) { f.results = f.results[:1] },
		"duplicate result":  func(f *fixture) { f.results[1] = f.results[0] },
		"unknown chunk":     func(f *fixture) { f.results[1].Chunk.ID = "other" },
		"modified range":    func(f *fixture) { f.results[1].Chunk.StartSample++ },
		"nil response":      func(f *fixture) { f.results[0].Response = nil },
		"nil raw turns":     func(f *fixture) { f.results[0].Response.Turns = nil },
		"invalid session":   func(f *fixture) { f.results[0].Response.SessionID = " " },
		"negative duration": func(f *fixture) { f.results[0].Response.AudioDurationMS = -1 },
		"missing raw text":  func(f *fixture) { f.results[0].Response.Transcript = "Hello" },
		"wrong duration":    func(f *fixture) { f.recording.DurationMS++ },
		"path metadata":     func(f *fixture) { f.recording.Name = "/private/meeting.wav" },
		"duplicate raw ID": func(f *fixture) {
			f.results[0].Response.Turns = []transcript.RawTurn{raw(0, 0, 100, "one"), raw(0, 100, 200, "two")}
		},
		"out of bounds turn": func(f *fixture) { f.results[0].Response.Turns = []transcript.RawTurn{raw(0, 0, 1001, "one")} },
		"overflowing turn":   func(f *fixture) { f.results[1].Response.Turns = []transcript.RawTurn{raw(0, 0, math.MaxInt64, "one")} },
		"negative turn":      func(f *fixture) { f.results[0].Response.Turns = []transcript.RawTurn{raw(0, -1, 0, "one")} },
		"invalid UTF8": func(f *fixture) {
			f.results[0].Response.Turns = []transcript.RawTurn{raw(0, 0, 100, string([]byte{0xff}))}
		},
		"unexplained overlap": func(f *fixture) { f.plan[0].Boundary = chunking.Pause; f.results[0].Chunk = f.plan[0] },
		"recording gap":       func(f *fixture) { f.plan[1].StartSample += 24; f.results[1].Chunk = f.plan[1] },
		"unaligned cut":       func(f *fixture) { f.plan[0].EndSample++; f.results[0].Chunk = f.plan[0] },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(true, 1000)
			damage(&f)
			out, err := Assemble(context.Background(), f.recording, f.pipeline, f.plan, f.results)
			if out != nil || !errors.Is(err, ErrAssembly) {
				t.Fatalf("bad input produced a transcript: %v", err)
			}
		})
	}
	f := newFixture(true, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := Assemble(ctx, f.recording, f.pipeline, f.plan, f.results); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation did not discard output")
	}
}

func TestAssemblySilenceAndSubMillisecondTail(t *testing.T) {
	f := newFixture(false, 1000)
	f.plan[1].EndSample++
	f.results[1].Chunk = f.plan[1]
	got := f.assemble(t)
	if got.Turns == nil || len(got.Turns) != 0 || got.SourceChunks[1].EndMS != 2000 {
		t.Fatal("silent audio or sample rounding changed")
	}
}
