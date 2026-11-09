package alignment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

// Word is one locally recognized word on the original recording clock.
type Word struct {
	Text       string
	Start, End time.Duration
}

// MaxWordGap ends a turn at a pause at least this long, even when the same
// speaker keeps talking, so long monologues keep readable timestamps.
const MaxWordGap = 1500 * time.Millisecond

// AlignWords builds a local-engine transcript. Each word is judged on its own
// with the policy Align applies to whole Meta turns; consecutive words with the
// same outcome (the same speaker, or both unattributable) and no pause of
// MaxWordGap form one turn. Words are never retimed or rewritten, and a word the
// policy cannot attribute stays in an Unknown turn. Words must be in start-time
// order within the recording. Errors and cancellation return no partial transcript.
func AlignWords(ctx context.Context, recording transcript.Recording, pipeline transcript.Pipeline, words []Word, timeline diarization.Timeline, policy Policy) (*transcript.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !policy.valid() || pipeline.Engine != transcript.EngineLocal {
		return nil, ErrAlignment
	}
	if timeline.Duration <= 0 || timeline.Duration.Milliseconds() != recording.DurationMS {
		return nil, fmt.Errorf("%w: recording/timeline duration mismatch", ErrAlignment)
	}
	spans, err := indexTimeline(ctx, timeline)
	if err != nil {
		return nil, err
	}
	out := &transcript.Transcript{SchemaVersion: transcript.SchemaVersion, Recording: recording, Pipeline: pipeline,
		Speakers: make([]transcript.Speaker, len(timeline.Speakers)), Turns: []transcript.Turn{}, SourceChunks: []transcript.SourceChunk{}}
	for i, speaker := range timeline.Speakers {
		out.Speakers[i] = transcript.Speaker{ID: speaker.ID, Index: speaker.Index}
	}
	var current *wordTurn
	flush := func() {
		if current != nil {
			out.Turns = append(out.Turns, current.turn(len(out.Turns)+1, timeline, recording.DurationMS))
			current = nil
		}
	}
	for i, w := range words {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if w.Start < 0 || w.End < w.Start || w.End > timeline.Duration || i > 0 && w.Start < words[i-1].Start ||
			!utf8.ValidString(w.Text) || strings.TrimSpace(w.Text) == "" {
			return nil, fmt.Errorf("%w: words must be nonempty and ordered within the recording", ErrAlignment)
		}
		d, err := attribute(ctx, spans, w.Start, w.End, policy)
		if err != nil {
			return nil, err
		}
		if current == nil || current.main != d.main || w.Start-current.end >= MaxWordGap {
			flush()
			current = &wordTurn{main: d.main, start: w.Start, heard: make([]bool, len(spans))}
		}
		current.add(w, d)
	}
	flush()
	if err := transcript.Validate(out); err != nil {
		return nil, errors.Join(ErrAlignment, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// wordTurn accumulates consecutive words that share one attribution outcome.
type wordTurn struct {
	main       int
	start, end time.Duration
	text       []string
	// heard marks, by timeline index, every speaker overlapping any word.
	heard []bool
}

func (t *wordTurn) add(w Word, d decision) {
	t.text = append(t.text, strings.TrimSpace(w.Text))
	t.end = max(t.end, w.End)
	for _, s := range d.candidates {
		t.heard[s] = true
	}
}

// turn converts the group to recording milliseconds: the start rounds down and
// the end up, so the interval still contains every word. The end is capped at
// the whole-millisecond recording length, so a word in its final fraction of a
// millisecond can still end up with no duration; such a turn stays Unknown.
func (t *wordTurn) turn(n int, timeline diarization.Timeline, durationMS int64) transcript.Turn {
	startMS := t.start.Milliseconds()
	endMS := min(durationMS, int64((t.end+time.Millisecond-1)/time.Millisecond))
	turn := transcript.Turn{ID: fmt.Sprintf("turn_%06d", n), StartMS: startMS, EndMS: endMS, Text: strings.Join(t.text, " ")}
	if t.main >= 0 && startMS < endMS {
		id := timeline.Speakers[t.main].ID
		turn.SpeakerID = &id
		turn.Attribution = transcript.Attribution{Status: transcript.Assigned, Method: transcript.WordTimelineOverlap}
		return turn
	}
	turn.Attribution = transcript.Attribution{Status: transcript.Unknown, Reason: transcript.AmbiguousOverlapOrBoundary}
	if startMS == endMS {
		turn.Attribution.Reason = transcript.InsufficientTiming
	}
	for s, heard := range t.heard {
		if heard {
			turn.Attribution.CandidateSpeakerIDs = append(turn.Attribution.CandidateSpeakerIDs, timeline.Speakers[s].ID)
		}
	}
	return turn
}
