package gc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/store"
)

// DefaultGrace is how long new objects are kept even if unreachable
// (doc/plan.md, "GC algorithm").
const DefaultGrace = 24 * time.Hour

// Options tune a garbage collection.
type Options struct {
	// Grace protects objects written less than this long before the
	// collection started: uploads not yet committed, and anything they
	// reference. Zero means DefaultGrace; negative means none.
	Grace time.Duration
	// DryRun computes what would be deleted without deleting it or
	// affecting writers.
	DryRun bool
	// Now is the collection's start time (tests); zero means time.Now().
	Now time.Time
	// AfterCondemn, if set, runs between condemn and sweep (tests).
	AfterCondemn func()
}

// Result reports a garbage collection.
type Result struct {
	Skipped        string      `json:"skipped,omitempty"` // why the repository was skipped
	DryRun         bool        `json:"dry_run,omitempty"`
	Retained       int         `json:"retained_commits"`
	Marked         int         `json:"marked"`
	Condemned      int         `json:"condemned"`
	Deleted        []hash.Hash `json:"-"`                 // objects removed (or that would be), sorted
	List           []hash.Hash `json:"deleted,omitempty"` // Deleted, when a listing is asked for
	DeletedObjects int         `json:"deleted_objects"`   // len(Deleted)
	DeletedBytes   int64       `json:"deleted_bytes"`
	Kept           int         `json:"kept"` // condemned objects left in mostly live packs
	PacksDeleted   []string    `json:"packs_deleted,omitempty"`
	PacksRewritten []string    `json:"packs_rewritten,omitempty"`
	PacksCreated   []string    `json:"packs_created,omitempty"`
	MetadataRows   int64       `json:"metadata_rows"` // digests and git_map rows removed
	Duration       string      `json:"duration"`
}

// Repo is the repository a collection runs on (a *repo.Repo).
type Repo interface {
	Frozen() bool
	Store() *store.Store
	Refs() map[string]hash.Hash
	Meta() repodir.Meta
}

