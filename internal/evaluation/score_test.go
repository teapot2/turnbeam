package evaluation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func document(windows []Span, predictions []string) *transcript.Transcript {
	t := &transcript.Transcript{SchemaVersion: 1, Recording: transcript.Recording{Name: "fixture.wav", DurationMS: 120000, SHA256: strings.Repeat("a", 64)},
		Pipeline: transcript.Pipeline{MetaModel: "test", MetaMode: "DIARIZATION", DiarizationModel: "test", DiarizationRevision: "test", RuntimeVersion: "test"},
		Speakers: []transcript.Speaker{{ID: "x", Index: 1}, {ID: "y", Index: 2}}, Turns: []transcript.Turn{}, SourceChunks: []transcript.SourceChunk{{ID: "chunk", EndMS: 120000, MetaSessionID: "test", RawTurns: []transcript.RawTurn{}}}}
	for i, w := range windows {
		raw := transcript.RawTurn{TurnID: int64(i), StartMS: w.Start.Milliseconds(), EndMS: w.End.Milliseconds(), Transcript: "Test."}
		t.SourceChunks[0].RawTurns = append(t.SourceChunks[0].RawTurns, raw)
		turn := transcript.Turn{ID: fmt.Sprint(i), StartMS: raw.StartMS, EndMS: raw.EndMS, Text: raw.Transcript, Source: transcript.Source{ChunkID: "chunk", MetaTurnID: raw.TurnID}, Attribution: transcript.Attribution{Status: transcript.Unknown, Reason: transcript.AmbiguousOverlapOrBoundary}}
		if predictions[i] != "" {
			id := predictions[i]
			turn.SpeakerID = &id
			turn.Attribution = transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}
		}
		t.Turns = append(t.Turns, turn)
	}
	sort.SliceStable(t.Turns, func(i, j int) bool { return t.Turns[i].StartMS < t.Turns[j].StartMS })
	return t
}

func seconds(a, b int) Span {
	return Span{time.Duration(a) * time.Second, time.Duration(b) * time.Second}
}

func TestScoringControlsAndReferenceStates(t *testing.T) {
	truth := Reference{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 1), "A"}, {seconds(1, 2), "B"}, {seconds(40, 42), "A"}, {seconds(60, 62), "A"}, {seconds(61, 63), "B"}}, Silence: []Span{seconds(80, 90)}}
	windows := []Span{seconds(0, 1), seconds(1, 2), seconds(0, 2), seconds(60, 62), seconds(80, 81), seconds(20, 21)}
	for _, tc := range []struct {
		name        string
		predictions []string
		coverage    float64
		precision   *float64
		falseRate   *float64
		abstention  float64
	}{
		{"oracle", []string{"x", "y", "", "", "", ""}, 1, pointer(1), pointer(0), 1},
		{"always unknown", []string{"", "", "", "", "", ""}, 0, nil, nil, 1},
		{"always wrong", []string{"y", "x", "x", "x", "x", "x"}, 1, pointer(0), pointer(1), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := document(windows, tc.predictions)
			before, _ := transcript.Encode(input)
			got, err := Score(context.Background(), input, truth, map[string]string{"x": "A", "y": "B"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Assignable.Total.Turns != 2 || got.Ambiguous.Total.Turns != 2 || got.Silence.Total.Turns != 1 || got.Unscored.Total.Turns != 1 || *got.KnownCoverage.Count != tc.coverage || !reflect.DeepEqual(got.AssignedPrecision.Count, tc.precision) || !reflect.DeepEqual(got.FalseAttribution.Count, tc.falseRate) || *got.ConservativeAbstention.Count != tc.abstention {
				t.Fatalf("bad controls: %+v", got)
			}
			after, _ := transcript.Encode(input)
			if string(before) != string(after) {
				t.Fatal("scorer mutated input")
			}
		})
	}
}
func pointer(value float64) *float64 { return &value }

func TestDurationWeightsUnmappedAndSilenceBounds(t *testing.T) {
	truth := Reference{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 10), "A"}}, Silence: []Span{seconds(20, 21), seconds(21, 22)}}
	input := document([]Span{seconds(0, 1), seconds(1, 10), seconds(20, 22), seconds(21, 23)}, []string{"x", "", "y", "x"})
	got, err := Score(context.Background(), input, truth, map[string]string{"x": "A"})
	if err != nil {
		t.Fatal(err)
	}
	if *got.KnownCoverage.Count != 0.5 || *got.KnownCoverage.Duration != 0.1 || *got.AssignedPrecision.Count != 0.5 || *got.AssignedPrecision.Duration != 1.0/3 || got.Silence.Total.Turns != 1 || got.Unscored.Total.Turns != 1 {
		t.Fatal("count/duration/unmapped or partial-silence policy changed")
	}
	zero := document([]Span{seconds(0, 0)}, []string{""})
	got, err = Score(context.Background(), zero, truth, nil)
	if err != nil || got.ZeroDuration != 1 || got.Unscored.Total.Turns != 1 || got.AssignedPrecision.Count != nil || got.KnownCoverage.Count != nil {
		t.Fatal("zero timing inflated metrics", err)
	}
}

