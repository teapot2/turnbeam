package alignment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/transcript"
)

var ErrAlignment = errors.New("invalid speaker alignment input")

// Policy contains temporal thresholds, never confidence scores. A turn is
// assigned to its main speaker only when that speaker holds at least
// MinSharePercent of all detected speech inside the turn (overlapping speakers
// each count) and covers at least MinCoveragePercent of the turn's duration.
type Policy struct {
	MinSharePercent    int
	MinCoveragePercent int
}

// DefaultPolicy was calibrated against human word-level annotations for the
// full AMI meeting ES2002a (210 turns). No threshold mislabeled a turn only one
// person spoke in; 85% share labels more turns dominated by one speaker than
// 90% without labeling more turns where someone else spoke over 20%, while 80%
// nearly doubled those. Pauses inside one person's turn are tolerated.
func DefaultPolicy() Policy {
	return Policy{MinSharePercent: 85, MinCoveragePercent: 30}
}

// Align attributes complete assembled turns against one full-recording timeline.
// It never splits, retimes or rewrites text, and ignores request-local Meta labels.
// Returned data is detached; errors/cancellation return no partial transcript.
// Existing display names survive only an identical ordered global-ID mapping.
func Align(ctx context.Context, input *transcript.Transcript, timeline diarization.Timeline, policy Policy) (*transcript.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A share above half guarantees a single main speaker.
	if policy.MinSharePercent <= 50 || policy.MinSharePercent > 100 || policy.MinCoveragePercent < 1 || policy.MinCoveragePercent > 100 {
		return nil, ErrAlignment
	}
	if err := transcript.Validate(input); err != nil {
		return nil, errors.Join(ErrAlignment, err)
	}
	if timeline.Duration <= 0 || timeline.Duration.Milliseconds() != input.Recording.DurationMS {
		return nil, fmt.Errorf("%w: recording/timeline duration mismatch", ErrAlignment)
	}
	spans, err := indexTimeline(ctx, timeline)
	if err != nil {
		return nil, err
	}
	if len(input.Speakers) != 0 {
		if len(input.Speakers) != len(timeline.Speakers) {
			return nil, fmt.Errorf("%w: existing global speaker mapping changed", ErrAlignment)
		}
		for i, speaker := range input.Speakers {
			if speaker.ID != timeline.Speakers[i].ID || speaker.Index != timeline.Speakers[i].Index {
				return nil, fmt.Errorf("%w: existing global speaker mapping changed", ErrAlignment)
			}
		}
	}
	out := *input
	out.Speakers = make([]transcript.Speaker, len(timeline.Speakers))
	for i, speaker := range timeline.Speakers {
		out.Speakers[i] = transcript.Speaker{ID: speaker.ID, Index: speaker.Index}
		if len(input.Speakers) != 0 {
			out.Speakers[i].Name = copyString(input.Speakers[i].Name)
		}
	}
	out.SourceChunks = make([]transcript.SourceChunk, len(input.SourceChunks))
	for i, c := range input.SourceChunks {
		out.SourceChunks[i] = c
		out.SourceChunks[i].RawTurns, err = copyRawTurns(ctx, c.RawTurns)
		if err != nil {
			return nil, err
		}
	}
	out.Turns = make([]transcript.Turn, len(input.Turns))
	for i, turn := range input.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start, end := time.Duration(turn.StartMS)*time.Millisecond, time.Duration(turn.EndMS)*time.Millisecond
		var candidates []string
		var best, total time.Duration
		main, tied := -1, false
		for s, intervals := range spans {
			if start == end {
				break
			}
			first := sort.Search(len(intervals), func(j int) bool { return intervals[j].end > start })
			var overlap time.Duration
			for j := first; j < len(intervals) && intervals[j].start < end; j++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				overlap += min(end, intervals[j].end) - max(start, intervals[j].start)
			}
			if overlap <= 0 {
				continue
			}
			candidates = append(candidates, timeline.Speakers[s].ID)
			total = min(total, math.MaxInt64-overlap) + overlap // saturate instead of overflowing
			switch {
			case overlap > best:
				best, main, tied = overlap, s, false
			case overlap == best:
				tied = true
			}
		}
		turn.SpeakerID = nil
		turn.Attribution = transcript.Attribution{Status: transcript.Unknown, Reason: transcript.AmbiguousOverlapOrBoundary, CandidateSpeakerIDs: candidates}
		if start == end {
			turn.Attribution.Reason = transcript.InsufficientTiming
		} else if main >= 0 && !tied && best >= percentOf(total, policy.MinSharePercent) && best >= percentOf(end-start, policy.MinCoveragePercent) {
			id := timeline.Speakers[main].ID
			turn.SpeakerID = &id
			turn.Attribution = transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}
		}
		out.Turns[i] = turn
	}
	if err := transcript.Validate(&out); err != nil {
		return nil, errors.Join(ErrAlignment, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &out, nil
}

// percentOf returns ceil(d*percent/100) without overflowing long durations.
func percentOf(d time.Duration, percent int) time.Duration {
	p := time.Duration(percent)
	return d/100*p + (d%100*p+99)/100
}

type speakerSpan struct{ start, end time.Duration }

// Merge each speaker's sorted intervals before querying them so duplicate or
// overlapping RTTM records cannot inflate coverage. Gaps remain genuine gaps.
func indexTimeline(ctx context.Context, timeline diarization.Timeline) ([][]speakerSpan, error) {
	if timeline.Speakers == nil || timeline.Segments == nil {
		return nil, fmt.Errorf("%w: missing timeline collections", ErrAlignment)
	}
	byID, rawIDs := map[string]int{}, map[string]bool{}
	for i, s := range timeline.Speakers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		labels := strings.Fields(s.RawID)
		if s.ID != fmt.Sprintf("speaker_%04d", i+1) || s.Index != i+1 || !utf8.ValidString(s.RawID) || len(labels) != 1 || labels[0] != s.RawID || strings.IndexFunc(s.RawID, func(r rune) bool { return r <= ' ' || r == 127 }) >= 0 || rawIDs[s.RawID] {
			return nil, fmt.Errorf("%w: invalid global speaker mapping", ErrAlignment)
		}
		byID[s.ID], rawIDs[s.RawID] = i, true
	}
	spans := make([][]speakerSpan, len(timeline.Speakers))
	first := make([]time.Duration, len(spans))
	var previous time.Duration
	for _, segment := range timeline.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		i, exists := byID[segment.SpeakerID]
		if !exists || segment.Start < previous || segment.Start < 0 || segment.Start >= segment.End || segment.End > timeline.Duration {
			return nil, fmt.Errorf("%w: invalid or unordered timeline interval", ErrAlignment)
		}
		previous = segment.Start
		n := len(spans[i])
		if n == 0 {
			first[i] = segment.Start
		}
		if n != 0 && segment.Start <= spans[i][n-1].end {
			spans[i][n-1].end = max(spans[i][n-1].end, segment.End)
		} else {
			spans[i] = append(spans[i], speakerSpan{segment.Start, segment.End})
		}
	}
	for i, s := range timeline.Speakers {
		if len(spans[i]) == 0 || i > 0 && (first[i] < first[i-1] || first[i] == first[i-1] && s.RawID < timeline.Speakers[i-1].RawID) {
			return nil, fmt.Errorf("%w: speakers are not in first-appearance order", ErrAlignment)
		}
	}
	return spans, nil
}
