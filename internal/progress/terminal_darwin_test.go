//go:build darwin

package progress

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestRealTerminalDetectionAndFallback(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
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
		t.Fatal("unterminated terminal name")
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	t.Setenv("TERM", "xterm")
	r := New(slave, Options{})
	if !r.tty {
		t.Fatal("TTY was not detected")
	}
	if err := r.Stage(context.Background(), Probe); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := master.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	output, err := bufio.NewReader(master).ReadString('\n')
	if err != nil || !strings.Contains(output, "Checking the recording") {
		t.Fatal("interactive status did not reach the terminal", err)
	}
	t.Setenv("TERM", "dumb")
	if New(slave, Options{}).tty {
		t.Fatal("dumb terminal enabled cursor controls")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if terminal(write) {
		t.Fatal("pipe was mistaken for a terminal")
	}
}
