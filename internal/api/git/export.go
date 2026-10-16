package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/tree"
)

// Exporter maps F1 objects to Git objects (doc/plan.md, "Object mapping",
// export). Results are memoized in git_map in cache.db, so each object is
// exported once; export is deterministic, so a rebuilt cache recomputes
// the same ids. Blob ids come from DigestRecords, so export never reads
// file content. An Exporter is not safe for concurrent use.
type Exporter struct {
	rp    *repo.Repo
	db    *cache.DB
	memo  map[memoKey]SHA
	blobs map[hash.Hash]SHA // blob ids of Files, for this exporter's lifetime
}

type memoKey struct {
	h    hash.Hash
	kind cache.GitKind
}

// NewExporter returns an exporter for a repository.
func NewExporter(rp *repo.Repo) *Exporter {
	return &Exporter{rp: rp, db: rp.Store().Cache(), memo: map[memoKey]SHA{}, blobs: map[hash.Hash]SHA{}}
}

func (x *Exporter) lookup(ctx context.Context, h hash.Hash, kind cache.GitKind) (SHA, bool, error) {
	if s, ok := x.memo[memoKey{h, kind}]; ok {
		return s, true, nil
	}
	s, ok, err := x.db.GitSHA(ctx, h, kind)
	if ok {
		x.memo[memoKey{h, kind}] = s
	}
	return s, ok, err
}

func (x *Exporter) record(ctx context.Context, h hash.Hash, kind cache.GitKind, s SHA) error {
	x.memo[memoKey{h, kind}] = s
	return x.db.PutGitSHA(ctx, h, kind, s)
}

// RefTarget returns the Git id and type of the object a ref points to (a
// commit or an annotated tag).
func (x *Exporter) RefTarget(ctx context.Context, h hash.Hash) (SHA, Type, error) {
	rec, err := x.rp.Store().Get(ctx, h)
	if err != nil {
		return SHA{}, 0, err
	}
	switch rec.Type {
	case object.TypeCommit:
		s, err := x.CommitSHA(ctx, h)
		return s, TypeCommit, err
	case object.TypeTag:
		s, err := x.TagSHA(ctx, h)
		return s, TypeTag, err
	}
	return SHA{}, 0, fmt.Errorf("git: ref points to a %s", rec.Type)
}

// CommitSHA returns the Git id of a commit, exporting its history as
// needed. The walk is iterative: histories are deep.
func (x *Exporter) CommitSHA(ctx context.Context, h hash.Hash) (SHA, error) {
	if s, ok, err := x.lookup(ctx, h, cache.GitCommit); ok || err != nil {
		return s, err
	}
	stack := []hash.Hash{h}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		if _, ok, err := x.lookup(ctx, top, cache.GitCommit); err != nil {
			return SHA{}, err
		} else if ok {
			stack = stack[:len(stack)-1]
			continue
		}
		c, err := x.rp.ReadCommit(ctx, top)
		if err != nil {
			return SHA{}, err
		}
		parents := make([]SHA, len(c.Parents))
		missing := false
		for i, p := range c.Parents {
			s, ok, err := x.lookup(ctx, p, cache.GitCommit)
			if err != nil {
				return SHA{}, err
			}
			if !ok {
				stack = append(stack, p)
				missing = true
			}
			parents[i] = s
		}
		if missing {
			continue
		}
		data, err := x.commitBytes(ctx, c, parents)
		if err != nil {
			return SHA{}, fmt.Errorf("git: export commit %s: %w", top.Short(), err)
		}
		if err := x.record(ctx, top, cache.GitCommit, Hash(TypeCommit, data)); err != nil {
			return SHA{}, err
		}
		stack = stack[:len(stack)-1]
	}
	s, _, err := x.lookup(ctx, h, cache.GitCommit)
	return s, err
}

