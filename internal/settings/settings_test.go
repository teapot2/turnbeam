package settings

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "turnbeam")
	path := filepath.Join(dir, "settings.json")
	if s, err := Load(path); err != nil || s != (Settings{}) {
		t.Fatalf("missing file: %+v, %v", s, err)
	}
	if err := Save(path, Settings{DefaultModel: "nemotron-en"}); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(path); err != nil || s.DefaultModel != "nemotron-en" {
		t.Fatalf("round trip: %+v, %v", s, err)
	}
	for name, perm := range map[string]os.FileMode{dir: 0700, path: 0600} {
		if info, err := os.Stat(name); err != nil || info.Mode().Perm() != perm {
			t.Fatalf("%s is not private: %v", name, err)
		}
	}
	if err := Save(path, Settings{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{\"schemaVersion\":1}\n" {
		t.Fatalf("cleared settings: %q, %v", data, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"malformed": "{", "unknown field": `{"schemaVersion":1,"theme":"dark"}`, "newer schema": `{"schemaVersion":2}`,
		"missing schema": `{"defaultModel":"parakeet"}`, "wrong type": `{"schemaVersion":1,"defaultModel":3}`,
	} {
		path := filepath.Join(dir, name+".json")
		os.WriteFile(path, []byte(data), 0600)
		if _, err := Load(path); !errors.Is(err, ErrSettings) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	folder := filepath.Join(dir, "folder.json")
	os.Mkdir(folder, 0700)
	link := filepath.Join(dir, "link.json")
	os.Symlink(filepath.Join(dir, "missing"), link)
	large := filepath.Join(dir, "large.json")
	os.WriteFile(large, make([]byte, maxBytes+1), 0600)
	for _, path := range []string{folder, link, large} {
		if _, err := Load(path); !errors.Is(err, ErrSettings) {
			t.Errorf("%s accepted: %v", filepath.Base(path), err)
		}
	}
}

func TestSaveFailsWithoutReplacing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := Save(path, Settings{DefaultModel: "parakeet"}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	if err := Save(path, Settings{DefaultModel: "meta"}); err == nil {
		t.Fatal("save into a read-only directory succeeded")
	}
	if s, err := Load(path); err != nil || s.DefaultModel != "parakeet" {
		t.Fatalf("failed save changed settings: %+v, %v", s, err)
	}
}
