package repo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/store"
	"github.com/trukeio/f1/internal/tree"
)

// Op is a change operation.
type Op int

const (
	OpPut    Op = iota + 1 // set Path to Entry
	OpDelete               // remove Path; a missing path is a no-op
	OpRename               // move the entry at From to Path
)

// Change is one path change in a commit. For OpPut, Entry.Name is ignored.
type Change struct {
	Op    Op
	Path  string
	From  string // OpRename only
	Entry object.Entry
}

// Person identifies an author or committer.
type Person struct {
	Name  string
	Email string
}

// CommitRequest describes a commit.
type CommitRequest struct {
	Ref string // short or full ref name
	// Parent is the expected current value of Ref (CAS). The zero hash means
	// that Ref must not exist yet. nil means "the current value", for
	// callers such as the group committer that retry on conflict.
	Parent    *hash.Hash
	Changes   []Change
	Author    Person
	Committer Person // defaults to Author
	Message   string
	Time      time.Time // defaults to now
	Principal string    // recorded in refs.log; the committer for lock checks
	// CheckPath is called for every changed file (see UpdateOptions).
	CheckPath func(ref string, c PathChange) error
	// Rebase applies the changes on the current value of Ref if it has
	// moved from Parent but no path the changes name has changed
	// (ADR-0014). It needs a non-zero Parent.
	Rebase bool

	skipLocks bool
}

// Commit applies changes to the parent's tree, writes the commit, seals it
// and updates the ref with compare-and-swap. A conflict returns a
// *ConflictError (errors.Is ErrConflict).
func (r *Repo) Commit(ctx context.Context, req CommitRequest) (hash.Hash, error) {
	h, _, err := r.CommitOn(ctx, req)
	return h, err
}

// rebaseAttempts bounds the retries of a rebased commit that keeps losing
// the compare-and-swap to other writers.
const rebaseAttempts = 8

// CommitOn is Commit that also returns the commit the new one was built on:
// the requested parent, or with Rebase the ref's value it was rebased on.
func (r *Repo) CommitOn(ctx context.Context, req CommitRequest) (hash.Hash, hash.Hash, error) {
	end, err := r.beginWrite()
	if err != nil {
		return hash.Zero, hash.Zero, err
	}
	defer end()
	ref := policy.FullRefName(req.Ref)
	if err := policy.ValidRefName(ref); err != nil {
		return hash.Zero, hash.Zero, err
	}
	parent := r.Ref(ref)
	if req.Parent != nil {
		parent = *req.Parent
	}
	rebase := req.Rebase && !parent.IsZero()
	for attempt := 1; ; attempt++ {
		base := parent
		if rebase {
			if base, err = r.rebaseBase(ctx, ref, parent, req.Changes); err != nil {
				return hash.Zero, hash.Zero, err
			}
		}
		h, err := r.commitOn(ctx, ref, parent, base, req)
		var ce *ConflictError
		if rebase && errors.As(err, &ce) && ce.Expected == base && attempt < rebaseAttempts {
			continue // another write landed after the check
		}
		return h, base, err
	}
}

// commitOn builds the commit on base and moves ref from base to it.
// parent is the commit the caller expected, for conflict reporting.
func (r *Repo) commitOn(ctx context.Context, ref string, parent, base hash.Hash, req CommitRequest) (hash.Hash, error) {
	root, err := r.commitTree(ctx, base)
	if err != nil {
		return hash.Zero, r.staleParent(ref, base, err)
	}
	root, err = r.applyChanges(ctx, root, req.Changes)
	if err != nil {
		return hash.Zero, r.staleParent(ref, base, err)
	}
	c := &object.Commit{Tree: root, Message: []byte(req.Message)}
	if !base.IsZero() {
		c.Parents = []hash.Hash{base}
	}
	t := req.Time
	if t.IsZero() {
		t = time.Now()
	}
	committer := req.Committer
	if committer == (Person{}) {
		committer = req.Author
	}
	c.Author = gitPerson(req.Author, t)
	c.Committer = gitPerson(committer, t)
	h, err := r.putObject(ctx, c)
	if err != nil {
		return hash.Zero, r.staleParent(ref, base, err)
	}
	reason := "commit"
	if base != parent {
		reason = "commit (rebased)"
	}
	opts := UpdateOptions{Principal: req.Principal, Reason: reason, CheckPath: req.CheckPath, skipLocks: req.skipLocks}
	if err := r.updateRefs(ctx, []RefUpdate{{Ref: ref, Old: base, New: h}}, opts); err != nil {
		return hash.Zero, err
	}
	return h, nil
}

