//go:build !darwin || !cgo

package credentials

import "context"

type Keychain struct{}

const KeychainSupported = false

func (Keychain) Load(ctx context.Context) ([]byte, error) {
	return nil, unavailable(ctx)
}
func (Keychain) Has(ctx context.Context) (bool, error)    { return false, unavailable(ctx) }
func (Keychain) Save(ctx context.Context, _ []byte) error { return unavailable(ctx) }
func (Keychain) Delete(ctx context.Context) error         { return unavailable(ctx) }

func unavailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}
