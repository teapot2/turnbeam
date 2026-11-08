//go:build !darwin

package credentials

import (
	"context"
	"io"
)

func ReadSecret(ctx context.Context, _ io.Writer) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrUnavailable
}
