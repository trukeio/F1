package git

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
)

// Importer converts Git objects into F1 objects (doc/plan.md, "Object
// mapping", import). Objects must be added in dependency order (D9):
// blobs, then trees bottom-up, then commits parents first, then tags.
// Every converted commit, tag and tree is exported again and must give the
// same Git id; where the canonical encoding differs, the exact Git bytes
// are kept (GitRaw for trees, git_raw_hint for commits and tags), so Git
// ids never change.
type Importer struct {
	rp *repo.Repo
	x  *Exporter
	// The objects converted or looked up so far: a push of a long history
	// has hundreds of thousands, so an entry is only a type and a hash, and
	// the rare extras are kept apart.
	objs     map[SHA]importedRef
	raws     map[SHA]hash.Hash // trees with a GitRaw
	pointers map[SHA][]byte    // LFS pointer blobs
}

type importedRef struct {
	f1  hash.Hash
	typ uint8
}

// Imported is a Git object converted to F1.
type Imported struct {
	Type    Type
	F1      hash.Hash  // File, tree root, commit or tag
	Raw     *hash.Hash // trees only: GitRaw with the exact bytes, if not canonical
	Pointer []byte     // blobs only: the bytes, if the blob is an LFS pointer
}

// NewImporter returns an importer into rp.
func NewImporter(rp *repo.Repo) *Importer {
	return &Importer{rp: rp, x: NewExporter(rp), objs: map[SHA]importedRef{}, raws: map[SHA]hash.Hash{}, pointers: map[SHA][]byte{}}
}

// Lookup returns an object imported by this importer.
func (im *Importer) Lookup(s SHA) (Imported, bool) {
	r, ok := im.objs[s]
	if !ok {
		return Imported{}, false
	}
	o := Imported{Type: Type(r.typ), F1: r.f1, Pointer: im.pointers[s]}
	if raw, ok := im.raws[s]; ok {
		o.Raw = &raw
	}
	return o, true
}

func (im *Importer) record(s SHA, o Imported) {
	im.objs[s] = importedRef{f1: o.F1, typ: uint8(o.Type)}
	if o.Raw != nil {
		im.raws[s] = *o.Raw
	}
	if o.Pointer != nil {
		im.pointers[s] = o.Pointer
	}
}

// Known returns an object imported by this importer or already in the
// repository (pushes reference existing objects, and thin packs delta
// against them).
func (im *Importer) Known(ctx context.Context, s SHA) (Imported, bool, error) {
	if o, ok := im.Lookup(s); ok {
		return o, true, nil
	}
	o, ok, err := im.x.Resolve(ctx, s)
	if err != nil || !ok {
		return Imported{}, false, err
	}
	imp := Imported{Type: o.Type, F1: o.F1, Raw: o.Raw}
	switch o.Type {
	case TypeTree:
		if imp.F1.IsZero() {
			return Imported{}, false, nil // raw bytes without a known F1 tree
		}
	case TypeBlob:
		imp.F1 = o.Blob.Target
		size, err := im.rp.FileSize(ctx, imp.F1)
		if err != nil {
			return Imported{}, false, err
		}
		if size <= maxPointerSize {
			rd, err := im.rp.OpenFile(ctx, imp.F1)
			if err != nil {
				return Imported{}, false, err
			}
			data, err := io.ReadAll(rd)
			if err != nil {
				return Imported{}, false, err
			}
			if _, ok := ParsePointer(data); ok {
				imp.Pointer = data
			}
		}
	}
	im.record(s, imp)
	return imp, true, nil
}

// Blob imports blob content of the given size; its Git id must be s.
func (im *Importer) Blob(ctx context.Context, s SHA, r io.Reader, size int64) error {
	got, err := im.BlobStream(ctx, r, size)
	if err != nil {
		return err
	}
	if got != s {
		return fmt.Errorf("git: blob %s has id %s", s, got)
	}
	return nil
}

// BlobStream imports blob content of the given size and returns its Git
// id, computed with collision detection.
func (im *Importer) BlobStream(ctx context.Context, r io.Reader, size int64) (SHA, error) {
	var ptr []byte
	if size <= maxPointerSize {
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return SHA{}, err
		}
		if _, ok := ParsePointer(data); ok {
			ptr = data
		}
		r = bytes.NewReader(data)
	}
	h := newObjectHasher(TypeBlob, size)
	res, err := im.rp.Ingest(ctx, io.TeeReader(r, h), size)
	if err != nil {
		return SHA{}, err
	}
	s, err := h.sum()
	if err != nil {
		return SHA{}, err
	}
	if SHA(res.Digests.SHA1Git) != s {
		return SHA{}, fmt.Errorf("git: blob id mismatch (%s, %s)", s, SHA(res.Digests.SHA1Git))
	}
	im.record(s, Imported{Type: TypeBlob, F1: res.File, Pointer: ptr})
	return s, nil
}

