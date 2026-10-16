package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// filter is a partial clone filter (doc/plan.md, "Protocol scope").
type filter struct {
	blobNone  bool
	blobLimit int64 // omit blobs of at least this size; -1: none
	treeNone  bool  // tree:0
}

func parseFilter(spec string) (filter, error) {
	f := filter{blobLimit: -1}
	switch {
	case spec == "blob:none":
		f.blobNone = true
	case strings.HasPrefix(spec, "blob:limit="):
		n, err := parseSize(strings.TrimPrefix(spec, "blob:limit="))
		if err != nil {
			return f, fmt.Errorf("invalid filter %q", spec)
		}
		f.blobLimit = n
	case spec == "tree:0":
		f.treeNone = true
	default:
		return f, fmt.Errorf("unsupported filter %q (supported: blob:none, blob:limit=<n>, tree:0)", spec)
	}
	return f, nil
}

// parseSize parses a size with an optional k, m or g suffix, as Git does.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'k', 'K':
			mult, s = 1<<10, s[:len(s)-1]
		case 'm', 'M':
			mult, s = 1<<20, s[:len(s)-1]
		case 'g', 'G':
			mult, s = 1<<30, s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid size")
	}
	return n * mult, nil
}

// fetchRequest is a negotiated fetch: what the client wants and has.
type fetchRequest struct {
	wants       []SHA
	haves       []SHA
	shallow     map[SHA]bool // commits that are shallow on the client
	depth       int          // deepen <n>; 0: no limit
	relative    bool         // deepen-relative: depth counts from the client's shallow commits
	deepenSince int64        // deepen-since <time>; 0: none
	filter      *filter
	includeTag  bool
	ofsDelta    bool // the client accepts ofs-delta objects
}

func (r *fetchRequest) deepening() bool { return r.depth > 0 || r.deepenSince > 0 }

// walkResult is what a fetch sends.
type walkResult struct {
	objects   []Object
	common    []SHA // haves the server knows
	shallow   []SHA // commits sent without their parents
	unshallow []SHA // client shallow commits whose parents are now sent
}

// walker enumerates the objects of a fetch.
type walker struct {
	x    *Exporter
	req  *fetchRequest
	res  walkResult
	seen map[SHA]bool // objects added or known to the client

	uninterestingCommits map[hash.Hash]bool
	uninterestingTrees   map[hash.Hash]bool // F1 tree roots the client has
}

// walk computes the objects to send: everything reachable from the wants
// that is not reachable from the common haves, within the depth limits and
// the filter. Explicitly wanted objects are always sent.
func walk(ctx context.Context, x *Exporter, req *fetchRequest, refTags []Object) (*walkResult, error) {
	w := &walker{x: x, req: req, seen: map[SHA]bool{}, uninterestingCommits: map[hash.Hash]bool{}, uninterestingTrees: map[hash.Hash]bool{}}
	if err := w.markHaves(ctx); err != nil {
		return nil, err
	}
	var commits []hash.Hash
	var wantedTrees, wantedBlobs []Object
	for _, s := range req.wants {
		o, ok, err := x.Resolve(ctx, s)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("not our ref %s", s)
		}
		// Peel tags: send the tags and what they point to.
		for o.Type == TypeTag {
			w.add(o)
			t, err := x.readTag(ctx, o.F1)
			if err != nil {
				return nil, err
			}
			target, err := x.TagTarget(ctx, t)
			if err != nil {
				return nil, err
			}
			next, ok, err := x.Resolve(ctx, target)
			if err != nil || !ok {
				return nil, fmt.Errorf("tag target %s not found: %v", target, err)
			}
			o = next
		}
		switch o.Type {
		case TypeCommit:
			commits = append(commits, o.F1)
		case TypeTree:
			wantedTrees = append(wantedTrees, o)
		case TypeBlob:
			wantedBlobs = append(wantedBlobs, o)
		}
	}
	sent, err := w.walkCommits(ctx, commits)
	if err != nil {
		return nil, err
	}
	for _, c := range sent {
		co, err := x.rp.ReadCommit(ctx, c)
		if err != nil {
			return nil, err
		}
		if req.filter != nil && req.filter.treeNone {
			continue
		}
		if err := w.walkTree(ctx, co.Tree, co.GitTreeRaw, ""); err != nil {
			return nil, err
		}
	}
	for _, t := range wantedTrees {
		if t.F1.IsZero() {
			w.add(t) // raw tree bytes only
			continue
		}
		if err := w.walkTree(ctx, t.F1, t.Raw, ""); err != nil {
			return nil, err
		}
	}
	for _, b := range wantedBlobs {
		w.add(b)
	}
	if req.includeTag {
		sentCommits := map[hash.Hash]bool{}
		for _, c := range sent {
			sentCommits[c] = true
		}
		for _, t := range refTags {
			tag, err := x.readTag(ctx, t.F1)
			if err == nil && tag.TargetType == object.TagTargetCommit && sentCommits[tag.Object] {
				w.add(t)
			}
		}
	}
	return &w.res, nil
}

func (w *walker) add(o Object) {
	if w.seen[o.SHA] {
		return
	}
	w.seen[o.SHA] = true
	w.res.objects = append(w.res.objects, o)
}