// rebaseBase returns the commit a rebased write is built on: the ref's
// current value, if every path the changes name (and each of its parent
// directories) is the same there as in parent. Otherwise it returns a
// *ConflictError listing the changed paths (ADR-0014).
func (r *Repo) rebaseBase(ctx context.Context, ref string, parent hash.Hash, changes []Change) (hash.Hash, error) {
	head := r.Ref(ref)
	if head == parent || head.IsZero() {
		return parent, nil
	}
	from, err := r.commitTree(ctx, parent)
	if err != nil {
		return hash.Zero, r.staleParent(ref, parent, err)
	}
	to, err := r.commitTree(ctx, head)
	if err != nil {
		return hash.Zero, err
	}
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Path)
		if c.Op == OpRename {
			paths = append(paths, c.From)
		}
	}
	var changed []string
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		same, err := r.samePath(ctx, from, to, p)
		if err != nil {
			return hash.Zero, err
		}
		if !same {
			changed = append(changed, p)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return hash.Zero, &ConflictError{Ref: ref, Expected: parent, Current: head, Paths: changed}
	}
	return head, nil
}

// samePath reports whether path has the same entry (kind, target and
// attrs, or absence) in trees a and b, and whether each of its parent
// directories is a directory in b or has the same kind as in a: a
// directory deleted or replaced by a file is a change.
func (r *Repo) samePath(ctx context.Context, a, b hash.Hash, path string) (bool, error) {
	if err := r.CheckPath(path); err != nil {
		return false, err
	}
	if r.meta.Layout != policy.LayoutFlat {
		for i := strings.IndexByte(path, '/'); i >= 0; {
			ea, err := r.entryAt(ctx, a, path[:i])
			if err != nil {
				return false, err
			}
			eb, err := r.entryAt(ctx, b, path[:i])
			if err != nil {
				return false, err
			}
			// A directory that appeared is no change: both writes may add
			// files to it.
			if (eb == nil || eb.Kind != object.KindDir) && ((ea == nil) != (eb == nil) || ea != nil && ea.Kind != eb.Kind) {
				return false, nil
			}
			j := strings.IndexByte(path[i+1:], '/')
			if j < 0 {
				break
			}
			i += 1 + j
		}
	}
	ea, err := r.entryAt(ctx, a, path)
	if err != nil {
		return false, err
	}
	eb, err := r.entryAt(ctx, b, path)
	if err != nil {
		return false, err
	}
	if ea == nil || eb == nil {
		return ea == nil && eb == nil, nil
	}
	return ea.Kind == eb.Kind && ea.Target == eb.Target && attrsEqual(ea.Attrs, eb.Attrs), nil
}

func attrsEqual(a, b *object.Attrs) bool {
	if a == nil {
		a = &object.Attrs{}
	}
	if b == nil {
		b = &object.Attrs{}
	}
	return reflect.DeepEqual(normAttrs(*a), normAttrs(*b))
}

// normAttrs makes empty maps and slices nil, so that decoding differences
// do not count as changes.
func normAttrs(a object.Attrs) object.Attrs {
	if len(a.Meta) == 0 {
		a.Meta = nil
	}
	if len(a.Checksums) == 0 {
		a.Checksums = nil
	}
	if len(a.GitSHA1) == 0 {
		a.GitSHA1 = nil
	}
	if len(a.LFSPointer) == 0 {
		a.LFSPointer = nil
	}
	return a
}

