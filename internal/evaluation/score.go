package evaluation

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/teapot2/turnbeam/internal/transcript"
)

// Total reports both turn counts and summed turn-duration, not unique audio time.
type Total struct {
	Turns       int64 `json:"turns"`
	Nanoseconds int64 `json:"turnNanoseconds"`
}

// Bucket separates assigned, correct and abstained turns within one truth state.
type Bucket struct {
	Total    Total `json:"total"`
	Assigned Total `json:"assigned"`
	Correct  Total `json:"correct"`
	Unknown  Total `json:"unknown"`
}

// Rate reports both weights; undefined denominators produce null, not success.
type Rate struct {
	Count    *float64 `json:"count"`
	Duration *float64 `json:"duration"`
}

// Report keeps ambiguous/silent/unscored data separate from assignable speech.
type Report struct {
	Assignable             Bucket       `json:"assignable"`
	Ambiguous              Bucket       `json:"overlapOrTransition"`
	Silence                Bucket       `json:"verifiedSilence"`
	Unscored               Bucket       `json:"unscored"`
	ZeroDuration           int          `json:"zeroDurationTurns"`
	KnownCoverage          Rate         `json:"knownCoverage"`
	AssignedPrecision      Rate         `json:"assignedPrecision"`
	FalseAttribution       Rate         `json:"falseAttributionOnAssigned"`
	UnknownOnAssignable    Rate         `json:"unknownOnAssignable"`
	ConservativeAbstention Rate         `json:"conservativeAbstention"`
	Returns                []ReturnPair `json:"returnPairs"`
}

func (t *Total) add(duration time.Duration) error {
	if t.Turns == math.MaxInt64 || int64(duration) > math.MaxInt64-t.Nanoseconds {
		return ErrReference
	}
	t.Turns++
	t.Nanoseconds += int64(duration)
	return nil
}
func ratio(a, b int64) *float64 {
	if b == 0 {
		return nil
	}
	value := float64(a) / float64(b)
	return &value
}
func rate(a, b Total) Rate        { return Rate{ratio(a.Turns, b.Turns), ratio(a.Nanoseconds, b.Nanoseconds)} }
func sum(a, b Total) Total        { return Total{a.Turns + b.Turns, a.Nanoseconds + b.Nanoseconds} }
func difference(a, b Total) Total { return Total{a.Turns - b.Turns, a.Nanoseconds - b.Nanoseconds} }

// Score never treats an unannotated gap as silence. Multi-person turns, including
// sequential transitions, require Unknown without inventing word boundaries.
// Unmapped assignments are wrong; undefined precision is null, not perfect.
func Score(ctx context.Context, input *transcript.Transcript, truth Reference, mapping map[string]string) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := transcript.Validate(input); err != nil {
		return nil, errors.Join(ErrReference, err)
	}
	r, err := prepare(ctx, truth)
	if err != nil {
		return nil, err
	}
	if truth.Duration.Milliseconds() != input.Recording.DurationMS {
		return nil, ErrReference
	}
	used := map[string]bool{}
	for _, id := range mapping {
		if _, ok := r.speech[id]; !ok || used[id] {
			return nil, ErrReference
		}
		used[id] = true
	}
	report := &Report{Returns: []ReturnPair{}}
	var all Total
	for _, turn := range input.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		window := Span{time.Duration(turn.StartMS) * time.Millisecond, time.Duration(turn.EndMS) * time.Millisecond}
		length := window.End - window.Start
		if err := all.add(length); err != nil {
			return nil, err
		}
		var refs []string
		if length > 0 {
			for _, id := range r.ids {
				if intersection([]Span{window}, r.speech[id]) > 0 {
					refs = append(refs, id)
				}
			}
		} else {
			report.ZeroDuration++
		}
		bucket := &report.Unscored
		switch {
		case len(refs) == 1:
			bucket = &report.Assignable
		case len(refs) > 1:
			bucket = &report.Ambiguous
		case length > 0 && intersection([]Span{window}, r.silence) == length:
			bucket = &report.Silence
		}
		if err := bucket.Total.add(length); err != nil {
			return nil, err
		}
		if turn.SpeakerID == nil {
			if err := bucket.Unknown.add(length); err != nil {
				return nil, err
			}
		} else {
			if err := bucket.Assigned.add(length); err != nil {
				return nil, err
			}
			if len(refs) == 1 && mapping[*turn.SpeakerID] == refs[0] {
				if err := bucket.Correct.add(length); err != nil {
					return nil, err
				}
			}
		}
	}
	assigned := sum(sum(report.Assignable.Assigned, report.Ambiguous.Assigned), report.Silence.Assigned)
	unsafeTotal := sum(report.Ambiguous.Total, report.Silence.Total)
	report.KnownCoverage = rate(report.Assignable.Assigned, report.Assignable.Total)
	report.AssignedPrecision = rate(report.Assignable.Correct, assigned)
	report.FalseAttribution = rate(difference(assigned, report.Assignable.Correct), assigned)
	report.UnknownOnAssignable = rate(report.Assignable.Unknown, report.Assignable.Total)
	report.ConservativeAbstention = rate(sum(report.Ambiguous.Unknown, report.Silence.Unknown), unsafeTotal)
	report.Returns, err = returns(ctx, input, r, mapping)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return report, nil
}
