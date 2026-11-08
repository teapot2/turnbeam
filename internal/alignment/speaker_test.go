package alignment

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func parsedTimeline(t *testing.T, durationMS int64, rows string) diarization.Timeline {
	t.Helper()
	timeline, err := diarization.ParseRTTM(context.Background(), strings.NewReader(rows), "recording", time.Duration(durationMS)*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return timeline
}

func singleTurn(t *testing.T, start, end int64) *transcript.Transcript {
	t.Helper()
	f := newFixture(false, 10000)
	f.results[0].Response.Turns = []transcript.RawTurn{raw(0, start, end, "Hello.")}
	return f.assemble(t)
}

// The turn spans 0.200–1.200 s (1000 ms): the main speaker needs 85% of the
// speech inside it and at least 300 ms of coverage. Patterns marked AMI were
// observed when calibrating against human annotations.
func TestMainSpeakerShareAndCoverage(t *testing.T) {
	const a, b = "speaker_0001", "speaker_0002"
	for _, tc := range []struct {
		name, rows, assigned string
		candidates           []string
	}{
		{"clear", "SPEAKER recording 1 0.000 2.000 <NA> <NA> A <NA> <NA>\n", a, nil},
		{"coverage at threshold", "SPEAKER recording 1 0.200 0.300 <NA> <NA> A <NA> <NA>\n", a, nil},
		{"coverage one ns below", "SPEAKER recording 1 0.200 0.299999999 <NA> <NA> A <NA> <NA>\n", "", []string{a}},
		{"AMI: pauses inside one turn", "SPEAKER recording 1 0.200 0.200 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.800 0.200 <NA> <NA> A <NA> <NA>\n", a, nil},
		{"duplicates cannot inflate coverage", "SPEAKER recording 1 0.200 0.200 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.200 0.200 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.300 0.150 <NA> <NA> A <NA> <NA>\n", "", []string{a}},
		{"AMI: brief interjection", "SPEAKER recording 1 0.200 1.000 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 1.190 0.010 <NA> <NA> B <NA> <NA>\n", a, nil},
		{"share at threshold", "SPEAKER recording 1 0.200 0.850 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 1.050 0.150 <NA> <NA> B <NA> <NA>\n", a, nil},
		{"share one ns below", "SPEAKER recording 1 0.200 0.849999999 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 1.049999999 0.150000001 <NA> <NA> B <NA> <NA>\n", "", []string{a, b}},
		{"AMI: substantial second voice", "SPEAKER recording 1 0.200 1.000 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.500 0.300 <NA> <NA> B <NA> <NA>\n", "", []string{a, b}},
		{"AMI: real exchange", "SPEAKER recording 1 0.200 0.500 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.700 0.500 <NA> <NA> B <NA> <NA>\n", "", []string{a, b}},
		{"equal speakers", "SPEAKER recording 1 0.200 1.000 <NA> <NA> A <NA> <NA>\nSPEAKER recording 1 0.200 1.000 <NA> <NA> B <NA> <NA>\n", "", []string{a, b}},
		{"neighbor ends at the turn", "SPEAKER recording 1 0.000 0.200 <NA> <NA> B <NA> <NA>\nSPEAKER recording 1 0.200 1.000 <NA> <NA> A <NA> <NA>\n", "speaker_0002", nil},
		{"neighbor one ns inside", "SPEAKER recording 1 0.000 0.200000001 <NA> <NA> B <NA> <NA>\nSPEAKER recording 1 0.200 1.000 <NA> <NA> A <NA> <NA>\n", "speaker_0002", nil},
		{"activity only after the turn", "SPEAKER recording 1 1.200 0.100 <NA> <NA> A <NA> <NA>\n", "", nil},
		{"far away activity", "SPEAKER recording 1 5.000 1.000 <NA> <NA> A <NA> <NA>\n", "", nil},
		{"silence", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := singleTurn(t, 200, 1200)
			before, err := transcript.Encode(input)
			if err != nil {
				t.Fatal(err)
			}
			timeline := parsedTimeline(t, input.Recording.DurationMS, tc.rows)
			got, err := Align(context.Background(), input, timeline, DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			turn := got.Turns[0]
			if tc.assigned == "" {
				if turn.SpeakerID != nil || turn.Attribution.Status != transcript.Unknown || turn.Attribution.Reason != transcript.AmbiguousOverlapOrBoundary || !reflect.DeepEqual(turn.Attribution.CandidateSpeakerIDs, tc.candidates) {
					t.Fatalf("unsafe attribution: %+v", turn.Attribution)
				}
			} else if turn.SpeakerID == nil || *turn.SpeakerID != tc.assigned || turn.Attribution.Status != transcript.Assigned || turn.Attribution.Method != transcript.TimelineOverlap || len(turn.Attribution.CandidateSpeakerIDs) != 0 {
				t.Fatalf("clear turn was not assigned: %+v", turn.Attribution)
			}
			if turn.ID != input.Turns[0].ID || turn.StartMS != 200 || turn.EndMS != 1200 || turn.Text != input.Turns[0].Text || turn.Source != input.Turns[0].Source || !reflect.DeepEqual(got.SourceChunks, input.SourceChunks) {
				t.Fatal("alignment changed timing, text or provenance")
			}
			after, _ := transcript.Encode(input)
			if string(before) != string(after) {
				t.Fatal("alignment mutated its input")
			}
			data, err := transcript.Encode(got)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := transcript.Decode(data)
			if err != nil || !reflect.DeepEqual(restored, got) {
				t.Fatal("alignment did not survive canonical JSON")
			}
		})
	}
}

func TestZeroDurationShortAcknowledgementAndPolicy(t *testing.T) {
	for _, duration := range []int64{0, 1, 10} {
		input := singleTurn(t, 250, 250+duration)
		timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.000 1.000 <NA> <NA> A <NA> <NA>\n")
		got, err := Align(context.Background(), input, timeline, DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if duration == 0 {
			if got.Turns[0].SpeakerID != nil || got.Turns[0].Attribution.Reason != transcript.InsufficientTiming {
				t.Fatal("zero timing was attributed")
			}
		} else if got.Turns[0].SpeakerID == nil {
			t.Fatal("clear short acknowledgement was forced unknown")
		}
	}
	input := singleTurn(t, 200, 1200)
	timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.200 0.800 <NA> <NA> A <NA> <NA>\n")
	got, err := Align(context.Background(), input, timeline, Policy{MinSharePercent: 90, MinCoveragePercent: 100})
	if err != nil || got.Turns[0].SpeakerID != nil {
		t.Fatal("calibration policy was ignored")
	}
}

func TestAlignmentHandlesLongDurationsWithoutOverflow(t *testing.T) {
	input := singleTurn(t, 0, 1000)
	duration := time.Duration(math.MaxInt64)
	input.Recording.DurationMS = duration.Milliseconds()
	input.SourceChunks = input.SourceChunks[:1]
	input.SourceChunks[0].EndMS = duration.Milliseconds()
	input.SourceChunks[0].RawTurns[0].EndMS = duration.Milliseconds()
	input.Turns[0].EndMS = duration.Milliseconds()
	timeline := diarization.Timeline{Duration: duration, Speakers: []diarization.Speaker{{ID: "speaker_0001", Index: 1, RawID: "A"}}, Segments: []diarization.Segment{{Start: 0, End: duration, SpeakerID: "speaker_0001"}}}
	got, err := Align(context.Background(), input, timeline, Policy{MinSharePercent: 100, MinCoveragePercent: 100})
	if err != nil || got.Turns[0].SpeakerID == nil {
		t.Fatal("long duration overflowed coverage or guard arithmetic", err)
	}
}

func TestAlignmentRejectsMalformedTimelineAndInput(t *testing.T) {
	for name, damage := range map[string]func(*transcript.Transcript, *diarization.Timeline){
		"duration":          func(_ *transcript.Transcript, d *diarization.Timeline) { d.Duration += time.Millisecond },
		"missing speakers":  func(_ *transcript.Transcript, d *diarization.Timeline) { d.Speakers = nil },
		"missing segments":  func(_ *transcript.Transcript, d *diarization.Timeline) { d.Segments = nil },
		"unknown reference": func(_ *transcript.Transcript, d *diarization.Timeline) { d.Segments[0].SpeakerID = "other" },
		"invalid ID":        func(_ *transcript.Transcript, d *diarization.Timeline) { d.Speakers[0].ID = "other" },
		"invalid index":     func(_ *transcript.Transcript, d *diarization.Timeline) { d.Speakers[0].Index = 2 },
		"invalid label":     func(_ *transcript.Transcript, d *diarization.Timeline) { d.Speakers[0].RawID = "private label" },
		"unused speaker": func(_ *transcript.Transcript, d *diarization.Timeline) {
			d.Speakers = append(d.Speakers, diarization.Speaker{ID: "speaker_0002", Index: 2, RawID: "B"})
		},
		"negative segment": func(_ *transcript.Transcript, d *diarization.Timeline) { d.Segments[0].Start = -1 },
		"empty segment":    func(_ *transcript.Transcript, d *diarization.Timeline) { d.Segments[0].End = 0 },
		"overrun segment":  func(_ *transcript.Transcript, d *diarization.Timeline) { d.Segments[0].End = d.Duration + 1 },
		"unsorted segments": func(_ *transcript.Transcript, d *diarization.Timeline) {
			d.Segments = append([]diarization.Segment{{Start: 2 * time.Second, End: 3 * time.Second, SpeakerID: "speaker_0001"}}, d.Segments...)
		},
		"mismatched existing mapping": func(t *transcript.Transcript, _ *diarization.Timeline) {
			t.Speakers = []transcript.Speaker{{ID: "other", Index: 1}}
		},
		"bad transcript": func(t *transcript.Transcript, _ *diarization.Timeline) { t.Turns[0].Source.MetaTurnID = 99 },
	} {
		t.Run(name, func(t *testing.T) {
			input := singleTurn(t, 200, 1200)
			timeline := parsedTimeline(t, input.Recording.DurationMS, "SPEAKER recording 1 0.000 2.000 <NA> <NA> A <NA> <NA>\n")
			damage(input, &timeline)
			if got, err := Align(context.Background(), input, timeline, DefaultPolicy()); got != nil || !errors.Is(err, ErrAlignment) {
				t.Fatalf("malformed alignment input accepted: %v", err)
			}
		})
	}
	input := singleTurn(t, 200, 1200)
	timeline := parsedTimeline(t, input.Recording.DurationMS, "")
	for _, policy := range []Policy{{MinSharePercent: 90, MinCoveragePercent: 0}, {MinSharePercent: 90, MinCoveragePercent: 101}, {MinSharePercent: 50, MinCoveragePercent: 30}, {MinSharePercent: 101, MinCoveragePercent: 30}} {
		if got, err := Align(context.Background(), input, timeline, policy); got != nil || !errors.Is(err, ErrAlignment) {
			t.Fatal("invalid policy accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := Align(ctx, nil, timeline, DefaultPolicy()); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was lost")
	}
}
