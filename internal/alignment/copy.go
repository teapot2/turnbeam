package alignment

import (
	"context"

	"github.com/teapot2/turnbeam/internal/transcript"
)

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyRawTurns(ctx context.Context, turns []transcript.RawTurn) ([]transcript.RawTurn, error) {
	out := make([]transcript.RawTurn, len(turns))
	for i, turn := range turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = turn
		out[i].Speaker = copyString(turn.Speaker)
	}
	return out, nil
}
