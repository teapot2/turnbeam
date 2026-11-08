package chunking

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
)

func speech(start, end time.Duration) diarization.Segment {
	return diarization.Segment{Start: start, End: end, SpeakerID: "speaker_0001"}
}

func TestPlanBoundaries(t *testing.T) {
	const s = time.Second
	for _, tc := range []struct {
		name     string
		activity []diarization.Segment
		quiet    []media.Silence
		boundary string
		end      time.Duration
	}{
		{"silent", nil, []media.Silence{{Start: 0, End: 1200 * s}}, Pause, 510 * s},
		{"no silence evidence", nil, nil, Forced, 540 * s},
		{"diarization gap only", []diarization.Segment{speech(0, 509*s), speech(511*s, 1200*s)}, nil, Forced, 540 * s},
		{"waveform only", []diarization.Segment{speech(0, 1200*s)}, []media.Silence{{Start: 0, End: 1200 * s}}, Forced, 540 * s},
		{"confirmed pause", []diarization.Segment{speech(0, 509*s), speech(511*s, 1200*s)}, []media.Silence{{Start: 509 * s, End: 511 * s}}, Pause, 510 * s},
		{"intersection not either input", []diarization.Segment{speech(0, 509*s), speech(511*s, 1200*s)}, []media.Silence{{Start: 510 * s, End: 512 * s}}, Pause, 510*s + 250*time.Millisecond},
		{"too short", nil, []media.Silence{{Start: 510 * s, End: 510*s + 499*time.Millisecond}}, Forced, 540 * s},
		{"boundary speech", []diarization.Segment{speech(0, 510*s+150*time.Millisecond), speech(510*s+600*time.Millisecond, 1200*s)}, []media.Silence{{Start: 509 * s, End: 511 * s}}, Forced, 540 * s},
		{"overlapping speaker fills gap", []diarization.Segment{speech(511*s, 1200*s), speech(509*s, 511*s), speech(0, 509*s)}, []media.Silence{{Start: 509 * s, End: 511 * s}}, Forced, 540 * s},
		{"overlap leaves real pause", []diarization.Segment{speech(0, 509*s), speech(511*s, 1200*s), speech(509*s, 510*s+400*time.Millisecond)}, []media.Silence{{Start: 509 * s, End: 511 * s}}, Pause, 510*s + 650*time.Millisecond},
		{"window start", nil, []media.Silence{{Start: 480*s - 250*time.Millisecond, End: 480*s + 250*time.Millisecond}}, Pause, 480 * s},
		{"window end", nil, []media.Silence{{Start: 540*s - 250*time.Millisecond, End: 540*s + 250*time.Millisecond}}, Pause, 540 * s},
		{"pause too late", nil, []media.Silence{{Start: 540 * s, End: 541 * s}}, Forced, 540 * s},
		{"pause too early", nil, []media.Silence{{Start: 478 * s, End: 479 * s}}, Forced, 540 * s},
		{"nearest and earlier tie", nil, []media.Silence{{Start: 508*s + 750*time.Millisecond, End: 509*s + 250*time.Millisecond}, {Start: 510*s + 750*time.Millisecond, End: 511*s + 250*time.Millisecond}}, Pause, 509 * s},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timeline := diarization.Timeline{Duration: 1200 * s, Segments: tc.activity}
			original := append([]diarization.Segment(nil), timeline.Segments...)
			chunks, err := Plan(context.Background(), 1200*media.SampleRate, timeline, tc.quiet)
			if err != nil {
				t.Fatal(err)
			}
			if chunks[0].Boundary != tc.boundary || chunks[0].EndSample != sampleFloor(tc.end) {
				t.Fatalf("first boundary: %+v; want %s at %v", chunks[0], tc.boundary, tc.end)
			}
			assertCoverage(t, chunks, 1200*media.SampleRate)
			again, err := Plan(context.Background(), 1200*media.SampleRate, timeline, tc.quiet)
			if err != nil || !reflect.DeepEqual(chunks, again) || !reflect.DeepEqual(original, timeline.Segments) {
				t.Fatal("planning mutated inputs or was nondeterministic")
			}
		})
	}
}

func assertCoverage(t *testing.T, chunks []Chunk, samples int64) {
	t.Helper()
	var end int64
	for i, c := range chunks {
		if c.ID != fmt.Sprintf("chunk_%04d", i+1) || c.StartSample < 0 || c.StartSample >= c.EndSample || c.StartSample > end || c.EndSample > samples || c.EndSample-c.StartSample > MaxChunkSamples {
			t.Fatalf("invalid coverage or limits: %+v", c)
		}
		wantOverlap := int64(0)
		if i > 0 && chunks[i-1].Boundary == Forced {
			wantOverlap = OverlapSamples
		}
		if end-c.StartSample != wantOverlap || c.OverlapSamples != wantOverlap || c.StartSample%(media.SampleRate/1000) != 0 {
			t.Fatalf("unexpected gap/overlap/offset: %+v", c)
		}
		if c.StartMS() != c.StartSample/24 || c.EndMS() != c.EndSample/24 {
			t.Fatal("wrong absolute milliseconds")
		}
		end = c.EndSample
	}
	if end != samples || len(chunks) > 0 && chunks[len(chunks)-1].Boundary != End {
		t.Fatal("recording tail lost")
	}
}