// Run collects garbage in one repository (doc/plan.md, "GC algorithm";
// ADR-0011). A frozen or held repository (freeze, archive, backup) is
// skipped, also if the hold starts before the sweep.
func Run(ctx context.Context, rp Repo, opts Options) (*Result, error) {
	start := time.Now()
	res := &Result{DryRun: opts.DryRun}
	defer func() { res.Duration = time.Since(start).Round(time.Millisecond).String() }()
	if rp.Frozen() {
		res.Skipped = "frozen"
		return res, nil
	}
	st := rp.Store()
	if err := st.BeginGC(); errors.Is(err, store.ErrHeld) {
		res.Skipped = "held (archive or backup in progress)"
		return res, nil
	} else if err != nil {
		return nil, err
	}
	defer st.EndGC()

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	grace := opts.Grace
	switch {
	case grace == 0:
		grace = DefaultGrace
	case grace < 0:
		grace = 0
	}
	cutoff := now.Add(-grace)
	m := &marker{st: st, marked: map[hash.Hash]struct{}{}}

	// Roots. The open pack is listed before the sealed packs, so that an
	// object sealed in between is seen in one of them; objects written
	// later are covered by the touched set.
	for _, e := range st.OpenEntries() {
		if err := m.mark(ctx, e.Hash, e.Type); err != nil {
			return nil, err
		}
	}
	packs, err := st.Cache().Packs(ctx)
	if err != nil {
		return nil, err
	}
	var old []string
	for _, p := range packs {
		t, ok := store.PackTime(p.Name)
		if ok && t.Before(cutoff) {
			old = append(old, p.Name)
			continue
		}
		// Written within the grace period: every object is a root.
		entries, err := st.Cache().ObjectsInPack(ctx, p.Name)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := m.mark(ctx, e.Hash, e.Type); err != nil {
				return nil, err
			}
		}
	}
	parts, err := st.Cache().PartFiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range parts {
		if err := m.mark(ctx, f, object.TypeFile); err != nil {
			return nil, err
		}
	}
	retained, err := retain(ctx, rp, now)
	if err != nil {
		return nil, err
	}
	res.Retained = len(retained)
	for _, c := range retained {
		if err := m.mark(ctx, c, object.TypeCommit); err != nil {
			return nil, err
		}
	}
	for _, h := range rp.Refs() {
		// Tags, and anything else a ref points to.
		if err := m.mark(ctx, h, 0); err != nil {
			return nil, err
		}
	}

	// Candidates: unmarked objects of old packs. A DigestRecord is live
	// exactly when its File is: marking a File marks its record (by the
	// hash of the record its digests row describes).
	candidates := map[hash.Hash]struct{}{}
	for _, name := range old {
		entries, err := st.Cache().ObjectsInPack(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if _, ok := m.marked[e.Hash]; !ok {
				candidates[e.Hash] = struct{}{}
			}
		}
	}

	condemned, err := st.Condemn(ctx, func(touched map[hash.Hash]struct{}) (map[hash.Hash]struct{}, error) {
		// Objects that writers found stored during the marking are live,
		// with everything they reference.
		for h := range touched {
			if err := m.mark(ctx, h, 0); err != nil {
				return nil, err
			}
		}
		out := map[hash.Hash]struct{}{}
		for h := range candidates {
			if _, ok := m.marked[h]; !ok {
				out[h] = struct{}{}
			}
		}
		return out, nil
	}, !opts.DryRun)
	if errors.Is(err, store.ErrHeld) {
		res.Skipped = "held (archive or backup in progress)"
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	res.Marked = len(m.marked)
	res.Condemned = len(condemned)
	if opts.AfterCondemn != nil {
		opts.AfterCondemn()
	}

	sw, err := st.Sweep(ctx, condemned, opts.DryRun)
	if err != nil {
		return nil, err
	}
	res.Deleted, res.DeletedObjects, res.DeletedBytes, res.Kept = sw.Deleted, len(sw.Deleted), sw.DeletedBytes, sw.Kept
	res.PacksDeleted, res.PacksRewritten, res.PacksCreated = sw.PacksDeleted, sw.PacksRewritten, sw.PacksCreated
	if opts.DryRun {
		return res, nil
	}
	if res.MetadataRows, err = st.Cache().DeleteOrphans(ctx); err != nil {
		return nil, err
	}
	if _, err := st.ConsolidateHeld(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// retain returns the commits that the namespace's retention policy keeps
// (doc/plan.md, "Retention"), for every ref. Missing commits end the walk:
// they are the boundary of truncated history.
func retain(ctx context.Context, rp Repo, now time.Time) ([]hash.Hash, error) {
	pol := rp.Meta().RetentionPolicy()
	seen := map[hash.Hash]bool{}
	var out []hash.Hash
	keep := func(h hash.Hash) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, head := range rp.Refs() {
		c, err := commitOf(ctx, rp, head)
		if err != nil {
			return nil, err
		}
		if c.IsZero() {
			continue
		}
		switch pol.Kind {
		case "all":
			// Every ancestor.
			stack := []hash.Hash{c}
			for len(stack) > 0 {
				h := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if seen[h] {
					continue
				}
				co, err := readCommit(ctx, rp, h)
				if err != nil {
					return nil, err
				}
				if co == nil {
					continue
				}
				keep(h)
				stack = append(stack, co.Parents...)
			}
		default:
			// First-parent history: the head always, then while the
			// policy allows.
			cutoff := now.Add(-time.Duration(pol.N) * 24 * time.Hour)
			for n, h := 0, c; !h.IsZero(); n++ {
				if pol.Kind == "none" && n >= 1 || pol.Kind == "last" && n >= pol.N {
					break
				}
				co, err := readCommit(ctx, rp, h)
				if err != nil {
					return nil, err
				}
				if co == nil {
					break
				}
				if pol.Kind == "days" && n > 0 && time.Unix(co.Committer.Time, 0).Before(cutoff) {
					break
				}
				keep(h)
				if len(co.Parents) == 0 {
					break
				}
				h = co.Parents[0]
			}
		}
	}
	return out, nil
}

// commitOf returns the commit a ref value points to, through annotated
// tags; zero if it points to something else.
func commitOf(ctx context.Context, rp Repo, h hash.Hash) (hash.Hash, error) {
	for range 100 {
		rec, err := rp.Store().Get(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			return hash.Zero, nil
		}
		if err != nil {
			return hash.Zero, err
		}
		switch rec.Type {
		case object.TypeCommit:
			return h, nil
		case object.TypeTag:
			payload, err := rec.Payload(nil)
			if err != nil {
				return hash.Zero, err
			}
			t, err := object.DecodeAs[*object.Tag](payload)
			if err != nil {
				return hash.Zero, err
			}
			h = t.Object
		default:
			return hash.Zero, nil
		}
	}
	return hash.Zero, fmt.Errorf("gc: tag chain too long at %s", h.Short())
}

// readCommit reads a commit, or returns nil if it is not stored.
func readCommit(ctx context.Context, rp Repo, h hash.Hash) (*object.Commit, error) {
	rec, err := rp.Store().Get(ctx, h)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rec.Type != object.TypeCommit {
		return nil, fmt.Errorf("gc: %s is a %s, not a commit", h.Short(), rec.Type)
	}
	payload, err := rec.Payload(nil)
	if err != nil {
		return nil, err
	}
	return object.DecodeAs[*object.Commit](payload)
}

// marker marks objects and everything they reference, except the parents
// of commits (retention decides which commits are kept). Already marked
// objects are not visited again, so shared subtrees are read once.
type marker struct {
	st     *store.Store
	marked map[hash.Hash]struct{}
}

type item struct {
	h hash.Hash
	t object.Type // 0: unknown, read the object
}

func (m *marker) mark(ctx context.Context, h hash.Hash, t object.Type) error {
	stack := []item{{h, t}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if it.h.IsZero() {
			continue
		}
		if _, ok := m.marked[it.h]; ok {
			continue
		}
		m.marked[it.h] = struct{}{}
		if it.t == object.TypeChunk {
			continue
		}
		rec, err := m.st.Get(ctx, it.h)
		if errors.Is(err, store.ErrNotFound) {
			continue // a history boundary, or reported by fsck
		}
		if err != nil {
			return err
		}
		if rec.Type == object.TypeChunk {
			continue
		}
		payload, err := rec.Payload(nil)
		if err != nil {
			return err
		}
		o, err := object.Decode(rec.Type, payload)
		if err != nil {
			return err
		}
		stack = append(stack, references(o)...)
		if _, ok := o.(*object.File); ok {
			// The File's DigestRecord, if indexed.
			if d, ok, err := m.st.Cache().Digest(ctx, it.h); err != nil {
				return err
			} else if ok {
				if dh, _, err := object.Hash(&d); err == nil {
					m.marked[dh] = struct{}{}
				}
			}
		}
	}
	return nil
}

// references returns what an object references, with types where they
// are known; commits' parents are left out.
func references(o object.Object) []item {
	var out []item
	switch o := o.(type) {
	case *object.File:
		t := object.TypeFileIndex
		if o.Level == 0 {
			t = object.TypeChunk
		}
		for _, r := range o.Refs {
			out = append(out, item{r.Hash, t})
		}
	case *object.FileIndex:
		t := object.TypeFileIndex
		if o.Level == 0 {
			t = object.TypeChunk
		}
		for _, r := range o.Refs {
			out = append(out, item{r.Hash, t})
		}
	case *object.TreeNode:
		for _, c := range o.Children {
			out = append(out, item{c.Node, object.TypeTree})
		}
		for _, e := range o.Entries {
			switch e.Kind {
			case object.KindDir:
				out = append(out, item{e.Target, object.TypeTree})
			case object.KindGitlink:
			default:
				out = append(out, item{e.Target, object.TypeFile})
			}
			if e.Attrs != nil && e.Attrs.GitRaw != nil {
				out = append(out, item{*e.Attrs.GitRaw, object.TypeGitRaw})
			}
		}
	case *object.Commit:
		out = append(out, item{o.Tree, object.TypeTree})
		if o.GitTreeRaw != nil {
			out = append(out, item{*o.GitTreeRaw, object.TypeGitRaw})
		}
	case *object.Tag:
		out = append(out, item{o.Object, 0})
	case *object.DigestRecord:
		// Not a reference for liveness: a record lives with its File.
	}
	return out
}
