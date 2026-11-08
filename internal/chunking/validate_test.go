package chunking

import (
	"context"
	"errors"
	"testing"

	"github.com/teapot2/turnbeam/internal/media"
)

func TestValidateCompletePlan(t *testing.T) {
	samples := int64(2*media.SampleRate + 1)
	valid := []Chunk{{ID: "first", EndSample: media.SampleRate, Boundary: Forced}, {ID: "second", StartSample: media.SampleRate - OverlapSamples, EndSample: samples, OverlapSamples: OverlapSamples, Boundary: End}}
	if err := Validate(context.Background(), valid, samples); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]Chunk){
		"duplicate ID":       func(p []Chunk) { p[1].ID = p[0].ID },
		"invalid UTF8":       func(p []Chunk) { p[0].ID = string([]byte{0xff}) },
		"blank ID":           func(p []Chunk) { p[0].ID = " " },
		"gap":                func(p []Chunk) { p[1].StartSample += 24 },
		"wrong overlap":      func(p []Chunk) { p[1].OverlapSamples++ },
		"unknown boundary":   func(p []Chunk) { p[0].Boundary = "other" },
		"missing ending":     func(p []Chunk) { p[1].Boundary = Forced },
		"short coverage":     func(p []Chunk) { p[1].EndSample-- },
		"long coverage":      func(p []Chunk) { p[1].EndSample++ },
		"missing origin":     func(p []Chunk) { p[0].StartSample = 24 },
		"unaligned internal": func(p []Chunk) { p[0].EndSample++; p[1].StartSample++ },
	} {
		t.Run(name, func(t *testing.T) {
			p := append([]Chunk(nil), valid...)
			change(p)
			if err := Validate(context.Background(), p, samples); !errors.Is(err, ErrPlan) {
				t.Fatal("malformed plan accepted", err)
			}
		})
	}
	for _, p := range [][]Chunk{nil, make([]Chunk, maxPlanChunks+1), {{ID: "too_long", EndSample: MaxChunkSamples + 1, Boundary: End}}} {
		if err := Validate(context.Background(), p, samples); !errors.Is(err, ErrPlan) {
			t.Fatal("invalid plan size accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Validate(ctx, valid, samples); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost")
	}
}