func TestEmptyShortAndExactTails(t *testing.T) {
	for _, samples := range []int64{0, 1, 23, 24, 24001, MaxChunkSamples, MaxChunkSamples + 1, MaxChunkSamples*3 + 13} {
		duration := time.Duration(samples/media.SampleRate)*time.Second + time.Duration(samples%media.SampleRate)*time.Second/media.SampleRate
		chunks, err := Plan(context.Background(), samples, diarization.Timeline{Duration: duration}, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertCoverage(t, chunks, samples)
		if samples == 0 && len(chunks) != 0 || samples > 0 && samples <= MaxChunkSamples && len(chunks) != 1 {
			t.Fatalf("unnecessary/missing chunks: %+v", chunks)
		}
	}
}

func TestPlanRejectsInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		samples  int64
		timeline diarization.Timeline
		quiet    []media.Silence
	}{
		{-1, diarization.Timeline{}, nil},
		{0, diarization.Timeline{Duration: -1}, nil},
		{1, diarization.Timeline{}, nil},
		{24000, diarization.Timeline{Duration: time.Second, Segments: []diarization.Segment{speech(-1, time.Second)}}, nil},
		{24000, diarization.Timeline{Duration: time.Second, Segments: []diarization.Segment{speech(0, 0)}}, nil},
		{24000, diarization.Timeline{Duration: time.Second, Segments: []diarization.Segment{speech(0, time.Second+1)}}, nil},
		{24000, diarization.Timeline{Duration: time.Second}, []media.Silence{{Start: -1, End: time.Second}}},
		{24000, diarization.Timeline{Duration: time.Second}, []media.Silence{{Start: 0, End: time.Second + 1}}},
		{24000, diarization.Timeline{Duration: time.Second}, []media.Silence{{Start: 0, End: 0}}},
		{24000, diarization.Timeline{Duration: time.Second}, []media.Silence{{Start: 0, End: time.Second}, {Start: 0, End: time.Second}}},
	} {
		if chunks, err := Plan(context.Background(), tc.samples, tc.timeline, tc.quiet); chunks != nil || !errors.Is(err, ErrPlan) {
			t.Fatalf("invalid evidence accepted: %+v, %v", chunks, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if chunks, err := Plan(ctx, 0, diarization.Timeline{}, nil); chunks != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestSubSampleEvidenceIsConservative(t *testing.T) {
	const duration = 1200 * time.Second
	// Only 499.999999 ms of confirmed pause: rounding must not promote it.
	quiet := []media.Silence{{Start: 510 * time.Second, End: 510*time.Second + media.MinSilence - time.Nanosecond}}
	chunks, err := Plan(context.Background(), 1200*media.SampleRate, diarization.Timeline{Duration: duration}, quiet)
	if err != nil || chunks[0].Boundary != Forced {
		t.Fatalf("widened quiet evidence: %+v, %v", chunks, err)
	}
}

func TestPlanResourceBound(t *testing.T) {
	duration := time.Duration(maxPlanChunks+1) * 9 * time.Minute
	if chunks, err := Plan(context.Background(), media.SamplesCeil(duration), diarization.Timeline{Duration: duration}, nil); chunks != nil || !errors.Is(err, ErrPlan) {
		t.Fatalf("unbounded plan accepted: %d chunks, %v", len(chunks), err)
	}
}

func FuzzPlan(f *testing.F) {
	f.Add([]byte{25, 80, 190, 45, 100})
	f.Add([]byte{0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 5 {
			return
		}
		duration := time.Duration(int(data[0])+1) * time.Minute
		qStart := duration * time.Duration(data[1]) / 256
		qEnd := qStart + (duration-qStart)*time.Duration(int(data[2])+1)/256
		sStart := duration * time.Duration(data[3]) / 256
		sEnd := sStart + (duration-sStart)*time.Duration(int(data[4])+1)/256
		timeline := diarization.Timeline{Duration: duration, Segments: []diarization.Segment{speech(sStart, sEnd)}}
		quiet := []media.Silence{{Start: qStart, End: qEnd}}
		samples := media.SamplesCeil(duration)
		chunks, err := Plan(context.Background(), samples, timeline, quiet)
		if err != nil {
			t.Fatal(err)
		}
		assertCoverage(t, chunks, samples)
		for _, c := range chunks {
			if c.Boundary != Pause {
				continue
			}
			if c.EndSample < media.SamplesCeil(qStart)+MinPauseSamples/2 || c.EndSample > sampleFloor(qEnd)-MinPauseSamples/2 || c.EndSample > sampleFloor(sStart)-MinPauseSamples/2 && c.EndSample < media.SamplesCeil(sEnd)+MinPauseSamples/2 {
				t.Fatalf("unsafe pause: %+v, quiet=%+v speech=%+v", c, quiet, timeline.Segments)
			}
		}
	})
}

func TestLongestChunkFitsOneRequest(t *testing.T) {
	if MaxChunkSamples*2+media.RequestHeadroom >= media.MaxRequestBytes {
		t.Fatal("a maximum-length chunk would exceed Meta's request size limit")
	}
}
