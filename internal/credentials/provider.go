// Package credentials resolves Meta keys without persisting them outside Keychain.
package credentials

import (
	"context"
	"errors"
	"os"
)

const EnvName = "META_API_KEY"
const maxKeyBytes = 8192

var (
	ErrMissing     = errors.New("no Meta API key; run `turnbeam key set` or set META_API_KEY")
	ErrInvalid     = errors.New("invalid Meta API key; expected a nonempty ASCII token without whitespace")
	ErrNotFound    = errors.New("CLI credential is not stored in Keychain")
	ErrUnavailable = errors.New("Keychain requires a macOS build with cgo; use META_API_KEY instead")
	ErrTerminal    = errors.New("`turnbeam key set` needs an interactive terminal")
)

// Source identifies availability without exposing the credential itself.
type Source string

const (
	Environment    Source = "environment"
	KeychainSource Source = "Keychain"
)

// Store is the provider's boundary to one dedicated CLI credential, not a
// general keychain interface. Has checks presence without retrieving secret data.
type Store interface {
	Load(context.Context) ([]byte, error)
	Has(context.Context) (bool, error)
	Save(context.Context, []byte) error
	Delete(context.Context) error
}

// Provider uses the environment first and its dedicated Keychain item otherwise.
// Zero values select the current process environment and native Keychain store.
type Provider struct {
	Store  Store
	getenv func(string) string
}

func (p Provider) store() Store {
	if p.Store != nil {
		return p.Store
	}
	return Keychain{}
}

func (p Provider) environment() string {
	if p.getenv != nil {
		return p.getenv(EnvName)
	}
	return os.Getenv(EnvName)
}

// Resolve returns a key only to the inference caller. Invalid nonempty environment
// values fail rather than falling back to a potentially unintended stored key.
func (p Provider) Resolve(ctx context.Context) (string, Source, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if key := p.environment(); key != "" {
		if !valid([]byte(key)) {
			return "", "", ErrInvalid
		}
		return key, Environment, nil
	}
	key, err := p.store().Load(ctx)
	defer clear(key)
	if errors.Is(err, ErrNotFound) {
		return "", "", ErrMissing
	}
	if err != nil {
		return "", "", err
	}
	if !valid(key) {
		return "", "", ErrInvalid
	}
	return string(key), KeychainSource, nil
}

// Status reports only presence/source. It never loads a stored secret or contacts
// Meta; presence does not promise that the remote service will accept the key.
func (p Provider) Status(ctx context.Context) (Source, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if key := p.environment(); key != "" {
		if !valid([]byte(key)) {
			return "", false, ErrInvalid
		}
		return Environment, true, nil
	}
	ok, err := p.store().Has(ctx)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	return KeychainSource, true, nil
}

// Save stores a validated key. The caller owns and should clear key afterwards.
func (p Provider) Save(ctx context.Context, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !valid(key) {
		return ErrInvalid
	}
	return p.store().Save(ctx, key)
}

// Remove deletes only the dedicated CLI item; environment keys remain untouched.
func (p Provider) Remove(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := p.store().Delete(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func valid(key []byte) bool {
	if len(key) == 0 || len(key) > maxKeyBytes {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
