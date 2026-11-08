package chunking

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/media"
)

// Validate checks complete, exact recording coverage under the pinned policy.
// Only forced boundaries repeat audio; internal cuts use whole milliseconds.
func Validate(ctx context.Context, plan []Chunk, samples int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if samples <= 0 || len(plan) == 0 || len(plan) > maxPlanChunks {
		return ErrPlan
	}
	var end int64
	ids := map[string]bool{}
	const ms = media.SampleRate / 1000
	for i, c := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		overlap := int64(0)
		if i > 0 && plan[i-1].Boundary == Forced {
			overlap = OverlapSamples
		}
		if !utf8.ValidString(c.ID) || strings.TrimSpace(c.ID) == "" || ids[c.ID] || c.StartSample != end-overlap || c.StartSample < 0 || c.StartSample >= c.EndSample || c.EndSample > samples || c.EndSample-c.StartSample > MaxChunkSamples || c.OverlapSamples != overlap || c.StartSample%ms != 0 || i > 0 && (c.StartSample <= plan[i-1].StartSample || c.EndSample <= end) {
			return ErrPlan
		}
		if i == len(plan)-1 {
			if c.Boundary != End {
				return ErrPlan
			}
		} else if (c.Boundary != Pause && c.Boundary != Forced) || c.EndSample%ms != 0 {
			return ErrPlan
		}
		ids[c.ID], end = true, c.EndSample
	}
	if end != samples {
		return ErrPlan
	}
	return nil
}
