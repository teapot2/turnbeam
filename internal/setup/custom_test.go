package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCustomModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Mine.GGUF")
	if err := os.WriteFile(path, []byte("gguf"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := CustomModel(context.Background(), path)
	if err != nil || !m.Custom() || m.Path() != path || m.Name != path || m.File != "Mine.GGUF" || m.Latency != 0 || m.Size() != 0 ||
		m.Revision != "sha256:1cb1b7e0f8b96cee3445e317b8064d8805bf35c7dc7de82cddcb9f78d4c95e0e" {
		t.Fatalf("custom model: %+v, %v", m, err)
	}
	if _, ok := FindLocalModel(path); ok || !IsModelFile(path) || IsModelFile("parakeet") {
		t.Fatal("a file path was treated as a catalog name")
	}
	if pinned, _ := FindLocalModel("parakeet"); pinned.Custom() || pinned.Path() != "" {
		t.Fatal("pinned model looked custom")
	}
	empty := filepath.Join(dir, "empty.gguf")
	os.WriteFile(empty, nil, 0600)
	folder := filepath.Join(dir, "folder.gguf")
	os.Mkdir(folder, 0700)
	notGGUF := filepath.Join(dir, "model.bin")
	os.WriteFile(notGGUF, []byte("x"), 0600)
	for _, bad := range []string{empty, folder, notGGUF, filepath.Join(dir, "missing.gguf")} {
		if _, err := CustomModel(context.Background(), bad); !errors.Is(err, ErrCustomModel) {
			t.Fatalf("%s accepted: %v", bad, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CustomModel(ctx, path); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCustomModel) {
		t.Fatalf("canceled hash: %v", err)
	}
}
