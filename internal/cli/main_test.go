package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points HOME and XDG_CONFIG_HOME (where Linux keeps settings) at an
// empty directory, so the default model never comes from the developer's or
// CI runner's saved settings or installed models.
// isolateHome gives one test its own empty home and settings directory.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "turnbeam-cli-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Unsetenv(modelEnv)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