func (x *Exporter) commitBytes(ctx context.Context, c *object.Commit, parents []SHA) ([]byte, error) {
	tree, err := x.TreeSHA(ctx, c.Tree, c.GitTreeRaw)
	if err != nil {
		return nil, err
	}
	if c.GitRawHint == nil {
		return EncodeCommit(c, tree, parents), nil
	}
	// The raw bytes are used only if they reference the same tree and
	// parents.
	pc, err := ParseCommit(c.GitRawHint)
	if err != nil {
		return nil, fmt.Errorf("git_raw_hint: %w", err)
	}
	if pc.Tree != tree || len(pc.Parents) != len(parents) {
		return nil, errors.New("git_raw_hint does not match the commit's tree or parents")
	}
	for i := range parents {
		if pc.Parents[i] != parents[i] {
			return nil, errors.New("git_raw_hint does not match the commit's parents")
		}
	}
	return c.GitRawHint, nil
}

// CommitBytes returns the Git encoding of a commit.
func (x *Exporter) CommitBytes(ctx context.Context, h hash.Hash) ([]byte, error) {
	c, err := x.rp.ReadCommit(ctx, h)
	if err != nil {
		return nil, err
	}
	parents := make([]SHA, len(c.Parents))
	for i, p := range c.Parents {
		if parents[i], err = x.CommitSHA(ctx, p); err != nil {
			return nil, err
		}
	}
	return x.commitBytes(ctx, c, parents)
}

func (x *Exporter) readTag(ctx context.Context, h hash.Hash) (*object.Tag, error) {
	rec, err := x.rp.Store().Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if rec.Type != object.TypeTag {
		return nil, fmt.Errorf("git: object %s is a %s, not a tag", h.Short(), rec.Type)
	}
	p, err := rec.Payload(nil)
	if err != nil {
		return nil, err
	}
	return object.DecodeAs[*object.Tag](p)
}

// TagSHA returns the Git id of an annotated tag.
func (x *Exporter) TagSHA(ctx context.Context, h hash.Hash) (SHA, error) {
	if s, ok, err := x.lookup(ctx, h, cache.GitTag); ok || err != nil {
		return s, err
	}
	data, err := x.TagBytes(ctx, h)
	if err != nil {
		return SHA{}, err
	}
	s := Hash(TypeTag, data)
	return s, x.record(ctx, h, cache.GitTag, s)
}

// TagBytes returns the Git encoding of a tag.
func (x *Exporter) TagBytes(ctx context.Context, h hash.Hash) ([]byte, error) {
	t, err := x.readTag(ctx, h)
	if err != nil {
		return nil, err
	}
	target, err := x.TagTarget(ctx, t)
	if err != nil {
		return nil, err
	}
	if t.GitRawHint == nil {
		return EncodeTag(t, target), nil
	}
	pt, err := ParseTag(t.GitRawHint)
	if err != nil || pt.Object != target {
		return nil, errors.New("git: tag git_raw_hint does not match the tag's target")
	}
	return t.GitRawHint, nil
}

// TagTarget returns the Git id of a tag's target.
func (x *Exporter) TagTarget(ctx context.Context, t *object.Tag) (SHA, error) {
	switch t.TargetType {
	case object.TagTargetCommit:
		return x.CommitSHA(ctx, t.Object)
	case object.TagTargetTag:
		return x.TagSHA(ctx, t.Object)
	case object.TagTargetTree:
		return x.TreeSHA(ctx, t.Object, nil)
	case object.TagTargetBlob:
		return x.fileSHA(ctx, t.Object)
	}
	return SHA{}, fmt.Errorf("git: unknown tag target type %q", t.TargetType)
}

// Entry is a Git tree entry with the F1 entry it exports.
type Entry struct {
	TreeEntry
	F1 object.Entry
}

// TreeEntries returns the Git entries of an F1 tree in Git order.
func (x *Exporter) TreeEntries(ctx context.Context, root hash.Hash) ([]Entry, error) {
	f1, err := tree.All(ctx, x.rp.Nodes(), root)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(f1))
	for i := range f1 {
		e := &f1[i]
		s, err := x.entrySHA(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("git: entry %q: %w", e.Name, err)
		}
		out[i] = Entry{TreeEntry: TreeEntry{Mode: ModeForKind(e), Name: e.Name, SHA: s}, F1: *e}
	}
	sortEntries(out)
	return out, nil
}