func TestOneGlobalMappingBeatsGreedyAssignment(t *testing.T) {
	truth := Reference{Duration: 20 * time.Second, Speech: []Speech{{seconds(0, 10), "A"}, {seconds(10, 20), "B"}}}
	timeline := diarization.Timeline{Duration: truth.Duration, Speakers: []diarization.Speaker{{ID: "x"}, {ID: "y"}}, Segments: []diarization.Segment{
		{Start: 0, End: 8 * time.Second, SpeakerID: "x"}, {Start: 0, End: 7 * time.Second, SpeakerID: "y"}, {Start: 10 * time.Second, End: 17 * time.Second, SpeakerID: "x"}}}
	mapping, err := Match(context.Background(), truth, timeline)
	if err != nil || !reflect.DeepEqual(mapping, map[string]string{"x": "B", "y": "A"}) {
		t.Fatal("global assignment was greedy or per-window", mapping, err)
	}
	timeline.Segments = append(timeline.Segments, timeline.Segments[0])
	again, err := Match(context.Background(), truth, timeline)
	if err != nil || !reflect.DeepEqual(mapping, again) {
		t.Fatal("duplicate intervals inflated mapping")
	}
}

func TestReturnPairsIncludeAbstentionMismatchAndOverlap(t *testing.T) {
	truth := Reference{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 1), "A"}, {seconds(40, 41), "A"}}}
	for _, tc := range []struct {
		predictions []string
		status      string
	}{{[]string{"x", "x"}, "retained"}, {[]string{"x", "y"}, "mismatched"}, {[]string{"", ""}, "unavailable_or_unknown"}} {
		got, err := Score(context.Background(), document([]Span{seconds(0, 1), seconds(40, 41)}, tc.predictions), truth, map[string]string{"x": "A"})
		if err != nil || len(got.Returns) != 1 || got.Returns[0].Status != tc.status {
			t.Fatal("return pair omitted or misclassified", err)
		}
	}
	truth.Speech = append(truth.Speech, Speech{seconds(0, 1), "B"})
	got, err := Score(context.Background(), document([]Span{seconds(0, 1), seconds(40, 41)}, []string{"x", "x"}), truth, map[string]string{"x": "A"})
	if err != nil || got.Returns[0].Status != "unscored_no_exclusive_reference" {
		t.Fatal("overlap-only return neighbor looked identifiable")
	}
}

func TestMalformedReferenceCancellationAndMapping(t *testing.T) {
	input := document([]Span{seconds(0, 1)}, []string{"x"})
	for _, truth := range []Reference{
		{}, {Duration: 120 * time.Second, Speech: []Speech{{seconds(1, 0), "A"}}},
		{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 1), ""}}},
		{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 1), "A"}}, Silence: []Span{seconds(0, 1)}},
	} {
		if got, err := Score(context.Background(), input, truth, nil); got != nil || !errors.Is(err, ErrReference) {
			t.Fatal("bad reference accepted")
		}
	}
	truth := Reference{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 1), "A"}}}
	for _, mapping := range []map[string]string{{"x": "missing"}, {"x": "A", "y": "A"}} {
		if got, err := Score(context.Background(), input, truth, mapping); got != nil || !errors.Is(err, ErrReference) {
			t.Fatal("bad mapping accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := Score(ctx, input, truth, nil); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if got, err := Match(ctx, truth, diarization.Timeline{}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("mapping cancellation lost")
	}
}

func TestSummedTurnDurationOverflowFailsWithoutPartialScore(t *testing.T) {
	duration := time.Duration(math.MaxInt64)
	input := document([]Span{seconds(0, 1), seconds(0, 1)}, []string{"x", "x"})
	input.Recording.DurationMS = duration.Milliseconds()
	input.SourceChunks[0].EndMS = duration.Milliseconds()
	for i := range input.Turns {
		input.Turns[i].EndMS = duration.Milliseconds()
		input.SourceChunks[0].RawTurns[i].EndMS = duration.Milliseconds()
	}
	truth := Reference{Duration: duration, Speech: []Speech{{Span{0, duration}, "A"}}}
	if report, err := Score(context.Background(), input, truth, map[string]string{"x": "A"}); report != nil || !errors.Is(err, ErrReference) {
		t.Fatal("overflow produced a partial or wrapped metric")
	}
}

func FuzzScore(f *testing.F) {
	f.Add(int64(0), int64(time.Second), uint8(0))
	f.Add(int64(5*time.Second), int64(12*time.Second), uint8(1))
	f.Add(int64(30*time.Second), int64(40*time.Second), uint8(2))
	f.Fuzz(func(t *testing.T, start, end int64, state uint8) {
		input := document([]Span{{time.Duration(start), time.Duration(end)}}, []string{[]string{"x", "y", ""}[state%3]})
		truth := Reference{Duration: 120 * time.Second, Speech: []Speech{{seconds(0, 10), "A"}, {seconds(5, 15), "B"}}, Silence: []Span{seconds(30, 40)}}
		report, err := Score(context.Background(), input, truth, map[string]string{"x": "A", "y": "B"})
		if err != nil {
			if report != nil {
				t.Fatal("invalid score returned partial report")
			}
			return
		}
		for _, b := range []Bucket{report.Assignable, report.Ambiguous, report.Silence, report.Unscored} {
			if b.Correct.Turns > b.Assigned.Turns || b.Assigned.Turns+b.Unknown.Turns != b.Total.Turns || b.Assigned.Nanoseconds+b.Unknown.Nanoseconds != b.Total.Nanoseconds {
				t.Fatal("invalid score totals")
			}
		}
		for _, rate := range []Rate{report.KnownCoverage, report.AssignedPrecision, report.FalseAttribution, report.UnknownOnAssignable, report.ConservativeAbstention} {
			for _, value := range []*float64{rate.Count, rate.Duration} {
				if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1) {
					t.Fatal("invalid score ratio")
				}
			}
		}
	})
}