// staleParent turns an error from building on parent into a conflict when
// the ref has moved away from parent: garbage collection may have removed
// the objects of a commit that is no longer the head (retention other than
// "all", ADR-0011), and the compare-and-swap would refuse the commit anyway.
func (r *Repo) staleParent(ref string, parent hash.Hash, err error) error {
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrMissingReference) {
		return err
	}
	if cur := r.Ref(ref); cur != parent {
		return &ConflictError{Ref: ref, Expected: parent, Current: cur}
	}
	return err
}

// gitPerson converts to the commit form. Native and S3 commits use UTC
// (doc/plan.md, "Writes from other protocols into Git namespaces").
func gitPerson(p Person, t time.Time) object.Person {
	return object.Person{Name: []byte(p.Name), Email: []byte(p.Email), Time: t.Unix(), TZ: "+0000"}
}

// commitTree returns the root tree of a commit, or a stored empty tree for
// the zero hash.
func (r *Repo) commitTree(ctx context.Context, commit hash.Hash) (hash.Hash, error) {
	if commit.IsZero() {
		return tree.Empty(ctx, r.nodes)
	}
	c, err := r.ReadCommit(ctx, commit)
	if err != nil {
		return hash.Zero, err
	}
	return c.Tree, nil
}

// CheckPath validates a path for the namespace layout.
func (r *Repo) CheckPath(p string) error { return checkPath(r.meta.Layout, p) }

