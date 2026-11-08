//go:build darwin

package credentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const keyPrompt = "Paste your Meta API key (input is hidden): "

// ReadSecret reads a bounded ASCII token from the controlling terminal, not stdin
// or arguments. The caller owns the returned bytes and should clear them after use.
func ReadSecret(ctx context.Context, prompt io.Writer) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrTerminal
	}
	defer f.Close()
	return readTerminal(ctx, f, prompt)
}

func terminalState(f *os.File, request uintptr, state *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(unsafe.Pointer(state)))
	if errno != 0 {
		return errno
	}
	return nil
}

func readTerminal(ctx context.Context, f *os.File, prompt io.Writer) (key []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var saved syscall.Termios
	if terminalState(f, syscall.TIOCGETA, &saved) != nil {
		return nil, ErrTerminal
	}
	fd := int(f.Fd())
	raw := saved
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON
	raw.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := terminalState(f, syscall.TIOCSETAF, &raw); err != nil {
		return nil, fmt.Errorf("disable terminal echo: %w", err)
	}
	defer func() {
		// Discard queued input before restoring echo so interrupted pasted keys
		// cannot subsequently be echoed or interpreted by the user's shell.
		restore := terminalState(f, syscall.TIOCSETAF, &saved)
		_, newline := fmt.Fprintln(prompt)
		err = errors.Join(err, restore, newline)
		if err != nil {
			clear(key)
			key = nil
		}
	}()
	if _, err := fmt.Fprint(prompt, keyPrompt); err != nil {
		return nil, err
	}
	key = make([]byte, 0, maxKeyBytes)
	var buffer [128]byte
	defer clear(buffer[:])
	for {
		if err := ctx.Err(); err != nil {
			return key, err
		}
		ready, err := waitReadable(fd, 100*time.Millisecond)
		if err != nil {
			return key, err
		}
		if !ready {
			continue
		}
		n, readErr := syscall.Read(fd, buffer[:])
		if readErr == syscall.EAGAIN || readErr == syscall.EINTR {
			continue
		}
		if readErr != nil {
			return key, readErr
		}
		if n == 0 {
			return key, io.EOF
		}
		for _, c := range buffer[:n] {
			switch c {
			case '\r', '\n':
				return key, nil
			case 3:
				return key, context.Canceled
			case 4:
				return key, io.EOF
			case 8, 127:
				if len(key) > 0 {
					key[len(key)-1] = 0
					key = key[:len(key)-1]
				}
			case 21:
				clear(key)
				key = key[:0]
			default:
				if c < 33 || c > 126 || len(key) == maxKeyBytes {
					return key, ErrInvalid
				}
				key = append(key, c)
			}
		}
	}
}

// waitReadable reports whether fd has input within timeout. It uses select
// because macOS kqueue and poll cannot wait on /dev/tty, so Go deadlines fail there.
func waitReadable(fd int, timeout time.Duration) (bool, error) {
	var set syscall.FdSet
	if fd < 0 || fd >= len(set.Bits)*32 {
		return false, ErrTerminal
	}
	word, bit := fd/32, int32(1)<<(fd%32)
	set.Bits[word] |= bit
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.Select(fd+1, &set, nil, nil, &tv); err != nil {
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return false, err
	}
	return set.Bits[word]&bit != 0, nil
}
