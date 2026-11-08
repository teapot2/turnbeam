package diarization

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func row(start, duration, speaker string) string {
	return fmt.Sprintf("SPEAKER recording 1 %s %s <NA> <NA> %s <NA> <NA>\n", start, duration, speaker)
}

func TestTimelineMappingAndChronology(t *testing.T) {
	lines := []string{
		row("600.100", "0.500", "native_b"),
		row("0.100", "0.200", "native_a"),
		row("539.990", "0.030", "native_b"),
		row("0.000", "0.200", "native_b"),
		row("1.001", "0.003", "native_a"),
	}
	got, err := ParseRTTM(context.Background(), strings.NewReader(strings.Join(lines, "")), recordingID, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wantSpeakers := []Speaker{{ID: "speaker_0001", Index: 1, RawID: "native_b"}, {ID: "speaker_0002", Index: 2, RawID: "native_a"}}
	if !reflect.DeepEqual(got.Speakers, wantSpeakers) {
		t.Fatalf("wrong first-appearance mapping: %+v", got.Speakers)
	}
	if len(got.Segments) != 5 || got.Segments[0].Start != 0 || got.Segments[1].Start != 100*time.Millisecond || got.Segments[1].End != 300*time.Millisecond {
		t.Fatalf("overlap or exact decimal arithmetic changed: %+v", got.Segments)
	}
	// One label crosses a proposed remote-request boundary and returns after it.
	for _, index := range []int{0, 3, 4} {
		if got.Segments[index].SpeakerID != "speaker_0001" {
			t.Fatal("speaker identity reset at a remote chunk boundary")
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	again, err := ParseRTTM(context.Background(), strings.NewReader(strings.Join(lines, "")), recordingID, 20*time.Minute)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("input ordering changed the timeline: %v", err)
	}
}

func TestFirstAppearanceTiesAndSilence(t *testing.T) {
	got, err := ParseRTTM(context.Background(), strings.NewReader(row("0", "0.1", "z")+row("0", "0.2", "a")), recordingID, time.Second)
	if err != nil || got.Speakers[0].RawID != "a" || got.Speakers[1].RawID != "z" {
		t.Fatalf("unstable simultaneous-speaker mapping: %+v, %v", got.Speakers, err)
	}
	for _, input := range []string{"", "\n# native silence\n\n"} {
		got, err := ParseRTTM(context.Background(), strings.NewReader(input), recordingID, time.Second)
		if err != nil || got.Duration != time.Second || got.Speakers == nil || got.Segments == nil || len(got.Speakers) != 0 || len(got.Segments) != 0 {
			t.Fatalf("empty/silent output became an invented interval: %+v, %v", got, err)
		}
	}
}

func TestExplicitEndAdjustment(t *testing.T) {
	input := "# header\n\n" + row("1.900", "0.110", "native")
	got, err := ParseRTTM(context.Background(), strings.NewReader(input), recordingID, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []EndAdjustment{{Line: 3, RawEnd: 2010 * time.Millisecond, End: 2 * time.Second}}
	if !reflect.DeepEqual(got.Adjustments, want) || got.Segments[0].End != 2*time.Second {
		t.Fatalf("native padding was silently changed: %+v", got)
	}
	// A non-millisecond recording boundary is preserved, not rounded or extended.
	got, err = ParseRTTM(context.Background(), strings.NewReader(row("1.9", "0.11", "native")), recordingID, 2*time.Second+time.Nanosecond)
	if err != nil || got.Segments[0].End != 2*time.Second+time.Nanosecond {
		t.Fatalf("exact recording boundary was lost: %+v, %v", got, err)
	}
}

func TestExactTimeBoundaries(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"0", 0}, {"0.001", time.Millisecond}, {"0.000000001", time.Nanosecond},
		{"1.000000001", time.Second + time.Nanosecond}, {"0001.2", 1200 * time.Millisecond},
		{"9223372036.854775807", time.Duration(math.MaxInt64)},
	} {
		got, err := seconds(test.value)
		if err != nil || got != test.want {
			t.Fatalf("%q: %v, %v; want %v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "-0.1", "+1", "NaN", "Inf", "1e3", "1s", "1.2.3", "1.", ".1", "0.0000000001", "9223372036.854775808", "99999999999999999999999999999999999"} {
		if _, err := seconds(value); err == nil {
			t.Fatalf("invalid seconds accepted: %q", value)
		}
	}
}

func TestMalformedRTTM(t *testing.T) {
	for name, input := range map[string]string{
		"missing output sentinel": "INCOMPLETE\n",
		"wrong record type":       strings.Replace(row("0", "1", "a"), "SPEAKER", "LEXEME", 1),
		"wrong recording":         strings.Replace(row("0", "1", "a"), recordingID, "private-recording-name", 1),
		"wrong channel":           strings.Replace(row("0", "1", "a"), "recording 1", "recording 2", 1),
		"wrong field count":       "SPEAKER recording 1 0 1 a\n",
		"negative start":          row("-1", "1", "a"),
		"zero length":             row("0", "0", "a"),
		"negative length":         row("0", "-1", "a"),
		"nonfinite start":         row("NaN", "1", "a"),
		"nonfinite duration":      row("0", "Inf", "a"),
		"time overflow":           row("9223372036.854775807", "0.000000001", "a"),
		"start at end":            row("2", "0.001", "a"),
		"start after end":         row("2.001", "0.001", "a"),
		"excessive overrun":       row("1.9", "0.110000001", "a"),
		"unknown native label":    row("0", "1", "<NA>"),
		"invalid label bytes":     row("0", "1", string([]byte{0xff})),
		"control label":           row("0", "1", "a\x00b"),
		"oversized line":          strings.Repeat("x", 65537),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRTTM(context.Background(), strings.NewReader(input), recordingID, 2*time.Second)
			if !errors.Is(err, ErrRTTM) || got.Duration != 0 || got.Segments != nil {
				t.Fatalf("malformed/partial data accepted: %+v, %v", got, err)
			}
			if strings.Contains(err.Error(), "private-recording-name") {
				t.Fatal("error exposed recording metadata")
			}
		})
	}
	for _, id := range []string{"", "two words", "\u00a0recording"} {
		if _, err := ParseRTTM(context.Background(), strings.NewReader(""), id, time.Second); !errors.Is(err, ErrRTTM) {
			t.Fatalf("invalid recording ID accepted: %v", err)
		}
	}
	if _, err := ParseRTTM(context.Background(), strings.NewReader(""), recordingID, 0); !errors.Is(err, ErrRTTM) {
		t.Fatal(err)
	}
}

func TestSpeakerLimitWarning(t *testing.T) {
	var input strings.Builder
	for i := 0; i < MaxSpeakers; i++ {
		input.WriteString(row(fmt.Sprint(i), "0.1", fmt.Sprint("native_", i)))
	}
	got, err := ParseRTTM(context.Background(), strings.NewReader(input.String()), recordingID, 10*time.Second)
	if err != nil || !reflect.DeepEqual(got.Warnings, []string{SpeakerLimitReached}) {
		t.Fatalf("speaker cap was not surfaced: %+v, %v", got.Warnings, err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestParserCancellationAndReaderErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParseRTTM(ctx, strings.NewReader(""), recordingID, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ParseRTTM(context.Background(), failingReader{io.ErrUnexpectedEOF}, recordingID, time.Second); !errors.Is(err, ErrRTTM) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error cause was lost: %v", err)
	}
}

func FuzzParseRTTM(f *testing.F) {
	f.Add(row("0.1", "0.2", "native"))
	f.Add(row("1.9", "0.11", "native"))
	f.Add("")
	f.Fuzz(func(t *testing.T, input string) {
		got, err := ParseRTTM(context.Background(), strings.NewReader(input), recordingID, 2*time.Second)
		if err != nil {
			if got.Segments != nil {
				t.Fatal("partial timeline returned on error")
			}
			return
		}
		known := map[string]bool{}
		for _, speaker := range got.Speakers {
			known[speaker.ID] = true
		}
		var previous time.Duration
		for _, segment := range got.Segments {
			if segment.Start < previous || segment.Start < 0 || segment.Start >= segment.End || segment.End > got.Duration || !known[segment.SpeakerID] {
				t.Fatal("invalid accepted timeline")
			}
			previous = segment.Start
		}
	})
}
