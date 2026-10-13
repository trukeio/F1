package git

import (
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
	rp   *repo.Repo
	x    *Exporter
	objs map[SHA]Imported
}

// Imported is a Git object converted to F1.
type Imported struct {
	Type Type
	F1   hash.Hash  // File, tree root, commit or tag
	Raw  *hash.Hash // trees only: GitRaw with the exact bytes, if not canonical
}

// NewImporter returns an importer into rp.
func NewImporter(rp *repo.Repo) *Importer {
	return &Importer{rp: rp, x: NewExporter(rp), objs: map[SHA]Imported{}}
}

// Lookup returns an imported object.
func (im *Importer) Lookup(s SHA) (Imported, bool) {
	o, ok := im.objs[s]
	return o, ok
}

// Blob imports blob content of the given size; its Git id must be s.
func (im *Importer) Blob(ctx context.Context, s SHA, r io.Reader, size int64) error {
	res, err := im.rp.Ingest(ctx, r, size)
	if err != nil {
		return err
	}
	if SHA(res.Digests.SHA1Git) != s {
		return fmt.Errorf("git: blob %s has id %s", s, SHA(res.Digests.SHA1Git))
	}
	im.objs[s] = Imported{Type: TypeBlob, F1: res.File}
	return nil
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
			child, ok := im.objs[ge.SHA]
			if !ok || child.Type != TypeTree {
				return fmt.Errorf("git: tree %s: subtree %s not imported", s, ge.SHA)
			}
			e.Target = child.F1
			if child.Raw != nil {
				e.Attrs = &object.Attrs{GitRaw: child.Raw}
			}
		default:
			blob, ok := im.objs[ge.SHA]
			if !ok || blob.Type != TypeBlob {
				return fmt.Errorf("git: tree %s: blob %s not imported", s, ge.SHA)
			}
			e.Target = blob.F1
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
	im.objs[s] = imp
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
	t, ok := im.objs[pc.Tree]
	if !ok || t.Type != TypeTree {
		return fmt.Errorf("git: commit %s: tree %s not imported", s, pc.Tree)
	}
	c.Tree, c.GitTreeRaw = t.F1, t.Raw
	for _, p := range pc.Parents {
		po, ok := im.objs[p]
		if !ok || po.Type != TypeCommit {
			return fmt.Errorf("git: commit %s: parent %s not imported", s, p)
		}
		c.Parents = append(c.Parents, po.F1)
	}
	if err := c.Validate(); err != nil {
		// Unusual commits (duplicate parents) are kept as raw bytes.
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
	im.objs[s] = Imported{Type: TypeCommit, F1: h}
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
	target, ok := im.objs[pt.Object]
	if !ok || target.Type.String() != t.TargetType {
		return fmt.Errorf("git: tag %s: target %s (%s) not imported", s, pt.Object, t.TargetType)
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
	im.objs[s] = Imported{Type: TypeTag, F1: h}
	return nil
}