// Tree imports a tree object with id s. Its blobs and subtrees must be
// imported.
func (im *Importer) Tree(ctx context.Context, s SHA, data []byte) error {
	if Hash(TypeTree, data) != s {
		return fmt.Errorf("git: tree %s does not hash to its id", s)
	}
	gitEntries, err := ParseTree(data)
	if err != nil {
		return err
	}
	entries := make([]object.Entry, 0, len(gitEntries))
	for _, ge := range gitEntries {
		kind, _, err := KindForMode(ge.Mode)
		if err != nil {
			return fmt.Errorf("git: tree %s entry %q: %w", s, ge.Name, err)
		}
		e := object.Entry{Name: ge.Name, Kind: kind}
		switch kind {
		case object.KindGitlink:
			e.Attrs = &object.Attrs{GitSHA1: append([]byte{}, ge.SHA[:]...)}
		case object.KindDir:
			child, ok, err := im.Known(ctx, ge.SHA)
			if err != nil {
				return err
			}
			if !ok || child.Type != TypeTree {
				return fmt.Errorf("git: tree %s: missing subtree %s", s, ge.SHA)
			}
			e.Target = child.F1
			if child.Raw != nil {
				e.Attrs = &object.Attrs{GitRaw: child.Raw}
			}
		default:
			blob, ok, err := im.Known(ctx, ge.SHA)
			if err != nil {
				return err
			}
			if !ok || blob.Type != TypeBlob {
				return fmt.Errorf("git: tree %s: missing blob %s", s, ge.SHA)
			}
			e.Target = blob.F1
			if blob.Pointer != nil && kind != object.KindSymlink {
				if e, err = im.lfsEntry(ctx, ge, kind, blob.Pointer); err != nil {
					return err
				}
			}
		}
		entries = append(entries, e)
	}
	root, err := im.rp.PutTree(ctx, entries)
	if err != nil {
		return fmt.Errorf("git: tree %s: %w", s, err)
	}
	imp := Imported{Type: TypeTree, F1: root}
	canon, err := im.x.TreeSHA(ctx, root, nil)
	if err != nil {
		return err
	}
	if canon != s {
		// Non-canonical (modes such as 100664, unusual order): keep the
		// exact bytes.
		g := object.GitRaw(data)
		raw, err := im.rp.Put(ctx, &g)
		if err != nil {
			return err
		}
		if got, err := im.x.TreeSHA(ctx, root, &raw); err != nil || got != s {
			return fmt.Errorf("git: tree %s does not round-trip: %v", s, err)
		}
		imp.Raw = &raw
	}
	im.record(s, imp)
	return nil
}

// Commit imports a commit object with id s. Its tree and parents must be
// imported.
func (im *Importer) Commit(ctx context.Context, s SHA, data []byte) error {
	if Hash(TypeCommit, data) != s {
		return fmt.Errorf("git: commit %s does not hash to its id", s)
	}
	pc, err := ParseCommit(data)
	if err != nil {
		return fmt.Errorf("git: commit %s: %w", s, err)
	}
	c := pc.Commit
	t, ok, err := im.Known(ctx, pc.Tree)
	if err != nil {
		return err
	}
	if !ok || t.Type != TypeTree {
		return fmt.Errorf("git: commit %s: missing tree %s", s, pc.Tree)
	}
	c.Tree, c.GitTreeRaw = t.F1, t.Raw
	for _, p := range pc.Parents {
		po, ok, err := im.Known(ctx, p)
		if err != nil {
			return err
		}
		if !ok || po.Type != TypeCommit {
			return fmt.Errorf("git: commit %s: missing parent %s", s, p)
		}
		c.Parents = append(c.Parents, po.F1)
	}
	if dedup := uniqueHashes(c.Parents); len(dedup) != len(c.Parents) {
		// Git allows a parent to be listed twice; F1 commits do not
		// (doc/format.md). The F1 commit lists each parent once, and the
		// exact Git bytes are kept.
		c.Parents = dedup
		c.GitRawHint = data
	}
	if err := c.Validate(); err != nil {
		// Other unusual commits are kept as raw bytes.
		c.GitRawHint = data
	}
	if c.GitRawHint == nil {
		got, err := im.x.commitBytes(ctx, &c, pc.Parents)
		if err != nil {
			return err
		}
		if Hash(TypeCommit, got) != s {
			c.GitRawHint = data
		}
	}
	h, err := im.rp.Put(ctx, &c)
	if err != nil {
		return fmt.Errorf("git: commit %s: %w", s, err)
	}
	if got, err := im.x.CommitSHA(ctx, h); err != nil || got != s {
		return fmt.Errorf("git: commit %s does not round-trip (got %s): %v", s, got, err)
	}
	im.record(s, Imported{Type: TypeCommit, F1: h})
	return nil
}

// Tag imports an annotated tag with id s. Its target must be imported.
func (im *Importer) Tag(ctx context.Context, s SHA, data []byte) error {
	if Hash(TypeTag, data) != s {
		return fmt.Errorf("git: tag %s does not hash to its id", s)
	}
	pt, err := ParseTag(data)
	if err != nil {
		return fmt.Errorf("git: tag %s: %w", s, err)
	}
	t := pt.Tag
	target, ok, err := im.Known(ctx, pt.Object)
	if err != nil {
		return err
	}
	if !ok || target.Type.String() != t.TargetType {
		return fmt.Errorf("git: tag %s: missing target %s (%s)", s, pt.Object, t.TargetType)
	}
	t.Object = target.F1
	if t.TargetType == object.TagTargetTree && target.Raw != nil {
		return fmt.Errorf("git: tag %s points to a non-canonical tree, which F1 cannot tag", s)
	}
	if Hash(TypeTag, EncodeTag(&t, pt.Object)) != s {
		t.GitRawHint = data
	}
	h, err := im.rp.Put(ctx, &t)
	if err != nil {
		return fmt.Errorf("git: tag %s: %w", s, err)
	}
	if got, err := im.x.TagSHA(ctx, h); err != nil || got != s {
		return fmt.Errorf("git: tag %s does not round-trip (got %s): %v", s, got, err)
	}
	im.record(s, Imported{Type: TypeTag, F1: h})
	return nil
}

// uniqueHashes returns hs without repeats, in first-occurrence order.
func uniqueHashes(hs []hash.Hash) []hash.Hash {
	seen := map[hash.Hash]bool{}
	var out []hash.Hash
	for _, h := range hs {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}
