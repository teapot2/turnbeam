// Package subprocess owns cancelable native process lifetimes and environment isolation.
package subprocess

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/teapot2/turnbeam/internal/nativeenv"
)

// ErrStart identifies an unavailable or unlaunchable local dependency.
var ErrStart = errors.New("could not start a required local tool; run doctor to check dependencies")

// ErrCleanup reports a child process group that could not be terminated.
var ErrCleanup = errors.New("local tool cleanup failed; child processes may still be running")

// Run requires a CommandContext-created command. It waits for the child and
// terminates its owned process group (macOS/Linux), including descendants, before returning.
// Output policy is the caller's responsibility; raw causes remain inspectable.
func Run(ctx context.Context, cmd *exec.Cmd) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.Env = nativeenv.Sanitize(cmd.Environ())
	cmd.WaitDelay = 2 * time.Second
	terminate := ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return errors.Join(ErrStart, ctx.Err(), err)
	}
	defer func() {
		if cleanup := terminate(); cleanup != nil {
			err = errors.Join(err, ErrCleanup, cleanup)
		}
	}()
	return errors.Join(cmd.Wait(), ctx.Err())
}
