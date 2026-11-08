package safeio

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestReaderStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := Reader(ctx, strings.NewReader("abc"))
	if data, err := io.ReadAll(io.LimitReader(r, 1)); err != nil || string(data) != "a" {
		t.Fatal(data, err)
	}
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("read continued after cancellation", err)
	}
}

func TestSyncDir(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := SyncDir(root); err != nil {
		t.Fatal(err)
	}
}
