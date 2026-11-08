package alignment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestReturningGlobalSpeakerAndDetachedRealignment(t *testing.T) {
	f := newFixture(false, 540000)
	a, b := "A", "B"
	f.results[0].Response.Turns = []transcript.RawTurn{raw(0, 100, 200, "First."), raw(1, 2100, 2200, "Second.")}
	f.results[1].Response.Turns = []transcript.RawTurn{raw(0, 200, 500, "Returning after silence.")}
	f.results[0].Response.Turns[0].Speaker, f.results[0].Response.Turns[1].Speaker = &a, &b
	f.results[1].Response.Turns[0].Speaker = &b // B in this request is not B above.
	input := f.assemble(t)
	timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.000 1.000 <NA> <NA> native_z <NA> <NA>\nSPEAKER recording 1 2.000 1.000 <NA> <NA> native_a <NA> <NA>\nSPEAKER recording 1 540.000 1.000 <NA> <NA> native_z <NA> <NA>\n")
	got, err := Align(context.Background(), input, timeline, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"speaker_0001", "speaker_0002", "speaker_0001"} {
		if got.Turns[i].SpeakerID == nil || *got.Turns[i].SpeakerID != id {
			t.Fatal("returning speaker or first-appearance IDs changed")
		}
	}
	name := "Alice"
	got.Speakers[0].Name = &name
	got.SourceChunks[0].RawTurns[0].Speaker = copyString(&a)
	again, err := Align(context.Background(), got, timeline, DefaultPolicy())
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatal("realignment changed stable identities/display metadata", err)
	}
	*again.Speakers[0].Name = "Changed output"
	*again.SourceChunks[0].RawTurns[0].Speaker = "Changed output"
	*again.Turns[0].SpeakerID = "Changed output"
	if *got.Speakers[0].Name != "Alice" || *got.SourceChunks[0].RawTurns[0].Speaker != "A" || *got.Turns[0].SpeakerID != "speaker_0001" || len(input.Speakers) != 0 || input.Turns[0].SpeakerID != nil {
		t.Fatal("alignment outputs alias inputs")
	}
}

func TestCandidateOrderAndFirstAppearanceTies(t *testing.T) {
	input := singleTurn(t, 200, 1200)
	var rows strings.Builder
	// Raw order is deliberately reversed; simultaneous first appearances use labels.
	for i := 7; i >= 0; i-- {
		fmt.Fprintf(&rows, "SPEAKER recording 1 0.000 2.000 <NA> <NA> native_%d <NA> <NA>\n", i)
	}
	timeline := parsedTimeline(t, input.Recording.DurationMS, rows.String())
	got, err := Align(context.Background(), input, timeline, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns[0].SpeakerID != nil || len(got.Turns[0].Attribution.CandidateSpeakerIDs) != 8 {
		t.Fatal("overlapping speakers were forced to one identity")
	}
	for i, id := range got.Turns[0].Attribution.CandidateSpeakerIDs {
		if id != fmt.Sprintf("speaker_%04d", i+1) || timeline.Speakers[i].RawID != fmt.Sprintf("native_%d", i) {
			t.Fatal("candidate or first-appearance tie order is nondeterministic")
		}
	}
	timeline.Speakers[0].RawID, timeline.Speakers[1].RawID = timeline.Speakers[1].RawID, timeline.Speakers[0].RawID
	if out, err := Align(context.Background(), input, timeline, DefaultPolicy()); out != nil || !errors.Is(err, ErrAlignment) {
		t.Fatal("invalid first-appearance tie mapping was accepted")
	}
}

func TestAlignmentEmptyTurnsAndSubMillisecondTimelineTail(t *testing.T) {
	f := newFixture(false, 1000)
	input := f.assemble(t)
	timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.000 1.000 <NA> <NA> A <NA> <NA>\n")
	timeline.Duration += time.Microsecond
	got, err := Align(context.Background(), input, timeline, DefaultPolicy())
	if err != nil || got.Turns == nil || len(got.Turns) != 0 || len(got.Speakers) != 1 {
		t.Fatal("silence/text absence lost the global speaker mapping or exact timeline tail", err)
	}
}

func TestAlignmentCancellationDuringProcessing(t *testing.T) {
	input := singleTurn(t, 200, 1200)
	timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.000 2.000 <NA> <NA> A <NA> <NA>\n")
	for remaining := uint64(1); remaining < 20; remaining++ {
		ctx := &countdownContext{Context: context.Background(), remaining: remaining}
		got, err := Align(ctx, input, timeline, DefaultPolicy())
		if err != nil && (!errors.Is(err, context.Canceled) || got != nil) {
			t.Fatal("cancellation cause or all-or-error contract lost", err)
		}
	}
}

func FuzzAlign(f *testing.F) {
	f.Add(int64(200), int64(1200), int64(0), int64(2000000000), 90, 30)
	f.Add(int64(0), int64(0), int64(0), int64(1), 100, 100)
	f.Fuzz(func(t *testing.T, start, end, speakerStart, speakerEnd int64, share, coverage int) {
		fixture := newFixture(false, 10000)
		fixture.results[0].Response.Turns = []transcript.RawTurn{raw(0, start, end, "Hello.")}
		input, err := Assemble(context.Background(), fixture.recording, fixture.pipeline, fixture.plan, fixture.results)
		if err != nil {
			return
		}
		timeline := diarization.Timeline{Duration: time.Duration(input.Recording.DurationMS) * time.Millisecond,
			Speakers: []diarization.Speaker{{ID: "speaker_0001", Index: 1, RawID: "A"}},
			Segments: []diarization.Segment{{Start: time.Duration(speakerStart), End: time.Duration(speakerEnd), SpeakerID: "speaker_0001"}}}
		got, err := Align(context.Background(), input, timeline, Policy{MinSharePercent: share, MinCoveragePercent: coverage})
		if err != nil {
			if got != nil {
				t.Fatal("invalid alignment returned partial output")
			}
			return
		}
		if err := transcript.Validate(got); err != nil {
			t.Fatal(err)
		}
		if got.Turns[0].StartMS != start || got.Turns[0].EndMS != end || !reflect.DeepEqual(got.SourceChunks, input.SourceChunks) || input.Turns[0].SpeakerID != nil {
			t.Fatal("alignment altered original data")
		}
	})
}
