package output

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/transcript"
)

var (
	ErrSourceChanged = errors.New("source transcript changed; reopen it before retrying")
	// ErrCommit indicates a complete replacement whose durability is uncertain.
	ErrCommit = errors.New("speaker names were replaced but directory sync failed; inspect the transcript before retrying")
	ErrSource = errors.New("inspect transcript: expected a valid, accessible canonical JSON file")
)

// Bound canonical-file reads; oversized inputs fail, never truncate or migrate.
const maxTranscriptBytes = 256 << 20

// Export reads only canonical JSON. No credentials, media or inference are used.
// The exact input sidecar may be reused in place, but is never rewritten.
func Export(ctx context.Context, input, path string, opts transcript.RenderOptions, stdout io.Writer) (err error) {
	defer func() { err = storageError(err) }()
	if err := opts.Validate(); err != nil {
		return err
	}
	s, err := openSource(ctx, input)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	d, err := openDestination(ctx, path, opts, stdout, s)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	return d.Write(ctx, s.transcript)
}

// Rename atomically updates the supplied JSON's display names only. Existing
// human exports are intentionally untouched; re-export to a new name afterward.
func Rename(ctx context.Context, path string, names map[int]string) (err error) {
	defer func() { err = storageError(err) }()
	s, err := openSource(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	updated, err := transcript.Rename(ctx, s.transcript, names)
	if err != nil {
		return err
	}
	return s.save(ctx, updated)
}

// Hold a nonblocking advisory parent-directory lock across read/check/commit.
// This coordinates our own offline commands across atomic inode replacement;
// fingerprints also detect non-cooperating edits observed before publication.
type source struct {
	root       *os.Root
	lock       *os.File
	name       string
	info       os.FileInfo
	digest     [32]byte
	transcript *transcript.Transcript
	commit     func(string, string) error
	sync       func() error
}

func openSource(ctx context.Context, path string) (_ *source, err error) {
	defer func() {
		if err != nil && !errors.Is(err, filelock.ErrBusy) {
			err = errors.Join(ErrSource, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("open transcript directory: %w", err)
	}
	s := &source{root: root, name: filepath.Base(abs), commit: root.Rename, sync: func() error { return syncDir(root) }}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	s.lock, err = filelock.TryDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("lock transcript directory: %w", err)
	}
	s.info, err = root.Lstat(s.name)
	if err != nil {
		return nil, fmt.Errorf("inspect transcript: %w", err)
	}
	if !s.info.Mode().IsRegular() || s.info.Size() < 0 || s.info.Size() > maxTranscriptBytes {
		return nil, fmt.Errorf("%w: transcript must be a regular file of at most 256 MiB, not a symlink", ErrPath)
	}
	f, err := root.Open(s.name)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader{ctx, f}, maxTranscriptBytes+1))
	err = errors.Join(readErr, f.Close(), ctx.Err())
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	if len(data) > maxTranscriptBytes || int64(len(data)) != s.info.Size() {
		return nil, ErrSourceChanged
	}
	s.digest = sha256.Sum256(data)
	if err = s.check(ctx); err != nil {
		return nil, err
	}
	s.transcript, err = transcript.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}
	return s, nil
}

func unchanged(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode() == a.Mode() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *source) check(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := s.root.Lstat(s.name)
	if err != nil {
		return errors.Join(ErrSourceChanged, err)
	}
	if !unchanged(s.info, info) {
		return ErrSourceChanged
	}
	f, err := s.root.Open(s.name)
	if err != nil {
		return errors.Join(ErrSourceChanged, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err = f.Stat()
	if err != nil || !unchanged(s.info, info) {
		return errors.Join(ErrSourceChanged, err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader{ctx, f}, maxTranscriptBytes+1))
	if err != nil {
		return err
	}
	info, err = s.root.Lstat(s.name)
	if err != nil || !unchanged(s.info, info) || n != s.info.Size() || [32]byte(hash.Sum(nil)) != s.digest {
		return errors.Join(ErrSourceChanged, err)
	}
	return ctx.Err()
}

func (s *source) isPath(path string) bool {
	if filepath.Base(path) != s.name {
		return false
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false
	}
	own, err := s.root.Stat(".")
	return err == nil && os.SameFile(own, parent)
}

func (s *source) ownsDirectory(root *os.Root) bool {
	parent, err := root.Stat(".")
	if err != nil {
		return false
	}
	own, err := s.root.Stat(".")
	return err == nil && os.SameFile(own, parent)
}

func (s *source) save(ctx context.Context, updated *transcript.Transcript) (err error) {
	data, err := transcript.Encode(updated)
	if err != nil {
		return err
	}
	if err = s.check(ctx); err != nil {
		return err
	}
	stage, err := newStage(s.root)
	if err != nil {
		return err
	}
	defer func() {
		if cleanup := s.root.RemoveAll(stage); cleanup != nil {
			err = errors.Join(err, ErrCleanup, cleanup)
		}
	}()
	d := Destination{root: s.root, stage: stage}
	if _, err = d.writeStage(ctx, "renamed", data); err != nil {
		return fmt.Errorf("stage speaker names: %w", err)
	}
	// Check directory syncing before replacement, and check the original again
	// immediately before commit. Never roll back a post-rename durability error.
	if err = s.sync(); err != nil {
		return err
	}
	if err = s.check(ctx); err != nil {
		return err
	}
	if err = s.commit(filepath.Join(stage, "renamed"), s.name); err != nil {
		return fmt.Errorf("replace speaker names: %w", err)
	}
	if err = s.sync(); err != nil {
		return errors.Join(ErrCommit, err)
	}
	return nil
}

func (s *source) close() error {
	var err error
	if s.lock != nil {
		err = s.lock.Close()
	}
	return errors.Join(err, s.root.Close())
}