func sortEntries(es []Entry) {
	te := make([]TreeEntry, len(es))
	idx := map[string]int{}
	for i, e := range es {
		te[i] = e.TreeEntry
		idx[string(e.Name)] = i
	}
	SortTree(te)
	sorted := make([]Entry, len(es))
	for i, t := range te {
		sorted[i] = es[idx[string(t.Name)]]
	}
	copy(es, sorted)
}

func (x *Exporter) entrySHA(ctx context.Context, e *object.Entry) (SHA, error) {
	switch e.Kind {
	case object.KindDir:
		var raw *hash.Hash
		if e.Attrs != nil {
			raw = e.Attrs.GitRaw
		}
		return x.TreeSHA(ctx, e.Target, raw)
	case object.KindGitlink:
		var s SHA
		if e.Attrs == nil || len(e.Attrs.GitSHA1) != len(s) {
			return s, errors.New("gitlink without git_sha1")
		}
		copy(s[:], e.Attrs.GitSHA1)
		return s, nil
	case object.KindLFS:
		if e.Attrs == nil || len(e.Attrs.LFSPointer) == 0 {
			return SHA{}, errors.New("lfs entry without lfs_pointer")
		}
		return Hash(TypeBlob, e.Attrs.LFSPointer), nil
	}
	return x.fileSHA(ctx, e.Target)
}

// fileSHA returns the Git blob id of a File from its DigestRecord, computing
// the record if it is missing.
func (x *Exporter) fileSHA(ctx context.Context, file hash.Hash) (SHA, error) {
	if s, ok := x.blobs[file]; ok {
		return s, nil
	}
	d, ok, err := x.db.Digest(ctx, file)
	if err != nil {
		return SHA{}, err
	}
	if !ok {
		dg, err := x.rp.Digests(ctx, file)
		if err != nil {
			return SHA{}, err
		}
		d.SHA1Git = dg.SHA1Git
	}
	x.blobs[file] = SHA(d.SHA1Git)
	return SHA(d.SHA1Git), nil
}

// TreeSHA returns the Git id of an F1 tree; raw, if set, is the GitRaw
// object holding the exact Git tree bytes.
func (x *Exporter) TreeSHA(ctx context.Context, root hash.Hash, raw *hash.Hash) (SHA, error) {
	key, kind := root, cache.GitTree
	if raw != nil {
		key, kind = *raw, cache.GitRawTree
	}
	if s, ok, err := x.lookup(ctx, key, kind); ok || err != nil {
		return s, err
	}
	data, err := x.TreeBytes(ctx, root, raw)
	if err != nil {
		return SHA{}, err
	}
	s := Hash(TypeTree, data)
	if raw != nil {
		// Record the F1 tree too, so the Git id resolves to both.
		if err := x.record(ctx, root, cache.GitRawRoot, s); err != nil {
			return SHA{}, err
		}
	}
	return s, x.record(ctx, key, kind, s)
}

// TreeBytes returns the Git encoding of a tree: the canonical encoding of
// its entries, or the GitRaw bytes after checking that they describe the
// same entries.
func (x *Exporter) TreeBytes(ctx context.Context, root hash.Hash, raw *hash.Hash) ([]byte, error) {
	entries, err := x.TreeEntries(ctx, root)
	if err != nil {
		return nil, err
	}
	te := make([]TreeEntry, len(entries))
	for i, e := range entries {
		te[i] = e.TreeEntry
	}
	if raw == nil {
		return EncodeTree(te), nil
	}
	data, err := x.gitRaw(ctx, *raw)
	if err != nil {
		return nil, err
	}
	parsed, err := ParseTree(data)
	if err != nil {
		return nil, err
	}
	if !sameEntries(parsed, te) {
		return nil, fmt.Errorf("git: GitRaw %s does not describe tree %s", raw.Short(), root.Short())
	}
	return data, nil
}

