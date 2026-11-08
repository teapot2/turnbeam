package setup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/teapot2/turnbeam/internal/safeio"
)

type entry struct {
	kind byte
	mode fs.FileMode
	size int64
	sum  string
	link string
}

// readArchive builds a trusted inventory, optionally extracting into a fresh root.
// Symlinks are created last; os.Root also confines all filesystem operations.
func readArchive(ctx context.Context, source *os.File, prefix string, dest *os.Root) (_ map[string]entry, err error) {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("open runtime archive: %w", err)
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: safeio.Reader(ctx, gz), N: (128 << 20) + 1}
	defer func() {
		// The cap truncates the stream, which tar reports as a damaged archive.
		if err != nil && limited.N <= 0 {
			err = errors.New("runtime archive is larger than 128 MiB unpacked")
		}
	}()
	tr := tar.NewReader(limited)
	entries := make(map[string]entry)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read runtime archive: %w", err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != prefix && !strings.HasPrefix(name, prefix+"/") {
			return nil, errors.New("runtime archive has an unexpected root")
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, prefix), "/")
		if name == "" {
			name = "."
		}
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") || path.Clean(name) != name || len(entries) >= 512 {
			return nil, errors.New("unsafe runtime archive path or entry count")
		}
		if _, exists := entries[name]; exists {
			return nil, errors.New("duplicate runtime archive entry")
		}
		e := entry{kind: h.Typeflag, mode: 0600, size: h.Size, link: h.Linkname}
		if h.Mode&0111 != 0 {
			e.mode = 0700
		}
		switch h.Typeflag {
		case tar.TypeDir:
			e.mode = 0700
			if dest != nil {
				err = dest.MkdirAll(name, e.mode)
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > 64<<20 {
				return nil, errors.New("runtime archive member is too large")
			}
			var out *os.File
			var input io.Reader = tr
			if dest != nil {
				if err := dest.MkdirAll(path.Dir(name), 0700); err != nil {
					return nil, err
				}
				out, err = dest.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, e.mode)
				if err != nil {
					return nil, err
				}
				input = io.TeeReader(tr, out)
			}
			e.sum, err = digest(ctx, input, h.Size)
			if out != nil {
				err = errors.Join(err, out.Sync(), out.Close())
			}
		case tar.TypeSymlink:
			target := path.Join(path.Dir(name), h.Linkname)
			if h.Linkname == "" || path.IsAbs(h.Linkname) || strings.Contains(h.Linkname, "\\") || !filepath.IsLocal(target) {
				return nil, errors.New("unsafe runtime symlink")
			}
		default:
			return nil, errors.New("unsupported runtime archive entry type")
		}
		if err != nil {
			return nil, err
		}
		entries[name] = e
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, fmt.Errorf("verify archive compression: %w", err)
	}
	if limited.N == 0 || len(entries) == 0 {
		return nil, errors.New("runtime archive is empty or exceeds extraction limits")
	}
	// A path through an archived symlink could resolve outside the checked
	// layout, so no entry may sit inside a symlinked folder.
	for name := range entries {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if entries[dir].kind == tar.TypeSymlink {
				return nil, errors.New("runtime archive entry inside a symlinked folder")
			}
		}
	}
	if dest != nil {
		for name, e := range entries {
			if e.kind == tar.TypeSymlink {
				if err := dest.Symlink(e.link, name); err != nil {
					return nil, err
				}
			}
		}
	}
	return entries, nil
}

func verifyRuntime(ctx context.Context, root *os.Root, prefix string) error {
	archive, err := root.Open("runtime.tar.gz")
	if err != nil {
		return err
	}
	defer archive.Close()
	entries, err := readArchive(ctx, archive, prefix, nil)
	if err != nil {
		return err
	}
	// Do not trust a writable installation manifest as the source of file hashes.
	// Rebuild them from the archive whose SHA-256 is pinned in the application.
	err = fs.WalkDir(root.FS(), "runtime", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(name, "runtime/")
		if name == "runtime" {
			rel = "."
		}
		e, exists := entries[rel]
		if !exists {
			return fmt.Errorf("%w: unexpected runtime file", ErrIntegrity)
		}
		stat, err := root.Lstat(name)
		if err != nil {
			return err
		}
		switch e.kind {
		case tar.TypeDir:
			if !stat.IsDir() || stat.Mode().Perm() != 0700 {
				return ErrIntegrity
			}
		case tar.TypeSymlink:
			link, err := root.Readlink(name)
			if err != nil {
				return fmt.Errorf("%w: inspect runtime symlink: %w", ErrIntegrity, err)
			}
			if link != e.link {
				return fmt.Errorf("%w: runtime symlink changed", ErrIntegrity)
			}
		case tar.TypeReg:
			if err := verifyFile(ctx, root, artifact{name: name, sha256: e.sum, size: e.size}, e.mode); err != nil {
				return err
			}
		}
		delete(entries, rel)
		return nil
	})
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: runtime files are missing", ErrIntegrity)
	}
	return nil
}
