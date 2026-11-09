// Package settings keeps the user's Turnbeam preferences, such as the saved
// default model, in one small JSON file beside the managed installation.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/teapot2/turnbeam/internal/jsonstrict"
)

// ErrSettings identifies a settings file that cannot be read or is not a
// settings document this version understands.
var ErrSettings = errors.New("settings file is unreadable or invalid; fix or delete it")

const (
	schemaVersion = 1
	// maxBytes bounds the file; a settings document is a few dozen bytes.
	maxBytes = 64 << 10
)

// Settings holds the saved preferences. Zero values mean "not set".
type Settings struct {
	// DefaultModel is the model a run uses when neither --model nor
	// TURNBEAM_MODEL names one: a model name or an absolute .gguf path.
	DefaultModel string
}

type document struct {
	SchemaVersion int    `json:"schemaVersion"`
	DefaultModel  string `json:"defaultModel,omitempty"`
}

// Path returns the settings file in the user's Application Support folder:
// ~/Library/Application Support/turnbeam/settings.json.
func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate Application Support directory: %w", err)
	}
	return filepath.Join(base, "turnbeam", "settings.json"), nil
}

// Load reads the settings at path. A missing file is not an error: it returns
// zero Settings.
func Load(path string) (_ Settings, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrSettings, err)
		}
	}()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if !info.Mode().IsRegular() {
		return Settings{}, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Settings{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return Settings{}, err
	}
	if len(data) > maxBytes {
		return Settings{}, errors.New("file too large")
	}
	var doc document
	if err := jsonstrict.Decode(data, &doc); err != nil {
		return Settings{}, err
	}
	if doc.SchemaVersion != schemaVersion {
		return Settings{}, fmt.Errorf("unsupported schema version %d", doc.SchemaVersion)
	}
	return Settings{DefaultModel: doc.DefaultModel}, nil
}

// Save atomically replaces the settings at path with a private file, creating
// its directory if needed. Readers never see a partial file.
func Save(path string, s Settings) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("save settings: %w", err)
		}
	}()
	data, err := json.Marshal(document{SchemaVersion: schemaVersion, DefaultModel: s.DefaultModel})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	_, writeErr := f.Write(append(data, '\n'))
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