func checkPath(layout, p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if layout == policy.LayoutFlat {
		return nil
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return fmt.Errorf("%w: %q has an empty component", ErrInvalidPath, p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "." || c == ".." {
			return fmt.Errorf("%w: %q has a %q component", ErrInvalidPath, p, c)
		}
		if strings.IndexByte(c, 0) >= 0 {
			return fmt.Errorf("%w: %q contains NUL", ErrInvalidPath, p)
		}
	}
	return nil
}

// pathChange is a resolved change: Entry == nil deletes. removeDir allows
// removing a directory entry (renames of directories).
type pathChange struct {
	path      string
	entry     *object.Entry
	removeDir bool
}

// applyChanges applies changes to the tree rooted at base. Renames are
// resolved against base; for repeated paths the last change wins.
func (r *Repo) applyChanges(ctx context.Context, base hash.Hash, changes []Change) (hash.Hash, error) {
	var resolved []pathChange
	for _, c := range changes {
		if err := r.CheckPath(c.Path); err != nil {
			return hash.Zero, err
		}
		switch c.Op {
		case OpPut:
			if c.Entry.Kind == object.KindDir && r.meta.Layout == policy.LayoutFlat {
				return hash.Zero, fmt.Errorf("%w: directories are not allowed in a flat namespace", ErrInvalidPath)
			}
			e := c.Entry
			resolved = append(resolved, pathChange{path: c.Path, entry: &e})
		case OpDelete:
			resolved = append(resolved, pathChange{path: c.Path})
		case OpRename:
			if err := r.CheckPath(c.From); err != nil {
				return hash.Zero, err
			}
			e, err := r.statTree(ctx, base, c.From)
			if err != nil {
				return hash.Zero, fmt.Errorf("rename %s: %w", c.From, err)
			}
			resolved = append(resolved, pathChange{path: c.From, removeDir: true}, pathChange{path: c.Path, entry: &e})
		default:
			return hash.Zero, fmt.Errorf("repo: unknown change op %d", c.Op)
		}
	}
	// Last change per path wins.
	last := map[string]int{}
	for i, c := range resolved {
		last[c.path] = i
	}
	dedup := resolved[:0]
	for i, c := range resolved {
		if last[c.path] == i {
			dedup = append(dedup, c)
		}
	}
	if r.meta.Layout == policy.LayoutFlat {
		tc := make([]tree.Change, len(dedup))
		for i, c := range dedup {
			tc[i] = tree.Change{Name: []byte(c.path), Entry: c.entry}
		}
		return tree.Update(ctx, r.nodes, base, tc)
	}
	root, _, err := r.applyDir(ctx, base, "", dedup)
	return root, err
}

// applyDir applies changes, with paths relative to the directory at dirPath,
// to the directory tree dir. It reports whether the directory is empty
// afterwards (empty directories are pruned, as in Git).
func (r *Repo) applyDir(ctx context.Context, dir hash.Hash, dirPath string, changes []pathChange) (hash.Hash, bool, error) {
	direct := map[string]pathChange{}
	nested := map[string][]pathChange{}
	for _, c := range changes {
		first, rest, ok := strings.Cut(c.path, "/")
		if !ok {
			direct[first] = c
			continue
		}
		nested[first] = append(nested[first], pathChange{path: rest, entry: c.entry, removeDir: c.removeDir})
	}
	full := func(name string) string {
		if dirPath == "" {
			return name
		}
		return dirPath + "/" + name
	}

	var tc []tree.Change
	names := make([]string, 0, len(nested))
	for name := range nested {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		subs := nested[name]
		existing, exists, err := tree.Lookup(ctx, r.nodes, dir, []byte(name))
		if err != nil {
			return hash.Zero, false, err
		}
		d, replaced := direct[name]
		if replaced {
			if d.entry != nil {
				return hash.Zero, false, fmt.Errorf("%w: %s is written as a file and as a directory", ErrPathConflict, full(name))
			}
			// The entry is deleted and rebuilt as a directory.
			delete(direct, name)
			if exists && existing.Kind == object.KindDir && !d.removeDir {
				return hash.Zero, false, fmt.Errorf("%w: %s", ErrIsDir, full(name))
			}
			exists = false
		}
		var sub hash.Hash
		if exists {
			if existing.Kind != object.KindDir {
				if hasPut(subs) {
					return hash.Zero, false, fmt.Errorf("%w: %s exists as a file", ErrPathConflict, full(name))
				}
				continue // deletes below a file: nothing to delete
			}
			sub = existing.Target
		} else {
			if !hasPut(subs) {
				continue // deletes below a missing directory
			}
			if sub, err = tree.Empty(ctx, r.nodes); err != nil {
				return hash.Zero, false, err
			}
		}
		newSub, empty, err := r.applyDir(ctx, sub, full(name), subs)
		if err != nil {
			return hash.Zero, false, err
		}
		switch {
		case empty && (exists || replaced):
			tc = append(tc, tree.Change{Name: []byte(name)})
		case empty:
		case exists && newSub == sub:
		default:
			// A changed directory loses any exact Git tree bytes it carried.
			tc = append(tc, tree.Change{Name: []byte(name), Entry: &object.Entry{Kind: object.KindDir, Target: newSub}})
		}
	}
	for name, c := range direct {
		existing, exists, err := tree.Lookup(ctx, r.nodes, dir, []byte(name))
		if err != nil {
			return hash.Zero, false, err
		}
		if c.entry == nil {
			if !exists {
				continue
			}
			if existing.Kind == object.KindDir && !c.removeDir {
				return hash.Zero, false, fmt.Errorf("%w: %s", ErrIsDir, full(name))
			}
			tc = append(tc, tree.Change{Name: []byte(name)})
			continue
		}
		if exists && existing.Kind == object.KindDir && c.entry.Kind != object.KindDir {
			return hash.Zero, false, fmt.Errorf("%w: %s exists as a directory", ErrPathConflict, full(name))
		}
		e := c.entry
		if exists && existing.Kind == object.KindLFS && (e.Kind == object.KindFile || e.Kind == object.KindExec) {
			if e, err = r.lfsEntry(ctx, &existing, e); err != nil {
				return hash.Zero, false, err
			}
		}
		tc = append(tc, tree.Change{Name: []byte(name), Entry: e})
	}
	root, err := tree.Update(ctx, r.nodes, dir, tc)
	if err != nil {
		return hash.Zero, false, err
	}
	n, err := tree.Count(ctx, r.nodes, root)
	return root, n == 0, err
}

func hasPut(cs []pathChange) bool {
	for _, c := range cs {
		if c.entry != nil {
			return true
		}
	}
	return false
}
