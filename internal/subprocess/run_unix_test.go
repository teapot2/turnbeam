//go:build darwin || linux

package subprocess

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessTree(t *testing.T) {
	role := os.Getenv("TURNBEAM_PROCESS_ROLE")
	if role == "" {
		return
	}
	pipe := os.NewFile(3, "lifetime")
	if role == "grandchild" {
		if os.Getenv("META_API_KEY") != "" {
			os.Exit(6)
		}
		pipe.Write([]byte("R"))
		time.Sleep(time.Hour)
		os.Exit(8)
	}
	self, _ := os.Executable()
	child := exec.Command(self, "-test.run=^TestProcessTree$")
	child.Env = append(os.Environ(), "TURNBEAM_PROCESS_ROLE=grandchild")
	child.ExtraFiles = []*os.File{pipe}
	if err := child.Start(); err != nil {
		os.Exit(5)
	}
	pipe.Close()
	child.Wait()
	os.Exit(0)
}

func TestCancellationTerminatesDescendants(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestProcessTree$")
	cmd.Env = append(os.Environ(), "TURNBEAM_PROCESS_ROLE=parent", "META_API_KEY=fake-private-key", "GORACE=atexit_sleep_ms=0")
	cmd.ExtraFiles = []*os.File{w}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd) }()
	r.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready [1]byte
	if _, err := io.ReadFull(r, ready[:]); err != nil || ready[0] != 'R' {
		cancel()
		<-done
		t.Fatal("grandchild did not start with isolated environment", err)
	}
	cancel()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &exit) {
			t.Fatal("child cancellation/status was lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process group did not terminate")
	}
	w.Close()
	r.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.Read(ready[:]); !errors.Is(err, io.EOF) {
		t.Fatal("grandchild retained its lifetime pipe after cancellation", err)
	}
}

func TestStartFailureRetainsCause(t *testing.T) {
	ctx := context.Background()
	err := Run(ctx, exec.CommandContext(ctx, "/no-such-turnbeam-test-tool"))
	if !errors.Is(err, ErrStart) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launch cause lost", err)
	}
}
