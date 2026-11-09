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
	"github.com/teapot2/turnbeam/internal/safeio"
	"github.com/teapot2/turnbeam/internal/transcript"
)

var (
	ErrConflict = errors.New("output or canonical sidecar already exists; choose another output name")
	// ErrNoFreeName means every numbered alternative to a taken transcript name
	// is taken too.
	ErrNoFreeName = errors.New("output name and its numbered alternatives already exist; choose another output name")
	ErrPath       = errors.New("invalid output destination")
	ErrState      = errors.New("output destination is closed or already written")
	ErrCleanup    = errors.New("output rollback incomplete; inspect the destination before retrying")
	ErrStorage    = errors.New("output storage failed; check permissions, space and filesystem support")
	// ErrPublishLocked means another command kept the output directory locked
	// after transcription finished; the completed job can be published again.
	ErrPublishLocked = errors.New("output directory is locked by another turnbeam command; rerun with --resume to publish without repeating transcription")
)

// publishLockWait covers a concurrent rename or export, which hold the lock briefly.
var publishLockWait = 5 * time.Second

// maxNumbered bounds the numbered names tried for a taken transcript name.
const maxNumbered = 999

// Destination owns private staging until Close. Use it for one invocation only;
// methods are not concurrent. Preflight checks happen before expensive inference.
type Destination struct {
	root                 *os.Root
	dir, requested       string
	conflict             string
	stage, main, sidecar string
	// numbered moves a taken name to name-2, name-3, …; number is the current one.
	numbered        bool
	number          int
	stdout          io.Writer
	opts            transcript.RenderOptions
	closed, written bool
	source          *source
	reuseSidecar    bool
	link            func(string, string) error
	sync            func() error
	remove          func(string) error
	stageFile       func(context.Context, string, []byte) (os.FileInfo, error)
}

// Open selects stdout for an empty path. File output requires an existing parent
// and hard-link support for atomic no-replace publication; unsupported volumes
// fail before inference. Existing files are never replaced: when the path or its
// sidecar is taken, before or during the run, the transcript is published under
// the first free numbered name (meeting-2.md with meeting-2.transcript.json).
func Open(ctx context.Context, path string, opts transcript.RenderOptions, stdout io.Writer) (*Destination, error) {
	d, err := openDestination(ctx, path, opts, stdout, nil)
	return d, storageError(err)
}

// Path returns the transcript file's absolute path, or "" for stdout. It can
// change until Write succeeds.
func (d *Destination) Path() string {
	if d.root == nil {
		return ""
	}
	return filepath.Join(d.dir, d.main)
}

// Sidecar returns the canonical JSON sidecar's absolute path, or "" for stdout.
func (d *Destination) Sidecar() string {
	if d.root == nil {
		return ""
	}
	return filepath.Join(d.dir, d.sidecar)
}

// Conflict returns the absolute path of the existing file, the requested
// transcript or its sidecar, that moved the output to a numbered name; it is ""
// when the requested name was used.
func (d *Destination) Conflict() string {
	if d.root == nil || d.conflict == "" {
		return ""
	}
	return filepath.Join(d.dir, d.conflict)
}

// DefaultPath names a recording's transcript next to it: meeting.mp4 becomes
// meeting.md, or meeting.txt, .srt or .vtt for those formats. JSON is the
// canonical meeting.transcript.json, which is its own sidecar.
func DefaultPath(recording string, format transcript.Format) string {
	ext := ".md"
	switch format {
	case transcript.FormatText:
		ext = ".txt"
	case transcript.FormatJSON:
		ext = ".transcript.json"
	case transcript.FormatSRT:
		ext = ".srt"
	case transcript.FormatVTT:
		ext = ".vtt"
	}
	base := filepath.Base(recording)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		stem = base
	}
	return filepath.Join(filepath.Dir(recording), stem+ext)
}