func (x *Exporter) gitRaw(ctx context.Context, h hash.Hash) ([]byte, error) {
	rec, err := x.rp.Store().Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if rec.Type != object.TypeGitRaw {
		return nil, fmt.Errorf("git: object %s is a %s, not a GitRaw", h.Short(), rec.Type)
	}
	p, err := rec.Payload(nil)
	if err != nil {
		return nil, err
	}
	raw, err := object.DecodeAs[*object.GitRaw](p)
	if err != nil {
		return nil, err
	}
	return *raw, nil
}

// sameEntries compares raw tree entries with canonical ones by name, kind
// and id (modes may differ in spelling, order may differ).
func sameEntries(raw, canon []TreeEntry) bool {
	if len(raw) != len(canon) {
		return false
	}
	byName := map[string]TreeEntry{}
	for _, e := range canon {
		byName[string(e.Name)] = e
	}
	for _, r := range raw {
		c, ok := byName[string(r.Name)]
		if !ok || r.SHA != c.SHA {
			return false
		}
		rk, _, err1 := KindForMode(r.Mode)
		ck, _, err2 := KindForMode(c.Mode)
		if err1 != nil || err2 != nil || rk != ck {
			return false
		}
		delete(byName, string(r.Name))
	}
	return true
}

// Resolve returns the F1 object for a Git id: a commit, tree, tag or GitRaw
// recorded in git_map, or a File whose blob id is in digests.
func (x *Exporter) Resolve(ctx context.Context, s SHA) (Object, bool, error) {
	rows, err := x.db.GitObjects(ctx, s)
	if err != nil {
		return Object{}, false, err
	}
	var tree Object
	for _, r := range rows {
		switch r.Kind {
		case cache.GitCommit:
			return Object{Type: TypeCommit, SHA: s, F1: r.F1}, true, nil
		case cache.GitTag:
			return Object{Type: TypeTag, SHA: s, F1: r.F1}, true, nil
		case cache.GitTree:
			return Object{Type: TypeTree, SHA: s, F1: r.F1}, true, nil
		case cache.GitRawTree:
			h := r.F1
			tree.Type, tree.SHA, tree.Raw = TypeTree, s, &h
		case cache.GitRawRoot:
			tree.Type, tree.SHA, tree.F1 = TypeTree, s, r.F1
		}
	}
	if tree.Raw != nil {
		// A non-canonical tree: its bytes, and its F1 tree if recorded.
		return tree, true, nil
	}
	f, ok, err := x.db.FileBySHA1Git(ctx, s)
	if err != nil || !ok {
		return Object{}, false, err
	}
	e := object.Entry{Kind: object.KindFile, Target: f}
	return Object{Type: TypeBlob, SHA: s, Blob: &e}, true, nil
}

// Object is a Git object to send, with what produces its content.
type Object struct {
	Type Type
	SHA  SHA
	F1   hash.Hash     // commit, tag or tree root
	Raw  *hash.Hash    // GitRaw bytes of a tree
	Blob *object.Entry // blob: a file entry, or an LFS entry (its pointer)
	Path string        // tree or blob: its path in the commit (delta base selection)
}

// Content returns the content of a commit, tag or tree object.
func (x *Exporter) Content(ctx context.Context, o Object) ([]byte, error) {
	switch o.Type {
	case TypeCommit:
		return x.CommitBytes(ctx, o.F1)
	case TypeTag:
		return x.TagBytes(ctx, o.F1)
	case TypeTree:
		if o.F1.IsZero() && o.Raw != nil {
			return x.gitRaw(ctx, *o.Raw)
		}
		return x.TreeBytes(ctx, o.F1, o.Raw)
	}
	return nil, fmt.Errorf("git: no inline content for a %s", o.Type)
}

// pointer reports whether a blob is an LFS pointer and returns its bytes.
func pointer(e *object.Entry) ([]byte, bool) {
	if e.Kind == object.KindLFS && e.Attrs != nil {
		return e.Attrs.LFSPointer, true
	}
	return nil, false
}
