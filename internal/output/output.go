// Package output publishes transcripts and supports offline display-name edits.
package output

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/transcript"
)

var (
	ErrConflict = errors.New("output or canonical sidecar already exists; choose another output name")
	ErrPath     = errors.New("invalid output destination")
	ErrState    = errors.New("output destination is closed or already written")
	ErrCleanup  = errors.New("output rollback incomplete; inspect the destination before retrying")
	ErrStorage  = errors.New("output storage failed; check permissions, space and filesystem support")
	// ErrPublishLocked means another command kept the output directory locked
	// after transcription finished; the completed job can be published again.
	ErrPublishLocked = errors.New("output directory is locked by another turnbeam command; rerun with --resume to publish without repeating transcription")
)

// publishLockWait covers a concurrent rename or export, which hold the lock briefly.
var publishLockWait = 5 * time.Second

// Destination owns private staging until Close. Use it for one invocation only;
// methods are not concurrent. Preflight checks happen before expensive inference.
type Destination struct {
	root                 *os.Root
	stage, main, sidecar string
	stdout               io.Writer
	opts                 transcript.RenderOptions
	closed, written      bool
	source               *source
	reuseSidecar         bool
	link                 func(string, string) error
	sync                 func() error
	remove               func(string) error
	stageFile            func(context.Context, string, []byte) (os.FileInfo, error)
}

// Open selects stdout for an empty path. File output requires an existing parent
// and hard-link support for atomic no-replace publication; unsupported volumes
// fail before inference. A canonical JSON destination is written only once.
func Open(ctx context.Context, path string, opts transcript.RenderOptions, stdout io.Writer) (*Destination, error) {
	d, err := openDestination(ctx, path, opts, stdout, nil)
	return d, storageError(err)
}

func storageError(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrStorage, err)
}

