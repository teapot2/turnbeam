package alignment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

var localPipeline = transcript.Pipeline{Engine: transcript.EngineLocal, ASRModel: "asr.gguf", ASRRevision: "pinned",
	DiarizationModel: "local", DiarizationRevision: "pinned", RuntimeVersion: "pinned"}

func localRecording(durationMS int64) transcript.Recording {
	return transcript.Recording{Name: "meeting.wav", DurationMS: durationMS, SHA256: strings.Repeat("ab", 32)}
}

// A speaks 0–3 s and 8–20 s, B 3–6 s.
const wordRows = "SPEAKER recording 1 0.000 3.000 <NA> <NA> A <NA> <NA>\n" +
	"SPEAKER recording 1 3.000 3.000 <NA> <NA> B <NA> <NA>\n" +
	"SPEAKER recording 1 8.000 12.000 <NA> <NA> A <NA> <NA>\n"

func TestAlignWordsGroupsWordsByOutcome(t *testing.T) {
	const a, b = "speaker_0001", "speaker_0002"
	words := []Word{
		{"Hello", ms(500), ms(900)},
		{" there. ", ms(1000), ms(1400)},
		{"Both", ms(2800), ms(3200)},    // half A, half B: not attributable
		{"Hi.", ms(3200), ms(3600)},     // B
		{"um", ms(6500), ms(6700)},      // nobody detected
		{"Again", ms(8000), ms(8400)},   // A again
		{"later", ms(10000), ms(10400)}, // same speaker after a 1.6 s pause
		{"tick", ms(10500), ms(10500)},  // zero length
		{"precise", 12000400 * time.Microsecond, 12300600 * time.Microsecond},
	}
	got, err := AlignWords(context.Background(), localRecording(20000), localPipeline, words, parsedTimeline(t, 20000, wordRows), DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		start, end int64
		text       string
		speaker    string
		reason     transcript.AttributionReason
		candidates []string
	}
	want := []row{
		{500, 1400, "Hello there.", a, "", nil},
		{2800, 3200, "Both", "", transcript.AmbiguousOverlapOrBoundary, []string{a, b}},
		{3200, 3600, "Hi.", b, "", nil},
		{6500, 6700, "um", "", transcript.AmbiguousOverlapOrBoundary, nil},
		{8000, 8400, "Again", a, "", nil},
		{10000, 10400, "later", a, "", nil},
		{10500, 10500, "tick", "", transcript.InsufficientTiming, nil},
		{12000, 12301, "precise", a, "", nil},
	}
	var rows []row
	for _, turn := range got.Turns {
		r := row{turn.StartMS, turn.EndMS, turn.Text, "", turn.Attribution.Reason, turn.Attribution.CandidateSpeakerIDs}
		if turn.SpeakerID != nil {
			r.speaker = *turn.SpeakerID
			if turn.Attribution.Method != transcript.WordTimelineOverlap {
				t.Fatalf("assigned turn has method %q", turn.Attribution.Method)
			}
		}
		rows = append(rows, r)
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("turns:\n got %+v\nwant %+v", rows, want)
	}
	if len(got.Speakers) != 2 || len(got.SourceChunks) != 0 || got.Turns[0].ID != "turn_000001" || got.SchemaVersion != transcript.SchemaVersion {
		t.Fatalf("unexpected transcript shape: %+v", got)
	}
	if _, err := transcript.Encode(got); err != nil {
		t.Fatal(err)
	}
}

func TestAlignWordsWithoutSpeech(t *testing.T) {
	got, err := AlignWords(context.Background(), localRecording(5000), localPipeline, nil, parsedTimeline(t, 5000, ""), DefaultPolicy())
	if err != nil || len(got.Turns) != 0 || got.Turns == nil {
		t.Fatalf("silent recording: %+v, %v", got, err)
	}
}

func TestAlignWordsRejectsInvalidInput(t *testing.T) {
	timeline := parsedTimeline(t, 20000, wordRows)
	good := []Word{{"one", ms(100), ms(200)}, {"two", ms(300), ms(400)}}
	for name, tc := range map[string]struct {
		words      []Word
		pipeline   transcript.Pipeline
		policy     Policy
		durationMS int64
	}{
		"unordered":       {[]Word{good[1], good[0]}, localPipeline, DefaultPolicy(), 20000},
		"past recording":  {[]Word{{"late", ms(19900), ms(20001)}}, localPipeline, DefaultPolicy(), 20000},
		"negative start":  {[]Word{{"early", ms(-1), ms(10)}}, localPipeline, DefaultPolicy(), 20000},
		"inverted":        {[]Word{{"back", ms(200), ms(100)}}, localPipeline, DefaultPolicy(), 20000},
		"empty text":      {[]Word{{" ", ms(100), ms(200)}}, localPipeline, DefaultPolicy(), 20000},
		"invalid UTF-8":   {[]Word{{string([]byte{0xff}), ms(100), ms(200)}}, localPipeline, DefaultPolicy(), 20000},
		"Meta pipeline":   {good, transcript.Pipeline{Engine: transcript.EngineMeta}, DefaultPolicy(), 20000},
		"weak policy":     {good, localPipeline, Policy{MinSharePercent: 50, MinCoveragePercent: 30}, 20000},
		"length mismatch": {good, localPipeline, DefaultPolicy(), 19000},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := AlignWords(context.Background(), localRecording(tc.durationMS), tc.pipeline, tc.words, timeline, tc.policy)
			if got != nil || !errors.Is(err, ErrAlignment) {
				t.Fatalf("expected an alignment error, got %+v, %v", got, err)
			}
		})
	}
}

func TestAlignWordsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := AlignWords(ctx, localRecording(20000), localPipeline, []Word{{"one", ms(100), ms(200)}}, parsedTimeline(t, 20000, wordRows), DefaultPolicy())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled alignment returned %+v, %v", got, err)
	}
}

// A recording 0.5 ms past a whole millisecond: a word in that final half
// millisecond has no whole-millisecond duration, so it must stay Unknown
// instead of failing the run.
func TestAlignWordsFinalSubMillisecondWord(t *testing.T) {
	duration := 10*time.Second + 500*time.Microsecond
	timeline, err := diarization.ParseRTTM(context.Background(), strings.NewReader("SPEAKER recording 1 0.000 10.0005 <NA> <NA> A <NA> <NA>\n"), "recording", duration)
	if err != nil {
		t.Fatal(err)
	}
	words := []Word{{"Hello", ms(1000), ms(1400)}, {"bye", 10*time.Second + 100*time.Microsecond, duration}}
	got, err := AlignWords(context.Background(), localRecording(10000), localPipeline, words, timeline, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	last := got.Turns[len(got.Turns)-1]
	if len(got.Turns) != 2 || last.StartMS != 10000 || last.EndMS != 10000 || last.SpeakerID != nil || last.Attribution.Reason != transcript.InsufficientTiming {
		t.Fatalf("final word: %+v", got.Turns)
	}
}