// Available returns path when neither it nor its sidecar exists, otherwise the
// first free numbered name Open would choose now.
func Available(path string) (string, error) {
	dir, main := filepath.Split(path)
	for n := 1; n <= maxNumbered; n++ {
		name := numberedName(main, n)
		taken := false
		for _, candidate := range []string{name, sidecarName(name)} {
			if _, err := os.Lstat(filepath.Join(dir, candidate)); err == nil {
				taken = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
		if !taken {
			return filepath.Join(dir, name), nil
		}
	}
	return "", ErrNoFreeName
}

// numberedName inserts -n before the extension; n == 1 is the name itself.
// The canonical .transcript.json suffix stays whole.
func numberedName(name string, n int) string {
	if n == 1 {
		return name
	}
	base, ext := name, filepath.Ext(name)
	if strings.HasSuffix(strings.ToLower(name), ".transcript.json") {
		ext = name[len(name)-len(".transcript.json"):]
	}
	if ext != name {
		base = strings.TrimSuffix(name, ext)
	} else {
		ext = ""
	}
	return fmt.Sprintf("%s-%d%s", base, n, ext)
}

// sidecarName names the canonical JSON beside a transcript file; a canonical
// name is its own sidecar.
func sidecarName(main string) string {
	if strings.HasSuffix(strings.ToLower(main), ".transcript.json") {
		return main
	}
	stem := strings.TrimSuffix(main, filepath.Ext(main))
	if stem == "" {
		stem = main
	}
	return stem + ".transcript.json"
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
	d.sidecar = sidecarName(d.main)
	if d.sidecar == d.main && opts.Format != transcript.FormatJSON {
		return nil, fmt.Errorf("%w: canonical sidecar name requires JSON format", ErrPath)
	}
	// Only new transcripts are numbered; an export names an exact file.
	d.dir, d.requested, d.numbered, d.number = filepath.Dir(abs), d.main, input == nil, 1
	d.root, err = os.OpenRoot(d.dir)
	if err != nil {
		return nil, fmt.Errorf("open output directory: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
		}
	}()
	d.reuseSidecar = input != nil && d.main != d.sidecar && input.isPath(filepath.Join(d.dir, d.sidecar))
	if d.reuseSidecar {
		if err := input.check(ctx); err != nil {
			return nil, err
		}
	}
	if taken, err := d.taken(); err != nil {
		return nil, err
	} else if taken != "" {
		if err := d.next(); err != nil {
			return nil, err
		}
	}
	d.stage, err = newStage(d.root)
	if err != nil {
		return nil, err
	}
	d.link, d.remove = d.root.Link, d.root.Remove
	d.sync = func() error { return safeio.SyncDir(d.root) }
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
	rand.Read(nonce[:]) // never fails since Go 1.24
	name := ".turnbeam-output-" + hex.EncodeToString(nonce[:])
	if err := root.Mkdir(name, 0700); err != nil {
		return "", err
	}
	return name, nil
}

// taken returns the current transcript or sidecar name if it exists.
func (d *Destination) taken() (string, error) {
	for _, name := range []string{d.main, d.sidecar} {
		if name == d.sidecar && d.reuseSidecar {
			continue
		}
		if _, err := d.root.Lstat(name); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// next moves a numbered destination from its taken name to the next free one,
// remembering which existing file moved it off the requested name.
func (d *Destination) next() error {
	if !d.numbered {
		return ErrConflict
	}
	if d.number == 1 {
		name, err := d.taken()
		if err != nil {
			return err
		}
		d.conflict = name
	}
	for d.number < maxNumbered {
		d.number++
		d.main = numberedName(d.requested, d.number)
		d.sidecar = sidecarName(d.main)
		if taken, err := d.taken(); err != nil || taken == "" {
			return err
		}
	}
	return ErrNoFreeName
}

func (d *Destination) writeStage(ctx context.Context, name string, data []byte) (os.FileInfo, error) {
	f, err := d.root.OpenFile(filepath.Join(d.stage, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := io.Copy(f, safeio.Reader(ctx, bytes.NewReader(data)))
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
		_, err = io.Copy(d.stdout, safeio.Reader(ctx, bytes.NewReader(data)))
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
	// A name taken while staging moves a numbered destination to the next free
	// one; the staged files are linked again under that name.
	for {
		err = d.publish(ctx, mainInfo, sideInfo)
		if !errors.Is(err, ErrConflict) || errors.Is(err, ErrCleanup) || !d.numbered {
			return err
		}
		if err = d.next(); err != nil {
			return err
		}
	}
}

// publish links the staged pair under the current names, sidecar first, and
// rolls back owned, unchanged files main-first on error.
func (d *Destination) publish(ctx context.Context, mainInfo, sideInfo os.FileInfo) (err error) {
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
