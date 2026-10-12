package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// tmpPrefix marks in-progress Put files. Keys may not use it.
const tmpPrefix = ".tmp-"

// FS stores blobs as files under a root directory. Put writes a temporary
// file next to the target, fsyncs it, renames it into place and fsyncs the
// directory, so a blob is either absent or complete.
type FS struct {
	root string
}

// NewFS returns a backend rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}
	return &FS{root: dir}, nil
}

func (b *FS) path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(b.root, filepath.FromSlash(key)), nil
}

// Put implements Backend.
func (b *FS) Put(ctx context.Context, key string, r io.Reader, size int64) (err error) {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tmpPrefix+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	n, err := io.Copy(f, ctxReader{ctx, r})
	if err != nil {
		return fmt.Errorf("backend: put %s: %w", key, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("backend: put %s: got %d bytes, want %d", key, n, size)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	return syncDir(dir)
}

// Get implements Backend.
func (b *FS) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, fmt.Errorf("backend: negative offset %d", off)
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}
	if n < 0 {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("backend: %w", err)
		}
		n = max(st.Size()-off, 0)
	}
	return &sectionCloser{io.NewSectionReader(f, off, n), f}, nil
}

// Delete implements Backend.
func (b *FS) Delete(ctx context.Context, key string) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("backend: %w", err)
	}
	return syncDir(filepath.Dir(p))
}

// List implements Backend.
func (b *FS) List(ctx context.Context, prefix string, fn func(key string, size int64) error) error {
	var keys []string
	sizes := map[string]int64{}
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		keys = append(keys, key)
		sizes[key] = info.Size()
		return nil
	})
	if err != nil {
		return fmt.Errorf("backend: list: %w", err)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k, sizes[k]); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("backend: sync dir: %w", err)
	}
	return nil
}

type sectionCloser struct {
	*io.SectionReader
	f *os.File
}

func (s *sectionCloser) Close() error { return s.f.Close() }

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
