//go:build !darwin && !linux

package subprocess

import "os/exec"

func ownGroup(_ *exec.Cmd) func() error { return func() error { return nil } }
