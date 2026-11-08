package alignment

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestLabeledLongRecordingBoundaries(t *testing.T) {
	ctx := context.Background()
	const duration = 90 * time.Minute
	samples := int64(duration/time.Second)*media.SampleRate + 1
	plan, err := chunking.Plan(ctx, samples, diarization.Timeline{Duration: duration + time.Second/media.SampleRate}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(true, 1000)
	f.plan, f.results = plan, []meta.Result{}
	f.recording.DurationMS = duration.Milliseconds()
	for i, c := range plan {
		if c.StartMS() != int64(i)*(540000-500) {
			t.Fatal("planner introduced cumulative boundary drift")
		}
		f.results = append(f.results, meta.Result{Chunk: c, Response: &meta.Response{SessionID: fmt.Sprintf("request-%d", i), Turns: []transcript.RawTurn{}}})
	}
	type label struct {
		start, end int64
		text       string
	}
	want := map[transcript.Source]label{}
	add := func(i int, start, end int64, text string, keep bool) {
		r := f.results[i].Response
		id := int64(len(r.Turns))
		r.Turns = append(r.Turns, raw(id, start-plan[i].StartMS(), end-plan[i].StartMS(), text))
		if keep {
			want[transcript.Source{ChunkID: plan[i].ID, MetaTurnID: id}] = label{start, end, text}
		}
	}
	for i := 1; i < len(plan); i++ {
		end := plan[i-1].EndMS()
		add(i-1, end-700, end-550, "Before boundary.", true)
		add(i-1, end-400, end-100, "Whole repeated upload.", true)
		add(i, end-400, end-100, "Whole repeated upload.", false)
		for _, offset := range []int64{480, 420} {
			add(i-1, end-offset, end-offset+40, "Yes.", true)
			add(i, end-offset, end-offset+40, "Yes.", false)
		}
		add(i-1, end-100, end, "Crossing", true)
		add(i, end-100, end+100, "Crossing the boundary.", true)
		add(i, end+150, end+300, "After boundary.", true)
	}
	add(len(plan)-1, f.recording.DurationMS-100, f.recording.DurationMS, "Recording end.", true)
	got := f.assemble(t)
	if len(got.Turns) != len(want) {
		t.Fatalf("lost/repeated boundary utterances: got %d, want %d", len(got.Turns), len(want))
	}
	for _, turn := range got.Turns {
		expected, ok := want[turn.Source]
		if !ok || expected != (label{turn.StartMS, turn.EndMS, turn.Text}) {
			t.Fatal("labeled boundary timing/text changed")
		}
		delete(want, turn.Source)
	}
	if len(want) != 0 {
		t.Fatal("missing labeled boundary utterances")
	}
	for i, j := 0, len(f.results)-1; i < j; i, j = i+1, j-1 {
		f.results[i], f.results[j] = f.results[j], f.results[i]
	}
	if reversed := f.assemble(t); !reflect.DeepEqual(reversed, got) {
		t.Fatal("long recording depends on response completion order")
	}
}

func FuzzAssemble(f *testing.F) {
	f.Add(int64(600), int64(900), int64(100), int64(400), "Hello.")
	f.Add(int64(600), int64(600), int64(100), int64(100), "Yes.")
	f.Add(int64(-1), int64(1000), int64(0), int64(500), "")
	f.Fuzz(func(t *testing.T, start1, end1, start2, end2 int64, text string) {
		if len(text) > 4096 {
			t.Skip()
		}
		fixture := newFixture(true, 1000)
		fixture.results[0].Response.Turns = []transcript.RawTurn{raw(0, start1, end1, text)}
		fixture.results[1].Response.Turns = []transcript.RawTurn{raw(0, start2, end2, text)}
		got, err := Assemble(context.Background(), fixture.recording, fixture.pipeline, fixture.plan, fixture.results)
		if err != nil {
			if got != nil {
				t.Fatal("failed assembly returned output")
			}
			return
		}
		if err := transcript.Validate(got); err != nil {
			t.Fatal(err)
		}
		for _, c := range got.SourceChunks {
			i := 0
			if c.ID == "second" {
				i = 1
			}
			if !reflect.DeepEqual(c.RawTurns, fixture.results[i].Response.Turns) {
				t.Fatal("assembly altered raw source data")
			}
		}
	})
}

func TestAssemblyCancellationDuringWork(t *testing.T) {
	fixture := newFixture(true, 1000)
	fixture.results[0].Response.Turns = []transcript.RawTurn{raw(0, 600, 900, "Hello.")}
	fixture.results[1].Response.Turns = []transcript.RawTurn{raw(0, 100, 400, "Hello.")}
	// Deterministically cancel at different context checks without sleeps/races.
	for _, after := range []uint64{2, 5, 10, 15, 20, 25} {
		ctx := &countdownContext{Context: context.Background(), remaining: after}
		got, err := Assemble(ctx, fixture.recording, fixture.pipeline, fixture.plan, fixture.results)
		if err != nil && got != nil {
			t.Fatal("canceled assembly returned output")
		}
	}
}

type countdownContext struct {
	context.Context
	remaining uint64
}

func (c *countdownContext) Err() error {
	if c.remaining == 0 {
		return context.Canceled
	}
	c.remaining--
	return nil
}
