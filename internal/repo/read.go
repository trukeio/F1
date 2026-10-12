package repo

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/tree"
)

// Stat returns the entry at path in a commit.
func (r *Repo) Stat(ctx context.Context, commit hash.Hash, path string) (object.Entry, error) {
	root, err := r.commitTree(ctx, commit)
	if err != nil {
		return object.Entry{}, err
	}
	return r.statTree(ctx, root, path)
}

func (r *Repo) statTree(ctx context.Context, root hash.Hash, path string) (object.Entry, error) {
	if err := r.CheckPath(path); err != nil {
		return object.Entry{}, err
	}
	if r.meta.Layout == policy.LayoutFlat {
		return r.lookup(ctx, root, path, path)
	}
	dir := root
	comps := strings.Split(path, "/")
	for i, c := range comps {
		e, err := r.lookup(ctx, dir, c, path)
		if err != nil {
			return object.Entry{}, err
		}
		if i == len(comps)-1 {
			e.Name = []byte(path)
			return e, nil
		}
		if e.Kind != object.KindDir {
			return object.Entry{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		dir = e.Target
	}
	panic("unreachable")
}

func (r *Repo) lookup(ctx context.Context, dir hash.Hash, name, path string) (object.Entry, error) {
	e, ok, err := tree.Lookup(ctx, r.nodes, dir, []byte(name))
	if err != nil {
		return object.Entry{}, err
	}
	if !ok {
		return object.Entry{}, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return e, nil
}

// Open returns a reader for the content of the file at path. lfs entries
// read as their real content; symlinks as their link text.
func (r *Repo) Open(ctx context.Context, commit hash.Hash, path string) (*content.Reader, object.Entry, error) {
	e, err := r.Stat(ctx, commit, path)
	if err != nil {
		return nil, e, err
	}
	switch e.Kind {
	case object.KindDir:
		return nil, e, fmt.Errorf("%w: %s", ErrIsDir, path)
	case object.KindGitlink:
		return nil, e, fmt.Errorf("repo: %s is a submodule", path)
	}
	rd, err := r.OpenFile(ctx, e.Target)
	return rd, e, err
}

// OpenFile returns a reader for a File object.
func (r *Repo) OpenFile(ctx context.Context, file hash.Hash) (*content.Reader, error) {
	return content.Open(ctx, r.store, file)
}

// ListOpts selects the entries List returns.
type ListOpts struct {
	Prefix    string // only paths starting with Prefix
	Delimiter string // "" or "/": collapse paths below Prefix at the next delimiter
	After     string // only paths (or collapsed prefixes) greater than After
	Limit     int    // maximum number of items; 0 is unlimited
}

// ListItem is a file, or a collapsed common prefix when IsPrefix is set.
type ListItem struct {
	Path     string
	Entry    object.Entry
	IsPrefix bool
}

var errStop = errors.New("stop")

// List calls fn for the files of a commit in byte order of their full
// paths, the order S3 listings need. In tree layout a directory "a" sorts
// as "a/", so "a.txt" comes before "a/b". Directories themselves are not
// items; with a Delimiter, everything below Prefix up to the next
// delimiter is collapsed into one prefix item.
func (r *Repo) List(ctx context.Context, commit hash.Hash, opts ListOpts, fn func(ListItem) error) error {
	if opts.Delimiter != "" && opts.Delimiter != "/" {
		return fmt.Errorf("repo: unsupported delimiter %q", opts.Delimiter)
	}
	root, err := r.commitTree(ctx, commit)
	if err != nil {
		return err
	}
	n := 0
	var lastPrefix string
	emit := func(it ListItem) error {
		if opts.Delimiter != "" {
			rest := it.Path[len(opts.Prefix):]
			if i := strings.Index(rest, opts.Delimiter); i >= 0 {
				p := opts.Prefix + rest[:i+len(opts.Delimiter)]
				if p == lastPrefix || p <= opts.After {
					return nil
				}
				lastPrefix = p
				it = ListItem{Path: p, IsPrefix: true}
			}
		}
		if it.Path <= opts.After {
			return nil
		}
		if err := fn(it); err != nil {
			return err
		}
		n++
		if opts.Limit > 0 && n >= opts.Limit {
			return errStop
		}
		return nil
	}
	if r.meta.Layout == policy.LayoutFlat {
		err = r.listFlat(ctx, root, opts, emit)
	} else {
		err = r.listTree(ctx, root, opts, emit)
	}
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

func (r *Repo) listFlat(ctx context.Context, root hash.Hash, opts ListOpts, emit func(ListItem) error) error {
	from := opts.Prefix
	if opts.After > from {
		from = opts.After
	}
	it, err := tree.Seek(ctx, r.nodes, root, []byte(from))
	if err != nil {
		return err
	}
	for {
		e, ok := it.Next()
		if !ok {
			return it.Err()
		}
		p := string(e.Name)
		if !strings.HasPrefix(p, opts.Prefix) {
			return nil
		}
		if opts.Delimiter != "" {
			if i := strings.Index(p[len(opts.Prefix):], opts.Delimiter); i >= 0 {
				// Emit the collapsed prefix once, then skip every key below it.
				cp := p[:len(opts.Prefix)+i+len(opts.Delimiter)]
				if err := emit(ListItem{Path: p, Entry: e}); err != nil {
					return err
				}
				if it, err = tree.Seek(ctx, r.nodes, root, successor(cp)); err != nil {
					return err
				}
				continue
			}
		}
		if err := emit(ListItem{Path: p, Entry: e}); err != nil {
			return err
		}
	}
}

// successor returns the smallest key greater than every key with prefix p.
func successor(p string) []byte {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return b[:i+1]
		}
	}
	return nil // unreachable for the delimiter prefixes used here
}

func (r *Repo) listTree(ctx context.Context, root hash.Hash, opts ListOpts, emit func(ListItem) error) error {
	// Start in the directory holding the prefix: "a/b/c" lists directory
	// "a/b" restricted to names starting with "c".
	dirPath, namePrefix := "", opts.Prefix
	if i := strings.LastIndex(opts.Prefix, "/"); i >= 0 {
		dirPath, namePrefix = opts.Prefix[:i], opts.Prefix[i+1:]
	}
	dir := root
	if dirPath != "" {
		e, err := r.statTree(ctx, root, dirPath)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Kind != object.KindDir {
			return nil
		}
		dir = e.Target
	}
	base := ""
	if dirPath != "" {
		base = dirPath + "/"
	}
	return r.walkDir(ctx, dir, base, namePrefix, opts, emit)
}

// walkDir emits the files below dir (whose full path prefix is base) in
// full-path order, restricted to names starting with namePrefix.
func (r *Repo) walkDir(ctx context.Context, dir hash.Hash, base, namePrefix string, opts ListOpts, emit func(ListItem) error) error {
	// Seek: entries whose full path sorts at or before After can be skipped,
	// except directories that After lies inside, and directories "d" whose
	// key "d/" sorts after After although "d" sorts before it (when After
	// continues d with a byte below '/'). Seeking to After's next component
	// cut before its first byte below '/' keeps all of those.
	from := namePrefix
	if strings.HasPrefix(opts.After, base) {
		comp := opts.After[len(base):]
		if i := strings.IndexByte(comp, '/'); i >= 0 {
			comp = comp[:i]
		}
		for i := 0; i < len(comp); i++ {
			if comp[i] < '/' {
				comp = comp[:i]
				break
			}
		}
		if comp > from {
			from = comp
		}
	}
	it, err := tree.Seek(ctx, r.nodes, dir, []byte(from))
	if err != nil {
		return err
	}
	// Directories wait until every entry sorting before "name/" is emitted.
	var pending []object.Entry
	flush := func(upTo string, all bool) error {
		for len(pending) > 0 {
			d := pending[0]
			key := base + string(d.Name) + "/"
			if !all && key >= upTo {
				return nil
			}
			pending = pending[1:]
			if opts.After >= key && !strings.HasPrefix(opts.After, key) {
				continue // the whole directory sorts before After
			}
			if opts.Delimiter != "" {
				// Every file below collapses into the prefix key ("name/"
				// is the first delimiter after Prefix): no need to descend.
				if err := emit(ListItem{Path: key}); err != nil {
					return err
				}
				continue
			}
			if err := r.walkDir(ctx, d.Target, key, "", opts, emit); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		e, ok := it.Next()
		if !ok {
			if err := it.Err(); err != nil {
				return err
			}
			return flush("", true)
		}
		name := string(e.Name)
		if !strings.HasPrefix(name, namePrefix) {
			return flush("", true)
		}
		full := base + name
		if err := flush(full, false); err != nil {
			return err
		}
		if e.Kind == object.KindDir {
			pending = append(pending, e)
			// Order by "name/", not by name: "a!/" < "a/" although "a" < "a!".
			sort.Slice(pending, func(i, j int) bool {
				return bytes.Compare(append(pending[i].Name[:len(pending[i].Name):len(pending[i].Name)], '/'),
					append(pending[j].Name[:len(pending[j].Name):len(pending[j].Name)], '/')) < 0
			})
			continue
		}
		e.Name = []byte(full)
		if err := emit(ListItem{Path: full, Entry: e}); err != nil {
			return err
		}
	}
}

// CommitInfo is a commit in a log.
type CommitInfo struct {
	Hash   hash.Hash
	Commit *object.Commit
}

// Log returns up to limit commits reachable from start, newest first by
// committer time. With a path, only commits that changed the path relative
// to every parent are returned. Missing parents (truncated history) end the
// walk on that branch.
func (r *Repo) Log(ctx context.Context, start hash.Hash, path string, limit int) ([]CommitInfo, error) {
	var out []CommitInfo
	q := &commitQueue{}
	seen := map[hash.Hash]bool{start: true}
	c, err := r.ReadCommit(ctx, start)
	if err != nil {
		return nil, err
	}
	heap.Push(q, CommitInfo{start, c})
	for q.Len() > 0 && (limit <= 0 || len(out) < limit) {
		ci := heap.Pop(q).(CommitInfo)
		var parents []*object.Commit
		for _, p := range ci.Commit.Parents {
			pc, err := r.ReadCommit(ctx, p)
			if errors.Is(err, ErrNotFound) {
				continue // history boundary
			}
			if err != nil {
				return nil, err
			}
			parents = append(parents, pc)
			if !seen[p] {
				seen[p] = true
				heap.Push(q, CommitInfo{p, pc})
			}
		}
		include := true
		if path != "" {
			cur, err := r.entryAt(ctx, ci.Commit.Tree, path)
			if err != nil {
				return nil, err
			}
			if len(parents) == 0 {
				include = cur != nil
			}
			for _, pc := range parents {
				prev, err := r.entryAt(ctx, pc.Tree, path)
				if err != nil {
					return nil, err
				}
				if sameEntry(cur, prev) {
					include = false
					break
				}
			}
		}
		if include {
			out = append(out, ci)
		}
	}
	return out, nil
}

func (r *Repo) entryAt(ctx context.Context, root hash.Hash, path string) (*object.Entry, error) {
	e, err := r.statTree(ctx, root, path)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func sameEntry(a, b *object.Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.Target == b.Target
}

type commitQueue []CommitInfo

func (q commitQueue) Len() int { return len(q) }
func (q commitQueue) Less(i, j int) bool {
	if q[i].Commit.Committer.Time != q[j].Commit.Committer.Time {
		return q[i].Commit.Committer.Time > q[j].Commit.Committer.Time
	}
	return hash.Compare(q[i].Hash, q[j].Hash) < 0
}
func (q commitQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *commitQueue) Push(x any)   { *q = append(*q, x.(CommitInfo)) }
func (q *commitQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// PathChange is a changed file between two commits.
type PathChange struct {
	Path string
	Old  *object.Entry // nil: added
	New  *object.Entry // nil: deleted
}

// Diff calls fn for every file that differs between commits a and b (either
// may be zero, meaning empty), restricted to paths starting with prefix.
// Unchanged subtrees are skipped by hash.
func (r *Repo) Diff(ctx context.Context, a, b hash.Hash, prefix string, fn func(PathChange) error) error {
	ta, err := r.commitTree(ctx, a)
	if err != nil {
		return err
	}
	tb, err := r.commitTree(ctx, b)
	if err != nil {
		return err
	}
	filtered := func(c PathChange) error {
		if strings.HasPrefix(c.Path, prefix) {
			return fn(c)
		}
		return nil
	}
	if r.meta.Layout == policy.LayoutFlat {
		return tree.Diff(ctx, r.nodes, ta, tb, func(d tree.DiffEntry) error {
			return filtered(PathChange{Path: string(d.Name), Old: d.Old, New: d.New})
		})
	}
	return r.diffDir(ctx, ta, tb, "", prefix, filtered)
}

func (r *Repo) diffDir(ctx context.Context, a, b hash.Hash, base, prefix string, fn func(PathChange) error) error {
	return tree.Diff(ctx, r.nodes, a, b, func(d tree.DiffEntry) error {
		full := base + string(d.Name)
		// Skip subtrees that cannot contain the prefix.
		if !strings.HasPrefix(full, prefix) && !strings.HasPrefix(prefix, full+"/") {
			return nil
		}
		oldDir := d.Old != nil && d.Old.Kind == object.KindDir
		newDir := d.New != nil && d.New.Kind == object.KindDir
		switch {
		case oldDir && newDir:
			return r.diffDir(ctx, d.Old.Target, d.New.Target, full+"/", prefix, fn)
		case oldDir || newDir:
			// A directory appeared or disappeared, possibly replacing a file.
			var olds, news hash.Hash
			empty, err := tree.Empty(ctx, r.nodes)
			if err != nil {
				return err
			}
			olds, news = empty, empty
			if oldDir {
				olds = d.Old.Target
			} else if d.Old != nil {
				if err := fn(PathChange{Path: full, Old: named(d.Old, full)}); err != nil {
					return err
				}
			}
			if newDir {
				news = d.New.Target
			} else if d.New != nil {
				if err := fn(PathChange{Path: full, New: named(d.New, full)}); err != nil {
					return err
				}
			}
			return r.diffDir(ctx, olds, news, full+"/", prefix, fn)
		default:
			return fn(PathChange{Path: full, Old: named(d.Old, full), New: named(d.New, full)})
		}
	})
}

func named(e *object.Entry, path string) *object.Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.Name = []byte(path)
	return &c
}

// MergeBase returns a best common ancestor of commits a and b, or the zero
// hash if they have none.
func (r *Repo) MergeBase(ctx context.Context, a, b hash.Hash) (hash.Hash, error) {
	ancestors := map[hash.Hash]bool{}
	stack := []hash.Hash{a}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[h] {
			continue
		}
		c, err := r.ReadCommit(ctx, h)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return hash.Zero, err
		}
		ancestors[h] = true
		stack = append(stack, c.Parents...)
	}
	// Walk b's history newest first; the first commit that is an ancestor of
	// a is a best common ancestor.
	q := &commitQueue{}
	seen := map[hash.Hash]bool{b: true}
	cb, err := r.ReadCommit(ctx, b)
	if err != nil {
		return hash.Zero, err
	}
	heap.Push(q, CommitInfo{b, cb})
	for q.Len() > 0 {
		ci := heap.Pop(q).(CommitInfo)
		if ancestors[ci.Hash] {
			return ci.Hash, nil
		}
		for _, p := range ci.Commit.Parents {
			if seen[p] {
				continue
			}
			seen[p] = true
			pc, err := r.ReadCommit(ctx, p)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return hash.Zero, err
			}
			heap.Push(q, CommitInfo{p, pc})
		}
	}
	return hash.Zero, nil
}
