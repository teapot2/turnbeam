//go:build !darwin && !linux

package filelock

import (
	"errors"
	"os"
)

var ErrBusy = errors.New("directory is locked")

// Try refuses platforms without the supported advisory-lock implementation.
func Try(_ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("file locking requires macOS or Linux")
}

// TryDirectory refuses platforms without supported advisory directory locking.
func TryDirectory(_ *os.Root) (*os.File, error) {
	return nil, errors.New("file locking requires macOS or Linux")
}
