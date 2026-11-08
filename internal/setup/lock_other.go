//go:build !darwin && !linux

package setup

import "os"

func lockInstall(_ *os.Root) (*os.File, error) { return nil, ErrUnsupportedPlatform }
