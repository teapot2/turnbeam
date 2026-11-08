package testutil

import (
	"context"
	"os"
	"testing"
	"time"
)

func WaitReady(t *testing.T, path string, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("operation ended before readiness: %v", err)
		case <-deadline.C:
			cancel()
			WaitError(t, done)
			t.Fatal("operation did not become ready")
		case <-tick.C:
		}
	}
}

func WaitError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not terminate")
		return nil
	}
}