// markHaves records the common haves and what the client has: the commits
// reachable from them (not past the client's shallow commits) and the
// trees of the have commits themselves.
func (w *walker) markHaves(ctx context.Context) error {
	var queue []hash.Hash
	for _, s := range w.req.haves {
		o, ok, err := w.x.Resolve(ctx, s)
		if err != nil {
			return err
		}
		if !ok || o.Type != TypeCommit {
			continue
		}
		w.res.common = append(w.res.common, s)
		w.seen[s] = true
		queue = append(queue, o.F1)
		co, err := w.x.rp.ReadCommit(ctx, o.F1)
		if err != nil {
			return err
		}
		if err := w.markTree(ctx, co.Tree, co.GitTreeRaw); err != nil {
			return err
		}
	}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if w.uninterestingCommits[c] {
			continue
		}
		w.uninterestingCommits[c] = true
		s, err := w.x.CommitSHA(ctx, c)
		if err != nil {
			return err
		}
		w.seen[s] = true
		if w.req.shallow[s] {
			continue // the client does not have the parents
		}
		co, err := w.x.rp.ReadCommit(ctx, c)
		if err != nil {
			return err
		}
		queue = append(queue, co.Parents...)
	}
	return nil
}

// markTree marks a tree the client has and everything in it.
func (w *walker) markTree(ctx context.Context, root hash.Hash, raw *hash.Hash) error {
	if w.uninterestingTrees[root] {
		return nil
	}
	w.uninterestingTrees[root] = true
	s, err := w.x.TreeSHA(ctx, root, raw)
	if err != nil {
		return err
	}
	w.seen[s] = true
	entries, err := w.x.TreeEntries(ctx, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.F1.Kind {
		case object.KindDir:
			if err := w.markTree(ctx, e.F1.Target, gitRawOf(&e.F1)); err != nil {
				return err
			}
		case object.KindGitlink:
		default:
			w.seen[e.SHA] = true
		}
	}
	return nil
}

func gitRawOf(e *object.Entry) *hash.Hash {
	if e.Attrs != nil {
		return e.Attrs.GitRaw
	}
	return nil
}

type depthItem struct {
	c     hash.Hash
	depth int
}

// walkCommits returns the commits to send, oldest last, and records
// shallow boundaries.
func (w *walker) walkCommits(ctx context.Context, wants []hash.Hash) ([]hash.Hash, error) {
	var sent []hash.Hash
	visited := map[hash.Hash]bool{}
	queue := make([]depthItem, 0, len(wants))
	// Depth 0: outside the depth limit (deepen-relative counts from the
	// client's shallow commits, whose parents get depth 1).
	start := 1
	if w.req.relative {
		start = 0
	}
	for _, c := range wants {
		queue = append(queue, depthItem{c, start})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if visited[it.c] {
			continue
		}
		visited[it.c] = true
		uninteresting := w.uninterestingCommits[it.c]
		if uninteresting && !w.req.deepening() {
			continue
		}
		s, err := w.x.CommitSHA(ctx, it.c)
		if err != nil {
			return nil, err
		}
		co, err := w.x.rp.ReadCommit(ctx, it.c)
		if err != nil {
			return nil, err
		}
		if !uninteresting {
			w.add(Object{Type: TypeCommit, SHA: s, F1: it.c})
			sent = append(sent, it.c)
		}
		// Does the walk continue to the parents?
		follow := len(co.Parents) > 0
		boundary := false
		limited := it.depth > 0
		switch {
		case w.req.depth > 0 && limited && it.depth >= w.req.depth:
			follow, boundary = false, follow
		case w.req.deepenSince > 0 && co.Committer.Time < w.req.deepenSince:
			follow, boundary = false, follow
		case !w.req.deepening() && w.req.shallow[s]:
			follow = false // the client's shallow boundary stays
		}
		if boundary {
			if !w.req.shallow[s] {
				w.res.shallow = append(w.res.shallow, s)
			}
		} else if follow && w.req.shallow[s] {
			w.res.unshallow = append(w.res.unshallow, s)
		}
		if follow {
			next := it.depth + 1
			if !limited && !(w.req.relative && w.req.shallow[s]) {
				next = 0
			}
			for _, p := range co.Parents {
				queue = append(queue, depthItem{p, next})
			}
		}
	}
	return sent, nil
}

// walkTree adds a tree and everything in it the client lacks, following the
// filter.
func (w *walker) walkTree(ctx context.Context, root hash.Hash, raw *hash.Hash, path string) error {
	if w.uninterestingTrees[root] && raw == nil {
		return nil
	}
	s, err := w.x.TreeSHA(ctx, root, raw)
	if err != nil {
		return err
	}
	if w.seen[s] {
		return nil
	}
	w.add(Object{Type: TypeTree, SHA: s, F1: root, Raw: raw, Path: path})
	entries, err := w.x.TreeEntries(ctx, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.F1.Kind {
		case object.KindDir:
			if err := w.walkTree(ctx, e.F1.Target, gitRawOf(&e.F1), path+"/"+string(e.F1.Name)); err != nil {
				return err
			}
		case object.KindGitlink:
		default:
			if w.seen[e.SHA] {
				continue
			}
			if f := w.req.filter; f != nil {
				if f.blobNone {
					continue
				}
				if f.blobLimit >= 0 {
					size, err := w.blobSize(ctx, &e.F1)
					if err != nil {
						return err
					}
					if size >= f.blobLimit {
						continue
					}
				}
			}
			e := e
			w.add(Object{Type: TypeBlob, SHA: e.SHA, Blob: &e.F1, Path: path + "/" + string(e.F1.Name)})
		}
	}
	return nil
}

func (w *walker) blobSize(ctx context.Context, e *object.Entry) (int64, error) {
	if ptr, ok := pointer(e); ok {
		return int64(len(ptr)), nil
	}
	n, err := w.x.rp.FileSize(ctx, e.Target)
	return int64(n), err
}
