package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/jsonstrict"
	"github.com/teapot2/turnbeam/internal/media"
)

// Job holds an exclusive recovery slot until Close. Files contain private speech;
// they remain in the cache until an explicit purge, never automatic cleanup.
type Job struct {
	mu                             sync.Mutex
	slot, root                     *os.Root
	lock                           *os.File
	input                          string
	m                              manifest
	closed, poisoned, mediaChecked bool
	running                        bool
	write                          func(context.Context, *os.Root, string, []byte) error
	// pendingPointer names a new generation that --resume must not select until
	// it holds a transcription result, so an earlier run's paid work stays reachable.
	pendingPointer string
}

// DefaultDir separates recoverable speech data from durable managed models.
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "turnbeam", "jobs"), nil
}

// Open fingerprints the entire original file. Only its location hash and content
// hash identify the slot/generation; no input path or credential is persisted.
func Open(ctx context.Context, input string, settings Settings, opts Options) (_ *Job, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Resume && opts.Fresh || !settings.valid() {
		return nil, ErrOptions
	}
	path, err := filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	inputHash, err := media.FileSHA256(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("fingerprint recording: %w", err)
	}
	dir := opts.Dir
	if dir == "" {
		dir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	stat, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("%w: cache root must be a private directory (0700)", ErrCorrupt)
	}
	base, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	name := digest([]byte(path))
	if err := base.Mkdir(name, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err := syncDir(base); err != nil {
		return nil, err
	}
	stat, err = base.Lstat(name)
	if err != nil {
		return nil, corrupt(err)
	}
	if !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("%w: recovery slot must be a private directory (0700)", ErrCorrupt)
	}
	slot, err := base.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	j := &Job{slot: slot, input: path, write: atomicWrite}
	defer func() {
		if err != nil {
			j.Close()
		}
	}()
	j.lock, err = filelock.Try(slot, "job.lock")
	if errors.Is(err, filelock.ErrBusy) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, err
	}
	if opts.Resume {
		data, err := readFile(ctx, slot, "current.json", 4096)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissing
		}
		if err != nil {
			return nil, fmt.Errorf("%w: recovery pointer: %w", ErrCorrupt, err)
		}
		var p pointer
		if err := jsonstrict.Decode(data, &p); err != nil {
			return nil, corrupt(err)
		}
		if p.SchemaVersion != schemaVersion || !validGeneration(p.Generation) {
			return nil, ErrCorrupt
		}
		stat, err := slot.Lstat(p.Generation)
		if err != nil {
			return nil, corrupt(err)
		}
		if !stat.IsDir() || stat.Mode().Perm() != 0700 {
			return nil, ErrCorrupt
		}
		j.root, err = slot.OpenRoot(p.Generation)
		if err != nil {
			return nil, err
		}
		data, err = readFile(ctx, j.root, "manifest.json", maxManifest)
		if err != nil {
			return nil, corrupt(err)
		}
		var saved envelope
		if err := jsonstrict.Decode(data, &saved); err != nil {
			return nil, corrupt(err)
		}
		payload, encodeErr := json.Marshal(saved.Manifest)
		if encodeErr != nil || digest(payload) != saved.SHA256 || saved.Manifest.SchemaVersion != schemaVersion {
			return nil, ErrCorrupt
		}
		j.m = saved.Manifest
		if j.m.InputSHA256 != inputHash || j.m.Settings != settings {
			return nil, ErrMismatch
		}
		if err := j.verify(ctx); err != nil {
			return nil, err
		}
		return j, nil
	}
	generation, err := randomName("run-")
	if err != nil {
		return nil, err
	}
	if err := slot.Mkdir(generation, 0700); err != nil {
		return nil, err
	}
	j.root, err = slot.OpenRoot(generation)
	if err != nil {
		return nil, err
	}
	j.m = manifest{SchemaVersion: schemaVersion, InputSHA256: inputHash, Settings: settings, Completed: map[string]completed{}}
	if err := j.commit(ctx, j.m); err != nil {
		return nil, err
	}
	if _, err := slot.Lstat("current.json"); err == nil {
		j.pendingPointer = generation
		return j, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, corrupt(err)
	}
	if err := writePointer(ctx, slot, generation); err != nil {
		return nil, err
	}
	return j, nil
}

func writePointer(ctx context.Context, slot *os.Root, generation string) error {
	data, err := json.Marshal(pointer{schemaVersion, generation})
	if err != nil {
		return err
	}
	return atomicWrite(ctx, slot, "current.json", data)
}

func (j *Job) usable() error {
	if j.closed || j.poisoned {
		return ErrState
	}
	return nil
}
func (j *Job) commit(ctx context.Context, next manifest) error {
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope{Manifest: next, SHA256: digest(payload)})
	if err != nil || len(data) > maxManifest {
		return ErrCorrupt
	}
	if err := j.write(ctx, j.root, "manifest.json", data); err != nil {
		j.poisoned = true
		return fmt.Errorf("commit recovery manifest: %w", err)
	}
	j.m = next
	if j.pendingPointer != "" && (len(next.Completed) > 0 || next.Complete) {
		if err := writePointer(ctx, j.slot, j.pendingPointer); err != nil {
			j.poisoned = true
			return fmt.Errorf("commit recovery pointer: %w", err)
		}
		j.pendingPointer = ""
	}
	return nil
}

// Close releases handles and the exclusive lock, never deleting recovery data.
func (j *Job) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	var errs []error
	if j.root != nil {
		errs = append(errs, j.root.Close())
	}
	if j.slot != nil {
		errs = append(errs, j.slot.Close())
	}
	if j.lock != nil {
		errs = append(errs, j.lock.Close())
	}
	return errors.Join(errs...)
}
