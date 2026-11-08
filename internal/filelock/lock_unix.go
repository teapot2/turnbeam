//go:build darwin || linux

package filelock

import (
	"errors"
	"os"
	"syscall"
)

var ErrBusy = errors.New("directory is locked")

// Try holds an exclusive nonblocking lock until the returned file is closed.
func Try(root *os.Root, name string) (*os.File, error) {
	stat, err := root.Lstat(name)
	if err == nil && (!stat.Mode().IsRegular() || stat.Mode().Perm() != 0600) {
		return nil, errors.New("lock must be a private regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return lock(f)
}

// TryDirectory serializes short transcript edits/exports without a lock file.
// Lock the stable parent, since replacing a transcript changes its inode.
func TryDirectory(root *os.Root) (*os.File, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	return lock(f)
}

func lock(f *os.File) (*os.File, error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return f, nil
}
