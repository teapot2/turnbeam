//go:build darwin

package credentials

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func pseudoTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	for _, request := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), request, 0); errno != 0 {
			t.Fatal(errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatal(errno)
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		t.Fatal("unterminated pseudo-terminal name")
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func waitForPrompt(t *testing.T, master *os.File) {
	t.Helper()
	if err := master.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	var buffer [128]byte
	for !strings.Contains(got.String(), keyPrompt) {
		n, err := master.Read(buffer[:])
		got.Write(buffer[:n])
		if err != nil {
			t.Fatalf("terminal prompt missing: %v", err)
		}
	}
}

type secretResult struct {
	key []byte
	err error
}

func readResult(t *testing.T, done <-chan secretResult) secretResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("terminal read did not stop")
		return secretResult{}
	}
}

func TestTerminalSecretIsNotEchoedAndStateRestores(t *testing.T) {
	master, slave := pseudoTerminal(t)
	var before syscall.Termios
	if err := terminalState(slave, syscall.TIOCGETA, &before); err != nil {
		t.Fatal(err)
	}
	done := make(chan secretResult, 1)
	go func() { key, err := readTerminal(context.Background(), slave, slave); done <- secretResult{key, err} }()
	waitForPrompt(t, master)
	var reading syscall.Termios
	if err := terminalState(slave, syscall.TIOCGETA, &reading); err != nil || reading.Lflag&(syscall.ECHO|syscall.ECHONL) != 0 {
		t.Fatal("terminal echo was not disabled")
	}
	if _, err := io.WriteString(master, "wrong\x15fake-secretx\x7f\n"); err != nil {
		t.Fatal(err)
	}
	result := readResult(t, done)
	defer clear(result.key)
	if result.err != nil || string(result.key) != "fake-secret" {
		t.Fatalf("secret input/editing failed: %v", result.err)
	}
	var after syscall.Termios
	if err := terminalState(slave, syscall.TIOCGETA, &after); err != nil || after != before {
		t.Fatal("terminal settings not restored")
	}
	if err := master.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var echo [128]byte
	n, err := master.Read(echo[:])
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(string(echo[:n]), "fake-secret") || strings.Contains(string(echo[:n]), "wrong") {
		t.Fatal("terminal echoed secret input")
	}
}

func TestTerminalCancellationRestoresAndClears(t *testing.T) {
	master, slave := pseudoTerminal(t)
	var before syscall.Termios
	if err := terminalState(slave, syscall.TIOCGETA, &before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan secretResult, 1)
	go func() { key, err := readTerminal(ctx, slave, slave); done <- secretResult{key, err} }()
	waitForPrompt(t, master)
	if _, err := io.WriteString(master, "partial-secret"); err != nil {
		t.Fatal(err)
	}
	cancel()
	result := readResult(t, done)
	if !errors.Is(result.err, context.Canceled) || result.key != nil {
		t.Fatal("cancellation returned secret data or lost its cause")
	}
	var after syscall.Termios
	if err := terminalState(slave, syscall.TIOCGETA, &after); err != nil || before != after {
		t.Fatal("canceled input left echo disabled")
	}
}

func TestTerminalRejectsPipes(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if _, err := readTerminal(context.Background(), read, io.Discard); !errors.Is(err, ErrTerminal) {
		t.Fatal("non-terminal accepted")
	}
}

// ReadSecret opens /dev/tty, which Go cannot attach deadlines to on macOS. Run it
// in a child whose controlling terminal is a pseudo-terminal to cover that path.
func TestReadSecretUsesControllingTerminal(t *testing.T) {
	if os.Getenv("TURNBEAM_TTY_CHILD") == "1" {
		key, err := ReadSecret(context.Background(), os.Stdout)
		if err != nil || string(key) != "fake-tty-secret" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	master, slave := pseudoTerminal(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReadSecretUsesControllingTerminal$")
	cmd.Env = append(os.Environ(), "TURNBEAM_TTY_CHILD=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPrompt(t, master)
	if _, err := io.WriteString(master, "fake-tty-secret\n"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Keep draining output: closing the terminal at exit waits for it to be read.
	deadline := time.Now().Add(5 * time.Second)
	var sink [256]byte
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("controlling-terminal prompt failed: %v", err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("controlling-terminal prompt did not finish")
		}
		master.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		master.Read(sink[:])
	}
}
