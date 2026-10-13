package repo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
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

	skipLocks bool
}

// Commit applies changes to the parent's tree, writes the commit, seals it
// and updates the ref with compare-and-swap. A conflict returns a
// *ConflictError (errors.Is ErrConflict).
func (r *Repo) Commit(ctx context.Context, req CommitRequest) (hash.Hash, error) {
	end, err := r.beginWrite()
	if err != nil {
		return hash.Zero, err
	}
	defer end()
	ref := policy.FullRefName(req.Ref)
	if err := policy.ValidRefName(ref); err != nil {
		return hash.Zero, err
	}
	parent := r.Ref(ref)
	if req.Parent != nil {
		parent = *req.Parent
	}
	base, err := r.commitTree(ctx, parent)
	if err != nil {
		return hash.Zero, err
	}
	root, err := r.applyChanges(ctx, base, req.Changes)
	if err != nil {
		return hash.Zero, err
	}
	c := &object.Commit{Tree: root, Message: []byte(req.Message)}
	if !parent.IsZero() {
		c.Parents = []hash.Hash{parent}
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
		return hash.Zero, err
	}
	opts := UpdateOptions{Principal: req.Principal, Reason: "commit", CheckPath: req.CheckPath, skipLocks: req.skipLocks}
	if err := r.updateRefs(ctx, []RefUpdate{{Ref: ref, Old: parent, New: h}}, opts); err != nil {
		return hash.Zero, err
	}
	return h, nil
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
		tc = append(tc, tree.Change{Name: []byte(name), Entry: c.entry})
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
