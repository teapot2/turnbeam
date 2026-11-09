package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrArgument identifies a tool call the assistant made with invalid
// arguments. Its message is written for the assistant and is returned as is.
var ErrArgument = errors.New("invalid tool arguments")

// ErrOutsideRoots identifies a path outside the directories the server may use.
var ErrOutsideRoots = errors.New("path is outside the directories this server may access")

func argumentError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrArgument, fmt.Sprintf(format, args...))
}

// roots are the resolved directories tools may read and write.
type roots []string

// newRoots resolves the configured directories, or the home directory when
// none are given. Each must exist.
func newRoots(dirs []string) (roots, error) {
	if len(dirs) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home directory for the default --root: %w", err)
		}
		dirs = []string{home}
	}
	var resolved roots
	for _, dir := range dirs {
		expanded, err := expandHome(dir)
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return nil, fmt.Errorf("resolve --root: %w", err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("--root must be an existing directory: %w", err)
		}
		info, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("--root must be an existing directory: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("--root must be an existing directory")
		}
		resolved = append(resolved, real)
	}
	return resolved, nil
}

// expandHome replaces a leading ~ with the home directory. Relative paths are
// refused: the server's working directory is whatever the host chose.
func expandHome(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", argumentError("path must not be empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		return filepath.Join(home, path[1:]), nil
	}
	if !filepath.IsAbs(path) {
		return "", argumentError("path must be absolute or start with ~: %s", path)
	}
	return path, nil
}

// notExistError says a path is missing. It matches both ErrArgument, so the
// assistant sees it as written, and os.ErrNotExist, so callers can add advice.
type notExistError struct{ path string }

func (e *notExistError) Error() string {
	return "invalid tool arguments: path does not exist: " + e.path
}

func (e *notExistError) Is(target error) bool {
	return target == ErrArgument || target == os.ErrNotExist
}

// realize resolves symbolic links in the longest existing prefix of clean and
// reports whether the whole path exists, so a missing path can still be
// checked against the roots and reported as missing rather than forbidden.
func realize(clean string) (string, bool, error) {
	real, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return real, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean, false, nil
	}
	realParent, _, err := realize(parent)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(realParent, filepath.Base(clean)), false, nil
}

// resolve returns the real, absolute form of path if it lies within a root.
// With mustExist the path itself must exist; otherwise its parent directory
// must, so a new output can be checked before it is written.
func (r roots) resolve(path string, mustExist bool) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(expanded)
	real, exists, err := realize(clean)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if !r.contains(real) {
		return "", fmt.Errorf("%w: %w: %s", ErrArgument, ErrOutsideRoots, path)
	}
	if mustExist && !exists {
		return "", &notExistError{path}
	}
	if !exists {
		if _, parentExists, err := realize(filepath.Dir(clean)); err != nil || !parentExists {
			return "", argumentError("directory does not exist: %s", filepath.Dir(path))
		}
	}
	return real, nil
}

func (r roots) contains(path string) bool {
	for _, root := range r {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// CheckRoots reports whether every directory can serve as a root, so a
// command can refuse bad flags before it starts serving.
func CheckRoots(dirs []string) error {
	_, err := newRoots(dirs)
	return err
}
