// Package evaluation scores attribution against independent participant references.
package evaluation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrReference identifies malformed scoring inputs, not a poor accuracy result.
var ErrReference = errors.New("invalid evaluation reference or speaker mapping")

// Span is a half-open original-recording interval.
type Span struct{ Start, End time.Duration }

// Speech binds an independent reference identity to an interval.
type Speech struct {
	Span
	Speaker string
}

// Reference distinguishes independently verified silence from unannotated gaps.
type Reference struct {
	Duration time.Duration
	Speech   []Speech
	Silence  []Span
}

type reference struct {
	ids     []string
	speech  map[string][]Span
	silence []Span
}

func prepare(ctx context.Context, input Reference) (reference, error) {
	r := reference{speech: map[string][]Span{}}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if input.Duration <= 0 {
		return r, ErrReference
	}
	valid := func(s Span) bool { return s.Start >= 0 && s.Start < s.End && s.End <= input.Duration }
	for _, row := range input.Speech {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if !valid(row.Span) || !utf8.ValidString(row.Speaker) || strings.TrimSpace(row.Speaker) == "" {
			return r, ErrReference
		}
		r.speech[row.Speaker] = append(r.speech[row.Speaker], row.Span)
	}
	for id, spans := range r.speech {
		r.ids = append(r.ids, id)
		r.speech[id] = merge(spans)
	}
	sort.Strings(r.ids)
	for _, s := range input.Silence {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if !valid(s) {
			return r, ErrReference
		}
		r.silence = append(r.silence, s)
	}
	r.silence = merge(r.silence)
	for _, spans := range r.speech {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if intersection(spans, r.silence) > 0 {
			return r, ErrReference
		}
	}
	return r, ctx.Err()
}

func merge(spans []Span) []Span {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End < spans[j].End
	})
	out := spans[:0]
	for _, s := range spans {
		if len(out) > 0 && s.Start <= out[len(out)-1].End {
			out[len(out)-1].End = max(out[len(out)-1].End, s.End)
		} else {
			out = append(out, s)
		}
	}
	return out
}

func intersection(a, b []Span) time.Duration {
	var total time.Duration
	for i, j := 0, 0; i < len(a) && j < len(b); {
		total += max(0, min(a[i].End, b[j].End)-max(a[i].Start, b[j].Start))
		if a[i].End < b[j].End {
			i++
		} else {
			j++
		}
	}
	return total
}

func exclusive(window Span, speaker string, r reference) []Span {
	var others []Span
	for id, spans := range r.speech {
		if id == speaker {
			continue
		}
		for _, s := range spans {
			if s.End > window.Start && s.Start < window.End {
				others = append(others, Span{max(window.Start, s.Start), min(window.End, s.End)})
			}
		}
	}
	others = merge(others)
	var out []Span
	cursor := window.Start
	for _, s := range others {
		if s.Start > cursor {
			out = append(out, Span{cursor, s.Start})
		}
		cursor = max(cursor, s.End)
	}
	if cursor < window.End {
		out = append(out, Span{cursor, window.End})
	}
	return out
}
