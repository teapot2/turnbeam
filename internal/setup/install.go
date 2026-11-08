package setup

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Manager owns a dedicated installation directory. Zero values use the pinned
// artifacts, standard HTTPS client and the macOS Application Support location.
type Manager struct {
	Dir    string
	Client *http.Client
	// OnProgress, if set, receives Install activity on the calling goroutine.
	OnProgress func(Progress)
	// retryDelay and stallTimeout override download timing in tests.
	retryDelay   func(attempt int) time.Duration
	stallTimeout time.Duration
	pins         *catalog
	system       func() (string, string)
	command      func(context.Context, string, ...string) *exec.Cmd
}

// Installation identifies a fully verified, committed runtime/model pair.
type Installation struct {
	Directory string
	Runtime   string
	Model     string
}

// Installation errors; each message states its cause.
var (
	ErrUnsupportedPlatform = errors.New("requires macOS on Apple Silicon (darwin/arm64)")
	ErrBusy                = errors.New("setup is already running for this installation directory")
	ErrNotReady            = errors.New("local runtime/model unavailable; run doctor or setup")
	ErrCleanup             = errors.New("incomplete installation cleanup failed; inspect the installation directory")
	ErrUnsafeDirectory     = errors.New("installation directory must have mode 0700; run chmod 700 on it or choose another directory")
)

// DefaultDir keeps durable model/runtime files separate from recording caches.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate Application Support directory: %w", err)
	}
	return filepath.Join(base, "turnbeam"), nil
}

func (m Manager) directory() (string, error) {
	if m.Dir == "" {
		return DefaultDir()
	}
	return filepath.Abs(m.Dir)
}

func (m Manager) catalog() catalog {
	if m.pins != nil {
		return *m.pins
	}
	return pinnedCatalog()
}

func (m Manager) platform() error {
	goos, arch := runtime.GOOS, runtime.GOARCH
	if m.system != nil {
		goos, arch = m.system()
	}
	if goos != "darwin" || arch != "arm64" {
		return ErrUnsupportedPlatform
	}
	return nil
}

type current struct {
	SchemaVersion int    `json:"schemaVersion"`
	Generation    string `json:"generation"`
}

