package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := FileSHA256(context.Background(), path)
	if err != nil || hash != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("wrong fingerprint: %q %v", hash, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FileSHA256(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if _, err := FileSHA256(context.Background(), dir); !errors.Is(err, ErrInvalidMedia) {
		t.Fatal("directory accepted")
	}
	if _, err := FileSHA256(context.Background(), path+"missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing file error lost")
	}
}
