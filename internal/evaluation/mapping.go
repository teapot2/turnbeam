package evaluation

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/teapot2/turnbeam/internal/diarization"
)

// Match computes one maximum-overlap, one-to-one mapping for a whole recording.
// Reuse this mapping across policies/chunks; never optimize it on individual turns.
// At most eight speakers per side bounds the exhaustive assignment search.
func Match(ctx context.Context, truth Reference, timeline diarization.Timeline) (map[string]string, error) {
	r, err := prepare(ctx, truth)
	if err != nil {
		return nil, err
	}
	if timeline.Duration <= 0 || timeline.Duration.Milliseconds() != truth.Duration.Milliseconds() || len(r.ids) > 8 || len(timeline.Speakers) > 8 {
		return nil, ErrReference
	}
	hyp := map[string][]Span{}
	for _, s := range timeline.Speakers {
		if s.ID == "" {
			return nil, ErrReference
		}
		if _, ok := hyp[s.ID]; ok {
			return nil, ErrReference
		}
		hyp[s.ID] = nil
	}
	for _, s := range timeline.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := hyp[s.SpeakerID]; !ok || s.Start < 0 || s.Start >= s.End || s.End > timeline.Duration {
			return nil, ErrReference
		}
		hyp[s.SpeakerID] = append(hyp[s.SpeakerID], Span{s.Start, s.End})
	}
	var ids []string
	for id, spans := range hyp {
		ids = append(ids, id)
		hyp[id] = merge(spans)
	}
	sort.Strings(ids)
	weights := make([][]time.Duration, len(ids))
	for i, id := range ids {
		weights[i] = make([]time.Duration, len(r.ids))
		for j, ref := range r.ids {
			weights[i][j] = intersection(hyp[id], r.speech[ref])
		}
	}
	bestScore := time.Duration(-1)
	var best []int
	current := make([]int, len(ids))
	var visit func(int, uint16, time.Duration) error
	visit = func(i int, used uint16, score time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == len(ids) {
			if score > bestScore {
				bestScore = score
				best = append([]int(nil), current...)
			}
			return nil
		}
		for j, weight := range weights[i] {
			if used&(1<<j) != 0 || weight == 0 {
				continue
			}
			if weight > time.Duration(math.MaxInt64)-score {
				return ErrReference
			}
			current[i] = j
			if err := visit(i+1, used|1<<j, score+weight); err != nil {
				return err
			}
		}
		current[i] = -1
		return visit(i+1, used, score)
	}
	if err = visit(0, 0, 0); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, j := range best {
		if j >= 0 {
			out[ids[i]] = r.ids[j]
		}
	}
	return out, nil
}
