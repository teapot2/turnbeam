package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// FileSHA256 fingerprints a regular media file without buffering it in memory.
func FileSHA256(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: expected a regular file", ErrInvalidMedia)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() {
		return "", fmt.Errorf("%w: expected a regular file", ErrInvalidMedia)
	}
	h := sha256.New()
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	end, err := f.Stat()
	if err != nil {
		return "", err
	}
	if end.Size() != stat.Size() || !end.ModTime().Equal(stat.ModTime()) {
		return "", fmt.Errorf("%w: media changed while hashing", ErrInvalidMedia)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