func openCurrent(root *os.Root) (*os.Root, error) {
	stat, err := root.Lstat("current.json")
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 4096 || stat.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("%w: invalid installation pointer", ErrIntegrity)
	}
	data, err := root.ReadFile("current.json")
	if err != nil {
		return nil, err
	}
	var pointer current
	if err := json.Unmarshal(data, &pointer); err != nil {
		return nil, fmt.Errorf("%w: invalid installation pointer: %w", ErrIntegrity, err)
	}
	if pointer.SchemaVersion != 1 || !strings.HasPrefix(pointer.Generation, "install-") || strings.ContainsAny(pointer.Generation, `/\`) || !filepath.IsLocal(pointer.Generation) {
		return nil, fmt.Errorf("%w: incompatible installation pointer", ErrIntegrity)
	}
	stat, err = root.Lstat(pointer.Generation)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("%w: installation must be a private directory", ErrIntegrity)
	}
	return root.OpenRoot(pointer.Generation)
}

func paths(dir string) Installation {
	return Installation{Directory: dir, Runtime: filepath.Join(dir, "runtime", "bin", "nemo-speech"), Model: filepath.Join(dir, "model.gguf")}
}

func verify(ctx context.Context, root *os.Root, pins catalog) error {
	for _, a := range pins.assets {
		if err := verifyAsset(ctx, root, a); err != nil {
			return err
		}
	}
	return verifyRuntime(ctx, root, pins.prefix)
}

func privateDirectory(root *os.Root) error {
	stat, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect installation directory: %w", err)
	}
	if stat.Mode().Perm() != 0700 {
		return ErrUnsafeDirectory
	}
	return nil
}

// Verify checks installed bytes against the application pins without network or
// native execution. It never trusts stored file hashes supplied by the install.
func (m Manager) Verify(ctx context.Context) (Installation, error) {
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	dir, err := m.directory()
	if err != nil {
		return Installation{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Installation{}, fmt.Errorf("installation missing; run setup: %w", err)
	}
	defer root.Close()
	if err := privateDirectory(root); err != nil {
		return Installation{}, err
	}
	installed, err := openCurrent(root)
	if err != nil {
		return Installation{}, fmt.Errorf("installation incomplete; rerun setup: %w", err)
	}
	defer installed.Close()
	if err := verify(ctx, installed, m.catalog()); err != nil {
		return Installation{}, fmt.Errorf("installation damaged; rerun setup: %w", err)
	}
	return paths(installed.Name()), nil
}

// Ready verifies the managed installation and local Metal/model compatibility
// without downloading anything. Inference callers must resolve it before use.
func (m Manager) Ready(ctx context.Context) (_ Installation, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrNotReady, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	if err := m.platform(); err != nil {
		return Installation{}, err
	}
	installed, err := m.Verify(ctx)
	if err != nil {
		return Installation{}, err
	}
	if _, err := m.nativeChecks(ctx, installed); err != nil {
		return Installation{}, err
	}
	return installed, nil
}

// Install explicitly downloads and verifies pinned assets. A verified generation
// is atomically activated only after local compatibility checks pass. Prior
// generations remain intact, including on cancellation or failed repair.
func (m Manager) Install(ctx context.Context) (_ Installation, err error) {
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	if err := m.platform(); err != nil {
		return Installation{}, err
	}
	dir, err := m.directory()
	if err != nil {
		return Installation{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Installation{}, fmt.Errorf("create installation directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Installation{}, err
	}
	defer root.Close()
	if err := privateDirectory(root); err != nil {
		return Installation{}, err
	}
	lock, err := lockInstall(root)
	if err != nil {
		return Installation{}, err
	}
	defer lock.Close()
	removeStale(root)
	pins := m.catalog()
	previous, _ := openCurrent(root)
	if previous != nil {
		defer previous.Close()
		m.report(Progress{Step: StepCheck})
		if err := verify(ctx, previous, pins); err == nil {
			m.report(Progress{Step: StepCompatibility})
			installed := paths(previous.Name())
			if _, err := m.nativeChecks(ctx, installed); err != nil {
				return Installation{}, err
			}
			return installed, nil
		}
		if err := ctx.Err(); err != nil {
			return Installation{}, err
		}
	}
	stageDir, err := os.MkdirTemp(dir, ".stage-")
	if err != nil {
		return Installation{}, fmt.Errorf("create installation staging directory: %w", err)
	}
	stageName := filepath.Base(stageDir)
	committed := false
	defer func() {
		if !committed {
			if cleanup := root.RemoveAll(stageName); cleanup != nil {
				err = errors.Join(err, ErrCleanup, cleanup)
			}
		}
	}()
	stage, err := root.OpenRoot(stageName)
	if err != nil {
		return Installation{}, err
	}
	defer stage.Close()
	downloads, err := privateSubdirectory(root, downloadsDir)
	if err != nil {
		return Installation{}, err
	}
	defer downloads.Close()
	// Fetch small files first so a failure on the large model costs the least.
	assets := slices.SortedFunc(slices.Values(pins.assets), func(x, y artifact) int { return cmp.Compare(x.size, y.size) })
	for _, a := range assets {
		source := previous
		if previous == nil || verifyAsset(ctx, previous, a) != nil {
			if err := m.download(ctx, downloads, a); err != nil {
				return Installation{}, err
			}
			source = downloads
		}
		f, err := source.Open(a.name)
		if err != nil {
			return Installation{}, err
		}
		err = saveAsset(ctx, stage, a, f)
		f.Close()
		if err != nil {
			return Installation{}, fmt.Errorf("stage %s: %w", a.name, err)
		}
	}
	if err := stage.Mkdir("runtime", 0700); err != nil {
		return Installation{}, err
	}
	archive, err := stage.Open("runtime.tar.gz")
	if err != nil {
		return Installation{}, err
	}
	runtimeRoot, err := stage.OpenRoot("runtime")
	if err != nil {
		archive.Close()
		return Installation{}, err
	}
	_, extractErr := readArchive(ctx, archive, pins.prefix, runtimeRoot)
	err = errors.Join(extractErr, archive.Close(), runtimeRoot.Close())
	if err != nil {
		return Installation{}, err
	}
	m.report(Progress{Step: StepVerify})
	if err := verify(ctx, stage, pins); err != nil {
		return Installation{}, err
	}
	m.report(Progress{Step: StepCompatibility})
	if _, err := m.nativeChecks(ctx, paths(stageDir)); err != nil {
		return Installation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	generation := "install-" + strings.TrimPrefix(stageName, ".stage-")
	if err := root.Rename(stageName, generation); err != nil {
		return Installation{}, fmt.Errorf("publish verified installation: %w", err)
	}
	stageName = generation
	data, err := json.Marshal(current{SchemaVersion: 1, Generation: generation})
	if err != nil {
		return Installation{}, err
	}
	if err := publishPointer(root, data); err != nil {
		return Installation{}, err
	}
	committed = true
	keep := []string{generation}
	if previous != nil {
		keep = append(keep, filepath.Base(previous.Name()))
	}
	// Best effort: the new installation is already active.
	root.RemoveAll(downloadsDir)
	pruneGenerations(root, keep)
	return paths(filepath.Join(dir, generation)), nil
}

// downloadsDir holds verified and partial downloads between setup attempts.
const downloadsDir = "downloads"

func privateSubdirectory(root *os.Root, name string) (*os.Root, error) {
	if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	stat, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("%w: %s must be a private directory", ErrIntegrity, name)
	}
	return root.OpenRoot(name)
}

// removeStale deletes staging leftovers of setups that were killed before
// cleanup. Callers hold the installation lock, so none of them is in use.
func removeStale(root *os.Root) {
	entries, _ := fs.ReadDir(root.FS(), ".")
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, ".stage-") || strings.HasPrefix(name, ".current-") {
			root.RemoveAll(name)
		}
	}
}

// pruneGenerations keeps the active installation and the one before it, which
// a transcription started earlier may still be running from.
func pruneGenerations(root *os.Root, keep []string) {
	entries, _ := fs.ReadDir(root.FS(), ".")
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, "install-") && !slices.Contains(keep, name) {
			root.RemoveAll(name)
		}
	}
}

func publishPointer(root *os.Root, data []byte) (err error) {
	file, err := os.CreateTemp(root.Name(), ".current-")
	if err != nil {
		return err
	}
	name := filepath.Base(file.Name())
	defer root.Remove(name)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write installation pointer: %w", err)
	}
	if err := root.Rename(name, "current.json"); err != nil {
		return fmt.Errorf("activate verified installation: %w", err)
	}
	return nil
}
