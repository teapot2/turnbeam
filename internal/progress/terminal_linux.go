//go:build linux

package progress

import "syscall"

const terminalRequest = syscall.TCGETS
