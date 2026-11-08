package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type memoryStore struct {
	key                           []byte
	loads, checks, saves, deletes int
	err                           error
}

func (s *memoryStore) Load(context.Context) ([]byte, error) {
	s.loads++
	if s.err != nil {
		return nil, s.err
	}
	if s.key == nil {
		return nil, ErrNotFound
	}
	return append([]byte(nil), s.key...), nil
}
func (s *memoryStore) Has(context.Context) (bool, error) { s.checks++; return s.key != nil, s.err }
func (s *memoryStore) Save(_ context.Context, key []byte) error {
	s.saves++
	if s.err != nil {
		return s.err
	}
	s.key = append([]byte(nil), key...)
	return nil
}
func (s *memoryStore) Delete(context.Context) error {
	s.deletes++
	if s.err != nil {
		return s.err
	}
	s.key = nil
	return nil
}

func TestEnvironmentPrecedenceAndStatus(t *testing.T) {
	stored := &memoryStore{key: []byte("stored-test-token")}
	p := Provider{Store: stored, getenv: func(name string) string {
		if name != EnvName {
			t.Error("wrong environment name")
		}
		return "environment-test-token"
	}}
	key, source, err := p.Resolve(context.Background())
	if err != nil || key != "environment-test-token" || source != Environment || stored.loads != 0 {
		t.Fatalf("environment precedence failed: source=%q err=%v", source, err)
	}
	if source, ok, err := p.Status(context.Background()); err != nil || !ok || source != Environment || stored.checks != 0 {
		t.Fatal("status accessed Keychain despite an environment key")
	}
	p.getenv = func(string) string { return "" }
	if source, ok, err := p.Status(context.Background()); err != nil || !ok || source != KeychainSource || stored.loads != 0 || stored.checks != 1 {
		t.Fatal("status retrieved secret data or lost presence")
	}
	key, source, err = p.Resolve(context.Background())
	if err != nil || key != "stored-test-token" || source != KeychainSource || stored.loads != 1 {
		t.Fatalf("stored fallback failed: source=%q err=%v", source, err)
	}
	stored.key = nil
	if _, _, err := p.Resolve(context.Background()); !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), EnvName) {
		t.Fatal("missing credential did not give actionable guidance")
	}
	if _, ok, err := p.Status(context.Background()); err != nil || ok {
		t.Fatal("missing credential looked available")
	}
}

func TestInvalidKeysNeverFallBackOrPersist(t *testing.T) {
	for _, key := range []string{" ", "secret\nvalue", "secret\x00value", "secret value", "secret\u00e9", strings.Repeat("x", maxKeyBytes+1)} {
		store := &memoryStore{key: []byte("fallback-test-token")}
		p := Provider{Store: store, getenv: func(string) string { return key }}
		if _, _, err := p.Resolve(context.Background()); !errors.Is(err, ErrInvalid) || len(key) > 1 && strings.Contains(err.Error(), key) || store.loads != 0 {
			t.Fatal("invalid environment key leaked or silently fell back")
		}
		if _, _, err := p.Status(context.Background()); !errors.Is(err, ErrInvalid) || store.checks != 0 {
			t.Fatal("invalid environment status fell back")
		}
		if err := p.Save(context.Background(), []byte(key)); !errors.Is(err, ErrInvalid) || store.saves != 0 {
			t.Fatal("invalid login key persisted")
		}
		p.getenv = func(string) string { return "" }
		store.key = []byte(key)
		if _, _, err := p.Resolve(context.Background()); !errors.Is(err, ErrInvalid) || len(key) > 1 && strings.Contains(err.Error(), key) {
			t.Fatal("invalid stored key leaked or succeeded")
		}
	}
	if err := (Provider{}).Save(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty login accepted")
	}
}

func TestLoginLogoutAndCanceledOperations(t *testing.T) {
	store := &memoryStore{}
	p := Provider{Store: store, getenv: func(string) string { return "" }}
	key := []byte("fake-login-token")
	if err := p.Save(context.Background(), key); err != nil || store.saves != 1 || string(store.key) != string(key) {
		t.Fatal("login did not store token")
	}
	if err := p.Remove(context.Background()); err != nil || store.key != nil || store.deletes != 1 {
		t.Fatal("logout did not remove dedicated item")
	}
	store.err = ErrNotFound
	if err := p.Remove(context.Background()); err != nil {
		t.Fatal("already-missing logout was not idempotent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store.loads, store.checks, store.saves, store.deletes = 0, 0, 0, 0
	_, _, loadErr := p.Resolve(ctx)
	_, _, statusErr := p.Status(ctx)
	for _, err := range []error{loadErr, statusErr, p.Save(ctx, key), p.Remove(ctx)} {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation cause lost")
		}
	}
	if store.loads+store.checks+store.saves+store.deletes != 0 {
		t.Fatal("canceled operation accessed store")
	}
	// The native adapter must return before creating a context or accessing an item.
	if _, err := (Keychain{}).Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("native adapter accessed Keychain for a canceled operation")
	}
}

func TestStoreFailuresRemainInspectable(t *testing.T) {
	want := errors.New("store unavailable")
	p := Provider{Store: &memoryStore{err: want}, getenv: func(string) string { return "" }}
	_, _, resolve := p.Resolve(context.Background())
	_, _, status := p.Status(context.Background())
	for _, err := range []error{resolve, status, p.Save(context.Background(), []byte("test-token")), p.Remove(context.Background())} {
		if !errors.Is(err, want) {
			t.Fatal("store cause lost")
		}
	}
}
