package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/sanitize"
)

const chunkSize = 256 * 1024

var extRe = regexp.MustCompile(`[^a-z0-9]`)
var keyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

var ErrStorage = errors.New("storage error")
var ErrTooLarge = errors.New("file exceeds size limit")

// SanitizeName keeps an uploaded name displayable (display only; blobs are always named by uuid).
func SanitizeName(v any, limit int) string {
	raw := sanitize.Fold(v)
	raw = strings.ReplaceAll(raw, "\\", "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.Join(strings.Fields(raw), " ")
	if raw == "" {
		return "file"
	}
	return sanitize.Cap(raw, limit)
}

func NewKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func blobPath(cfg *config.Config, key string) (string, error) {
	if !keyRe.MatchString(key) {
		return "", ErrStorage
	}
	return filepath.Join(cfg.AttachmentsDir, key[:2], key), nil
}

// Save streams r into the blob store, returning (size, sha256hex). Removes partials on error.
func Save(cfg *config.Config, r io.Reader, key string, limit int64) (int64, string, error) {
	path, err := blobPath(cfg, key)
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, "", err
	}
	digest := sha256.New()
	var size int64
	buf := make([]byte, chunkSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > limit {
				f.Close()
				os.Remove(path)
				return 0, "", ErrTooLarge
			}
			digest.Write(buf[:n])
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(path)
				return 0, "", werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(path)
			return 0, "", rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return 0, "", err
	}
	return size, hex.EncodeToString(digest.Sum(nil)), nil
}

// Open returns a reader for a stored blob.
func Open(cfg *config.Config, key string) (*os.File, error) {
	path, err := blobPath(cfg, key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrStorage
	}
	return f, err
}

// ReadAll returns the full bytes of a stored blob.
func ReadAll(cfg *config.Config, key string) ([]byte, error) {
	f, err := Open(cfg, key)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func Remove(cfg *config.Config, key string) bool {
	path, err := blobPath(cfg, key)
	if err != nil {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}

func Stats(cfg *config.Config) (int, int64) {
	count := 0
	var total int64
	filepath.WalkDir(cfg.AttachmentsDir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
			count++
		}
		return nil
	})
	return count, total
}
