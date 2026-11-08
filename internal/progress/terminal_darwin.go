//go:build darwin

package progress

import "syscall"

const terminalRequest = syscall.TIOCGETA
