// Package chunking plans contiguous recording coverage without editing audio.
package chunking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
)

const (
	Version         = "speech-aware-v1"
	MinChunkSamples = 8 * 60 * media.SampleRate
	TargetSamples   = (8*60 + 30) * media.SampleRate
	MaxChunkSamples = media.MaxChunkSamples
	OverlapSamples  = media.SampleRate / 2
	MinPauseSamples = int64(media.MinSilence) * media.SampleRate / int64(time.Second)
	MaxRequestBytes = media.MaxRequestBytes
	// Includes WAV headers and multipart framing; extraction/upload must verify
	// actual sizes rather than treating this planning allowance as a measurement.
	RequestHeadroom = media.RequestHeadroom
	Pause           = "pause"
	Forced          = "forced"
	End             = "end"
	maxPlanChunks   = 10000
)

var ErrPlan = errors.New("invalid chunk planning input")

// Chunk is a half-open range on the original 24-kHz sample clock. Boundary
// describes its end; OverlapSamples records repetition with the previous chunk.
type Chunk struct {
	ID             string `json:"id"`
	StartSample    int64  `json:"startSample"`
	EndSample      int64  `json:"endSample"`
	OverlapSamples int64  `json:"overlapSamples"`
	Boundary       string `json:"boundary"`
}

// Milliseconds floor the absolute sample position. Internal boundaries are
// millisecond-aligned; a sub-ms recording tail remains exact in EndSample.
func (c Chunk) StartMS() int64            { return c.StartSample / (media.SampleRate / 1000) }
func (c Chunk) EndMS() int64              { return c.EndSample / (media.SampleRate / 1000) }
func (c Chunk) RequestBytesBudget() int64 { return (c.EndSample-c.StartSample)*2 + RequestHeadroom }

type span struct{ start, end int64 }

// Plan intersects waveform silence with the complement of ALL speakers' activity.
// A diarization gap alone is never sufficient silence evidence. Inputs are not
// mutated, and cancellation or invalid evidence returns no partial plan.
func Plan(ctx context.Context, samples int64, timeline diarization.Timeline, silence []media.Silence) ([]Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if samples < 0 || timeline.Duration < 0 || samples != sampleCeil(timeline.Duration) {
		return nil, ErrPlan
	}
	pauses, err := confirmedPauses(ctx, timeline, silence, samples)
	if err != nil {
		return nil, err
	}
	chunks := []Chunk{}
	var start, previousEnd int64
	p := 0
	for start < samples {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end, boundary := samples, End
		if samples-start > MaxChunkSamples {
			end, boundary = start+MaxChunkSamples, Forced
			bestDistance := int64(MaxChunkSamples)
			for p < len(pauses) && pauses[p].end < start+MinChunkSamples {
				p++
			}
			for i := p; i < len(pauses) && pauses[i].start <= start+MaxChunkSamples; i++ {
				pause := pauses[i]
				// Keep at least half the minimum pause on either side of the cut.
				low := max(start+MinChunkSamples, pause.start+MinPauseSamples/2)
				high := min(start+MaxChunkSamples, pause.end-MinPauseSamples/2)
				const ms = media.SampleRate / 1000
				low, high = (low+ms-1)/ms*ms, high/ms*ms
				if low > high {
					continue
				}
				candidate := min(max(start+TargetSamples, low), high)
				distance := candidate - (start + TargetSamples)
				if distance < 0 {
					distance = -distance
				}
				if distance < bestDistance || distance == bestDistance && candidate < end {
					end, boundary, bestDistance = candidate, Pause, distance
				}
			}
		}
		chunk := Chunk{ID: fmt.Sprintf("chunk_%04d", len(chunks)+1), StartSample: start, EndSample: end,
			OverlapSamples: max(0, previousEnd-start), Boundary: boundary}
		if end-start > MaxChunkSamples || chunk.RequestBytesBudget() >= MaxRequestBytes {
			return nil, fmt.Errorf("%w: chunk exceeds request budget", ErrPlan)
		}
		chunks = append(chunks, chunk)
		if len(chunks) > maxPlanChunks {
			return nil, fmt.Errorf("%w: too many chunks", ErrPlan)
		}
		start, previousEnd = end, end
		if boundary == Forced {
			start -= OverlapSamples
		}
	}
	return chunks, nil
}

func sampleFloor(d time.Duration) int64 {
	return int64(d/time.Second)*media.SampleRate + int64(d%time.Second)*media.SampleRate/int64(time.Second)
}

func sampleCeil(d time.Duration) int64 {
	return int64(d/time.Second)*media.SampleRate + (int64(d%time.Second)*media.SampleRate+int64(time.Second)-1)/int64(time.Second)
}

func confirmedPauses(ctx context.Context, timeline diarization.Timeline, silence []media.Silence, samples int64) ([]span, error) {
	speech := make([]span, 0, len(timeline.Segments))
	for _, segment := range timeline.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if segment.Start < 0 || segment.Start >= segment.End || segment.End > timeline.Duration {
			return nil, ErrPlan
		}
		// Round activity outward and quiet evidence inward, never widening a pause.
		speech = append(speech, span{sampleFloor(segment.Start), sampleCeil(segment.End)})
	}
	sort.Slice(speech, func(i, j int) bool { return speech[i].start < speech[j].start })
	gaps := []span{}
	var cursor int64
	for _, s := range speech {
		if s.start > cursor {
			gaps = append(gaps, span{cursor, s.start})
		}
		cursor = max(cursor, s.end)
	}
	if cursor < samples {
		gaps = append(gaps, span{cursor, samples})
	}
	pauses := []span{}
	var last time.Duration
	g := 0
	for _, quiet := range silence {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if quiet.Start < last || quiet.Start >= quiet.End || quiet.End > timeline.Duration {
			return nil, ErrPlan
		}
		last = quiet.End
		q := span{sampleCeil(quiet.Start), sampleFloor(quiet.End)}
		for g < len(gaps) && gaps[g].end <= q.start {
			g++
		}
		for i := g; i < len(gaps) && gaps[i].start < q.end; i++ {
			p := span{max(q.start, gaps[i].start), min(q.end, gaps[i].end)}
			if p.end-p.start >= MinPauseSamples {
				pauses = append(pauses, p)
			}
		}
	}
	return pauses, nil
}
