package alignment

import (
	"context"
	"strings"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/transcript"
)

type duplicateKey struct {
	start, end int64
	text       string
}
type occurrence struct {
	source transcript.Source
	count  int
}

// Only identical positive intervals wholly inside a declared forced overlap are
// eligible. Ambiguous many-to-one matches, partial turns and timing jitter stay.
func duplicates(ctx context.Context, plan []chunking.Chunk, turns [][]transcript.Turn) (map[transcript.Source]bool, error) {
	removed := map[transcript.Source]bool{}
	for i := 1; i < len(plan); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if plan[i-1].Boundary != chunking.Forced {
			continue
		}
		start, end := plan[i].StartMS(), plan[i-1].EndMS()
		left, err := occurrences(ctx, turns[i-1], start, end)
		if err != nil {
			return nil, err
		}
		right, err := occurrences(ctx, turns[i], start, end)
		if err != nil {
			return nil, err
		}
		for key, a := range left {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if b, ok := right[key]; ok && a.count == 1 && b.count == 1 {
				removed[b.source] = true
			}
		}
	}
	return removed, nil
}

func occurrences(ctx context.Context, turns []transcript.Turn, start, end int64) (map[duplicateKey]occurrence, error) {
	index := map[duplicateKey]occurrence{}
	for _, t := range turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t.StartMS < start || t.EndMS > end || t.StartMS == t.EndMS {
			continue
		}
		key := duplicateKey{t.StartMS, t.EndMS, strings.Join(strings.Fields(t.Text), " ")}
		item := index[key]
		item.source, item.count = t.Source, item.count+1
		index[key] = item
	}
	return index, nil
}
