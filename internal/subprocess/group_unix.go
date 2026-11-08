//go:build darwin || linux

package subprocess

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func ownGroup(cmd *exec.Cmd) func() error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid, cmd.SysProcAttr.Pgid = true, 0
	kill := func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	return func() error {
		err := kill()
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
}
