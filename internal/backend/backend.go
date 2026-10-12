// Package backend defines the blob backend interface used for pack files and
// its local filesystem implementation.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Backend stores immutable blobs by key. Keys are slash-separated relative
// paths such as "packs/01J....pack".
type Backend interface {
	// Put stores r under key. The blob is durable when Put returns nil. If
	// size >= 0, r must yield exactly size bytes.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get returns n bytes of the blob starting at off. n < 0 reads to the end.
	Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
	// Delete removes the blob. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List calls fn for every key with the given prefix, in lexical order.
	List(ctx context.Context, prefix string, fn func(key string, size int64) error) error
}

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("backend: not found")

// ValidKey reports whether key is a clean relative slash path without "."
// or ".." elements.
func ValidKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("backend: invalid key %q", key)
	}
	if path.Clean(key) != key {
		return fmt.Errorf("backend: key %q is not clean", key)
	}
	for _, el := range strings.Split(key, "/") {
		if el == "." || el == ".." || strings.HasPrefix(el, tmpPrefix) {
			return fmt.Errorf("backend: invalid key %q", key)
		}
	}
	return nil
}

// ReaderAt adapts a blob to io.ReaderAt using ranged Gets.
type ReaderAt struct {
	ctx context.Context
	b   Backend
	key string
}

// NewReaderAt returns an io.ReaderAt for key.
func NewReaderAt(ctx context.Context, b Backend, key string) *ReaderAt {
	return &ReaderAt{ctx: ctx, b: b, key: key}
}

// ReadAt implements io.ReaderAt.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	rc, err := r.b.Get(r.ctx, r.key, off, int64(len(p)))
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
