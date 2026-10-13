package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// Concat stores a File that is the concatenation of files without copying
// content (S3 CompleteMultipartUpload). The new File has no DigestRecord yet:
// call DigestLater, or Digests when a digest is needed at once.
func (r *Repo) Concat(ctx context.Context, files []hash.Hash) (hash.Hash, uint64, error) {
	end, err := r.beginWrite()
	if err != nil {
		return hash.Zero, 0, err
	}
	defer end()
	return content.Concat(ctx, r.store, files)
}

// Durable seals the open pack, so that everything stored so far is durable
// (S3 UploadPart acknowledges a part only after this).
func (r *Repo) Durable(ctx context.Context) error {
	return r.store.Flush(ctx)
}

// Digests returns the digests of a File. If its DigestRecord is missing
// (a multipart File whose background job has not run), they are computed
// from the content and the record is stored.
func (r *Repo) Digests(ctx context.Context, file hash.Hash) (content.Digests, error) {
	if d, ok, err := r.store.Cache().Digest(ctx, file); err != nil || ok {
		return content.Digests{SHA1Git: d.SHA1Git, SHA256: d.SHA256, MD5: d.MD5}, err
	}
	end, err := r.beginWrite()
	if err != nil {
		return content.Digests{}, err
	}
	defer end()
	d, err := content.Digest(ctx, r.store, file)
	if err != nil {
		return d, err
	}
	return d, r.store.Flush(ctx)
}

// DigestLater computes and stores the DigestRecord of a File in the
// background (doc/plan.md, "Digests"). If the repository is closed or
// frozen first, the record is computed on demand by Digests instead.
func (r *Repo) DigestLater(file hash.Hash) {
	r.groupMu.Lock()
	defer r.groupMu.Unlock()
	if r.closed {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// One background digest at a time per repository: it reads the
		// whole content, which competes with requests.
		select {
		case r.digestSem <- struct{}{}:
		case <-r.stopCtx.Done():
			return
		}
		defer func() { <-r.digestSem }()
		if _, err := r.Digests(r.stopCtx, file); err != nil && !errors.Is(err, ErrFrozen) && r.stopCtx.Err() == nil {
			r.log.Warn("background digest failed", "file", file.Short(), "err", err)
		}
	}()
}

// Put stores an object (a commit, tag or GitRaw built by a protocol layer,
// such as the Git importer) and returns its hash. Every object it
// references must be stored (D9).
func (r *Repo) Put(ctx context.Context, o object.Object) (hash.Hash, error) {
	end, err := r.beginWrite()
	if err != nil {
		return hash.Zero, err
	}
	defer end()
	return r.putObject(ctx, o)
}

// PutTree stores a tree with the given entries (unsorted; names must be
// unique) and returns its root hash. Entries of dir kind point to stored
// trees.
func (r *Repo) PutTree(ctx context.Context, entries []object.Entry) (hash.Hash, error) {
	end, err := r.beginWrite()
	if err != nil {
		return hash.Zero, err
	}
	defer end()
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b object.Entry) int { return bytes.Compare(a.Name, b.Name) })
	for i := 1; i < len(sorted); i++ {
		if bytes.Equal(sorted[i-1].Name, sorted[i].Name) {
			return hash.Zero, fmt.Errorf("%w: duplicate name %q", ErrInvalidPath, sorted[i].Name)
		}
	}
	return object.BuildTree(sorted, func(n *object.TreeNode) (hash.Hash, error) { return r.putObject(ctx, n) })
}