func openDestination(ctx context.Context, path string, opts transcript.RenderOptions, stdout io.Writer, input *source) (_ *Destination, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	d := &Destination{stdout: stdout, opts: opts, source: input}
	if path == "" {
		if stdout == nil {
			return nil, ErrPath
		}
		return d, nil
	}
	if strings.TrimSpace(path) == "" || strings.HasSuffix(path, string(os.PathSeparator)) {
		return nil, ErrPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	d.main = filepath.Base(abs)
	if d.main == "." || d.main == ".." || d.main == string(os.PathSeparator) {
		return nil, ErrPath
	}
	stem := strings.TrimSuffix(d.main, filepath.Ext(d.main))
	if stem == "" {
		stem = d.main
	}
	d.sidecar = stem + ".transcript.json"
	if strings.HasSuffix(strings.ToLower(d.main), ".transcript.json") {
		if opts.Format != transcript.FormatJSON {
			return nil, fmt.Errorf("%w: canonical sidecar name requires JSON format", ErrPath)
		}
		d.sidecar = d.main
	}
	d.root, err = os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("open output directory: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
		}
	}()
	d.reuseSidecar = input != nil && d.main != d.sidecar && input.isPath(filepath.Join(filepath.Dir(abs), d.sidecar))
	for _, name := range []string{d.main, d.sidecar} {
		if name == d.sidecar && d.reuseSidecar {
			if err := input.check(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := d.root.Lstat(name); err == nil {
			return nil, ErrConflict
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	d.stage, err = newStage(d.root)
	if err != nil {
		return nil, err
	}
	d.link, d.remove = d.root.Link, d.root.Remove
	d.sync = func() error { return syncDir(d.root) }
	d.stageFile = d.writeStage
	// Probe the same filesystem operation used for final no-clobber publication.
	if _, err = d.writeStage(ctx, "probe", nil); err != nil {
		return nil, err
	}
	if err = d.link(filepath.Join(d.stage, "probe"), filepath.Join(d.stage, "probe-link")); err != nil {
		return nil, fmt.Errorf("output volume requires atomic hard-link support: %w", err)
	}
	if err = d.sync(); err != nil {
		return nil, fmt.Errorf("sync output directory: %w", err)
	}
	return d, nil
}

func newStage(root *os.Root) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	name := ".turnbeam-output-" + hex.EncodeToString(nonce[:])
	if err := root.Mkdir(name, 0700); err != nil {
		return "", err
	}
	return name, nil
}

func syncDir(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type reader struct {
	ctx    context.Context
	source io.Reader
}

func (r reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func (d *Destination) writeStage(ctx context.Context, name string, data []byte) (os.FileInfo, error) {
	f, err := d.root.OpenFile(filepath.Join(d.stage, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := io.Copy(f, reader{ctx, bytes.NewReader(data)})
	info, statErr := f.Stat()
	err = errors.Join(writeErr, statErr, f.Sync(), f.Close(), ctx.Err())
	return info, err
}

// Write validates/renders everything before publishing. The sidecar is durable
// first, and the requested output is the completion marker, never a partial file.
// Flat filenames cannot appear simultaneously: interruption may leave only a
// valid sidecar. Ordinary errors roll back owned, unchanged files main-first.
func (d *Destination) Write(ctx context.Context, input *transcript.Transcript) (err error) {
	defer func() { err = storageError(err) }()
	if d.closed || d.written {
		return ErrState
	}
	d.written = true
	data, err := transcript.Render(ctx, input, d.opts)
	if err != nil {
		return err
	}
	if d.source != nil {
		if err := d.source.check(ctx); err != nil {
			return err
		}
	}
	if d.root == nil {
		_, err = io.Copy(d.stdout, reader{ctx, bytes.NewReader(data)})
		return errors.Join(err, ctx.Err())
	}
	// Coordinate publication with offline name edits. The lock is short-lived:
	// inference never holds it. Export already owns its source directory lock.
	if d.source == nil || !d.source.ownsDirectory(d.root) {
		lock, lockErr := filelock.WaitDirectory(ctx, d.root, publishLockWait)
		if d.source == nil && errors.Is(lockErr, filelock.ErrBusy) {
			return errors.Join(ErrPublishLocked, lockErr)
		}
		if lockErr != nil {
			return fmt.Errorf("lock output directory: %w", lockErr)
		}
		defer func() { err = errors.Join(err, lock.Close()) }()
	}
	canonical := data
	if d.opts.Format != transcript.FormatJSON && !d.reuseSidecar {
		canonical, err = transcript.Encode(input)
		if err != nil {
			return err
		}
	}
	mainInfo, err := d.stageFile(ctx, "main", data)
	if err != nil {
		return fmt.Errorf("stage transcript: %w", err)
	}
	var sideInfo os.FileInfo
	if d.main != d.sidecar && !d.reuseSidecar {
		sideInfo, err = d.stageFile(ctx, "sidecar", canonical)
		if err != nil {
			return fmt.Errorf("stage canonical sidecar: %w", err)
		}
	}
	if d.reuseSidecar {
		f, err := d.root.Open(d.sidecar)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return fmt.Errorf("sync source sidecar: %w", err)
		}
	}
	mainPublished, sidePublished := false, false
	defer func() {
		if err == nil {
			return
		}
		rollback := func(name string, info os.FileInfo) error {
			current, e := d.root.Lstat(name)
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			if !current.Mode().IsRegular() || !os.SameFile(current, info) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
				return ErrCleanup
			}
			return d.remove(name)
		}
		var cleanup error
		if mainPublished {
			cleanup = rollback(d.main, mainInfo)
		}
		if sidePublished && cleanup == nil {
			cleanup = rollback(d.sidecar, sideInfo)
		}
		if cleanup != nil {
			err = errors.Join(err, ErrCleanup, cleanup)
		}
		if e := d.sync(); e != nil {
			err = errors.Join(err, ErrCleanup, e)
		}
	}()
	publish := func(staged, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.source != nil {
			if err := d.source.check(ctx); err != nil {
				return err
			}
		}
		if err := d.link(filepath.Join(d.stage, staged), name); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.Join(ErrConflict, err)
			}
			return err
		}
		return nil
	}
	if d.main != d.sidecar && !d.reuseSidecar {
		if err = publish("sidecar", d.sidecar); err != nil {
			return err
		}
		sidePublished = true
		if err = d.sync(); err != nil {
			return err
		}
	}
	if err = publish("main", d.main); err != nil {
		return err
	}
	mainPublished = true
	if err = d.sync(); err != nil {
		return err
	}
	if d.source != nil {
		if err := d.source.check(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Close removes only private staging, not committed output or an orphan sidecar.
func (d *Destination) Close() (err error) {
	defer func() { err = storageError(err) }()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.root == nil {
		return nil
	}
	if d.stage != "" {
		err = d.root.RemoveAll(d.stage)
		if err != nil {
			err = errors.Join(ErrCleanup, err)
		}
	}
	return errors.Join(err, d.root.Close())
}
