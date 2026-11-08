//go:build darwin || linux

package filelock

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestWaitDirectory(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	held, err := TryDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := WaitDirectory(context.Background(), root, 60*time.Millisecond); f != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("held lock was not reported busy after waiting", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, err := WaitDirectory(ctx, root, time.Second); f != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled wait did not stop", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		held.Close()
	}()
	f, err := WaitDirectory(context.Background(), root, 2*time.Second)
	if err != nil {
		t.Fatal("lock released during the wait was not acquired", err)
	}
	f.Close()
}
