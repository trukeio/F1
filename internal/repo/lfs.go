package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/object"
)

// LFSPointer returns the canonical Git LFS pointer for content with the
// given SHA-256 and size.
func LFSPointer(sha256 [32]byte, size uint64) []byte {
	return fmt.Appendf(nil, "version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", hex.EncodeToString(sha256[:]), size)
}

// lfsEntry turns a plain file written over an LFS entry into an LFS entry
// with the new content and its canonical pointer, so that Git users who
// chose LFS keep a consistent repository (doc/plan.md, "LFS is for
// compatibility only"). The executable flag of the old entry is kept. The
// pointer bytes are stored as a File too, so the pointer blob can be
// fetched by its Git id.
func (r *Repo) lfsEntry(ctx context.Context, old, e *object.Entry) (*object.Entry, error) {
	d, ok, err := r.store.Cache().Digest(ctx, e.Target)
	if err != nil {
		return nil, err
	}
	if !ok {
		dg, err := content.Digest(ctx, r.store, e.Target)
		if err != nil {
			return nil, err
		}
		d.SHA256 = dg.SHA256
	}
	size, err := r.FileSize(ctx, e.Target)
	if err != nil {
		return nil, err
	}
	ptr := LFSPointer(d.SHA256, size)
	if _, err := content.Ingest(ctx, r.store, bytes.NewReader(ptr), int64(len(ptr)), content.Options{}); err != nil {
		return nil, err
	}
	out := object.Entry{Kind: object.KindLFS, Target: e.Target}
	attrs := object.Attrs{}
	if e.Attrs != nil {
		attrs = *e.Attrs
	}
	attrs.LFSPointer = ptr
	attrs.Exec = old.Attrs != nil && old.Attrs.Exec
	out.Attrs = &attrs
	return &out, nil
}
