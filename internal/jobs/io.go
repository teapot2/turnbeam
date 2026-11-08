package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/teapot2/turnbeam/internal/safeio"
)

const maxManifest = 8 << 20
const maxResponse = 4 << 20
const maxRTTM = 64 << 20

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validHash(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == value
}

func validGeneration(value string) bool {
	if !strings.HasPrefix(value, "run-") {
		return false
	}
	suffix := strings.TrimPrefix(value, "run-")
	data, err := hex.DecodeString(suffix)
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == suffix
}
func randomName(prefix string) string {
	var value [16]byte
	rand.Read(value[:]) // never fails since Go 1.24
	return prefix + hex.EncodeToString(value[:])
}
func readFile(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() < 0 || stat.Size() > limit {
		return nil, ErrCorrupt
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(safeio.Reader(ctx, f), limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != stat.Size() || int64(len(data)) > limit {
		return nil, ErrCorrupt
	}
	return data, nil
}

func readBounded(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(safeio.Reader(ctx, r), limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrCorrupt
	}
	return data, nil
}

// Rename is the commit point; directory sync makes that commit durable. A failure
// after rename is uncertain, so callers poison the session and must reopen it.
func atomicWrite(ctx context.Context, root *os.Root, name string, data []byte) error {
	tmp := randomName(".tmp-")
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, writeErr := io.Copy(f, safeio.Reader(ctx, bytes.NewReader(data)))
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return safeio.SyncDir(root)
}

func artifactName(prefix string, a artifact) string { return prefix + "-" + a.SHA256 }
func (j *Job) readArtifact(ctx context.Context, prefix string, a artifact, limit int64) ([]byte, error) {
	if !validHash(a.SHA256) || a.Bytes < 0 || a.Bytes > limit {
		return nil, ErrCorrupt
	}
	data, err := readFile(ctx, j.root, artifactName(prefix, a), limit)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != a.Bytes || digest(data) != a.SHA256 {
		return nil, ErrCorrupt
	}
	return data, nil
}

func (j *Job) writeArtifact(ctx context.Context, prefix string, data []byte, limit int) (artifact, error) {
	if len(data) > limit {
		return artifact{}, ErrCorrupt
	}
	a := artifact{SHA256: digest(data), Bytes: int64(len(data))}
	if err := atomicWrite(ctx, j.root, artifactName(prefix, a), data); err != nil {
		return artifact{}, err
	}
	return a, nil
}
