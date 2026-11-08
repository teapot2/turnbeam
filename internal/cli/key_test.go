package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/credentials"
)

type fakeKeys struct {
	source                   credentials.Source
	available                bool
	err                      error
	key                      []byte
	saves, removes, statuses int
}

func (a *fakeKeys) Save(_ context.Context, key []byte) error {
	a.saves++
	a.key = append([]byte(nil), key...)
	return a.err
}
func (a *fakeKeys) Remove(context.Context) error { a.removes++; return a.err }
func (a *fakeKeys) Status(context.Context) (credentials.Source, bool, error) {
	a.statuses++
	return a.source, a.available, a.err
}

func keyCommand(t *testing.T, a *fakeKeys, read func(context.Context, io.Writer) ([]byte, error), supported bool, args ...string) (string, error) {
	t.Helper()
	code := 2
	root := newRoot("test", &code)
	for _, c := range root.Commands() {
		if c.Name() == "key" {
			root.RemoveCommand(c)
		}
	}
	addKeyCommands(root, &code, func() keyDependencies { return keyDependencies{a, read, supported} })
	var stderr bytes.Buffer
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"key"}, args...))
	err := root.ExecuteContext(context.Background())
	return stderr.String(), err
}

func TestKeyCommandsDoNotEchoOrRetainInput(t *testing.T) {
	a := &fakeKeys{}
	input := []byte("fake-secret-not-for-output")
	read := func(context.Context, io.Writer) ([]byte, error) { return input, nil }
	output, err := keyCommand(t, a, read, true, "set", "--verbose")
	if err != nil || a.saves != 1 || string(a.key) != "fake-secret-not-for-output" || !strings.Contains(output, "Saved your Meta API key in the macOS Keychain") {
		t.Fatalf("login failed: %v", err)
	}
	if strings.Contains(output, "fake-secret-not-for-output") || !bytes.Equal(input, make([]byte, len(input))) {
		t.Fatal("login echoed or retained input buffer")
	}
	for _, status := range []struct {
		source    credentials.Source
		available bool
		want      string
	}{
		{credentials.Environment, true, "META_API_KEY environment variable"}, {credentials.KeychainSource, true, "saved in the macOS Keychain"}, {"", false, "No Meta API key found. Run `turnbeam key set`"},
	} {
		a.source, a.available = status.source, status.available
		output, err := keyCommand(t, a, nil, true, "status", "--verbose")
		if err != nil || !strings.Contains(output, status.want) || strings.Contains(output, "fake-secret-not-for-output") || a.saves != 1 {
			t.Fatalf("status exposed a key or attempted login: %v", err)
		}
	}
	output, err = keyCommand(t, a, nil, true, "remove")
	if err != nil || a.removes != 1 || !strings.Contains(output, "META_API_KEY in your environment is unaffected") {
		t.Fatal("logout failed")
	}
}

func TestKeyInputAndStoreFailures(t *testing.T) {
	want := errors.New("test input canceled")
	input := []byte("sensitive-partial-input")
	a := &fakeKeys{}
	if output, err := keyCommand(t, a, func(context.Context, io.Writer) ([]byte, error) { return input, want }, true, "set"); !errors.Is(err, want) || output != "" || a.saves != 0 || !bytes.Equal(input, make([]byte, len(input))) {
		t.Fatal("failed input was persisted or retained")
	}
	if _, err := keyCommand(t, a, nil, false, "set"); !errors.Is(err, credentials.ErrUnavailable) {
		t.Fatal("unsupported login read a secret")
	}
	a.err = want
	for _, args := range [][]string{{"status"}, {"remove"}, {"set"}} {
		if _, err := keyCommand(t, a, func(context.Context, io.Writer) ([]byte, error) { return []byte("test-token"), nil }, true, args...); !errors.Is(err, want) {
			t.Fatal("store error lost")
		}
	}
}

func TestKeyDoesNotAcceptSecretArgumentsOrFlags(t *testing.T) {
	for _, args := range [][]string{{"set", "fake-token"}, {"set", "--key", "fake-token"}, {"set", "--password=fake-token"}, {"set", "--quiet=fake-token"}, {"set", "--fake-token"}, {"remove", "--all"}} {
		a := &fakeKeys{}
		if output, err := keyCommand(t, a, nil, true, args...); err == nil || a.saves+a.removes+a.statuses != 0 || strings.Contains(output, "fake-token") || strings.Contains(err.Error(), "fake-token") {
			t.Fatal("secret argument or broad deletion flag accepted")
		}
	}
	// The production root can resolve an environment key without accessing Keychain.
	t.Setenv(credentials.EnvName, "fake-environment-token")
	code, output, diagnostics := invokeStreams(context.Background(), "key", "status", "--verbose")
	if code != 0 || !strings.Contains(output, "environment") || strings.Contains(output+diagnostics, "fake-environment-token") {
		t.Fatal("production status did not keep the environment key private")
	}
}
