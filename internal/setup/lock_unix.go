//go:build darwin || linux

package setup

import (
	"errors"
	"fmt"
	"os"

	"github.com/teapot2/turnbeam/internal/filelock"
)

func lockInstall(root *os.Root) (*os.File, error) {
	f, err := filelock.Try(root, "setup.lock")
	if errors.Is(err, filelock.ErrBusy) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, fmt.Errorf("lock installation: %w", err)
	}
	return f, err
}
