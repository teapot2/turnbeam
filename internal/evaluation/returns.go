package evaluation

import (
	"context"
	"time"

	"github.com/teapot2/turnbeam/internal/transcript"
)

// ReturnPair reports every qualifying reference return, with support visible.
// A retained pair means one correctly mapped ID on each exclusive neighbor,
// not a promise of complete coverage or independently verified human identity.
type ReturnPair struct {
	Reference       string        `json:"referenceSpeaker"`
	Gap             time.Duration `json:"gapNanoseconds"`
	BeforeExclusive time.Duration `json:"beforeExclusiveNanoseconds"`
	AfterExclusive  time.Duration `json:"afterExclusiveNanoseconds"`
	BeforeAssigned  time.Duration `json:"beforeAssignedNanoseconds"`
	AfterAssigned   time.Duration `json:"afterAssignedNanoseconds"`
	Status          string        `json:"status"`
}

func returns(ctx context.Context, input *transcript.Transcript, r reference, mapping map[string]string) ([]ReturnPair, error) {
	assigned := map[string][]Span{}
	for _, t := range input.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t.SpeakerID != nil && t.EndMS > t.StartMS {
			assigned[*t.SpeakerID] = append(assigned[*t.SpeakerID], Span{time.Duration(t.StartMS) * time.Millisecond, time.Duration(t.EndMS) * time.Millisecond})
		}
	}
	for id, spans := range assigned {
		assigned[id] = merge(spans)
	}
	identify := func(exclusive []Span) (string, time.Duration, int) {
		var id string
		var support time.Duration
		count := 0
		for key, spans := range assigned {
			if overlap := intersection(exclusive, spans); overlap > 0 {
				id = key
				support = overlap
				count++
			}
		}
		if count != 1 {
			return "", 0, count
		}
		return id, support, count
	}
	var out = []ReturnPair{}
	for _, ref := range r.ids {
		spans := r.speech[ref]
		for i := 1; i < len(spans); i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			a, b := spans[i-1], spans[i]
			if b.Start-a.End < 30*time.Second || a.End-a.Start < 500*time.Millisecond || b.End-b.Start < 500*time.Millisecond {
				continue
			}
			before, after := exclusive(a, ref, r), exclusive(b, ref, r)
			pair := ReturnPair{Reference: ref, Gap: b.Start - a.End, Status: "unscored_no_exclusive_reference"}
			for _, s := range before {
				pair.BeforeExclusive += s.End - s.Start
			}
			for _, s := range after {
				pair.AfterExclusive += s.End - s.Start
			}
			if pair.BeforeExclusive > 0 && pair.AfterExclusive > 0 {
				left, lc, ln := identify(before)
				right, rc, rn := identify(after)
				pair.BeforeAssigned, pair.AfterAssigned = lc, rc
				switch {
				case ln == 0 || rn == 0:
					pair.Status = "unavailable_or_unknown"
				case ln != 1 || rn != 1:
					pair.Status = "ambiguous_assignments"
				case left == right && mapping[left] == ref:
					pair.Status = "retained"
				default:
					pair.Status = "mismatched"
				}
			}
			out = append(out, pair)
		}
	}
	return out, ctx.Err()
}
