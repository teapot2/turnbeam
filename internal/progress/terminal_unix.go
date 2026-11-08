//go:build darwin || linux

package progress

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

func terminal(writer io.Writer) bool {
	f, ok := writer.(*os.File)
	if !ok {
		return false
	}
	var state syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), terminalRequest, uintptr(unsafe.Pointer(&state)))
	return err == 0
}
