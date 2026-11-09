package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points HOME at an empty directory, so the default model never comes
// from the developer's saved settings or installed models.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "turnbeam-cli-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Unsetenv(modelEnv)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
