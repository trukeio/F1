// Package repo is the F1 core: one Repo per repository directory, with the
// read path, commits, CAS ref updates and group commit. Protocol layers
// (native API, S3, Git) are built on it.
package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/metrics"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/store"
	"github.com/trukeio/f1/internal/tree"
)

// Errors returned by Repo operations.
var (
	ErrNotFound     = errors.New("repo: not found")
	ErrConflict     = errors.New("repo: ref changed concurrently") // CAS failure
	ErrPathConflict = errors.New("repo: path conflicts with the hierarchy")
	ErrIsDir        = errors.New("repo: path is a directory")
	ErrInvalidPath  = errors.New("repo: invalid path")
	ErrClosed       = errors.New("repo: closed")
)

// Options tune a Repo. Zero values use defaults.
type Options struct {
	Store       store.Options
	Logger      *slog.Logger
	NodeCache   int           // decoded tree nodes kept in memory (8192)
	GroupWindow time.Duration // group commit collection window (20 ms)
	GroupMax    int           // group commit batch size (1000)
}

// Repo is an open repository. It is safe for concurrent use.
type Repo struct {
	dir   *repodir.Dir
	meta  repodir.Meta
	store *store.Store
	nodes *tree.Nodes
	log   *slog.Logger
	opts  Options

	refMu sync.Mutex
	refs  map[string]hash.Hash

	graphMu sync.Mutex // guards graph (ADR-0015)
	graph   graphState

	gateMu   sync.Mutex
	gateCond *sync.Cond
	gate     gate

	locksMu sync.Mutex // after refMu when both are held
	locks   []Lock

	groupMu sync.Mutex
	groups  map[string]*committer

	digestSem chan struct{} // background digest jobs (one at a time)
	stop      context.CancelFunc
	stopCtx   context.Context
	wg        sync.WaitGroup
	closed    bool
}

// OpenReport describes the recovery steps Open performed.
type OpenReport struct {
	Recovery repodir.Recovery
	Store    store.OpenReport
}

// Create initializes a new repository directory.
func Create(path string, cfg policy.Config) error { return repodir.Create(path, cfg) }

// Open opens the repository at path. It takes the repository lock, makes
// refs and refs.log consistent after a crash, and brings cache.db up to
// date.
func Open(ctx context.Context, path string, opts Options) (*Repo, OpenReport, error) {
	var rep OpenReport
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.NodeCache <= 0 {
		opts.NodeCache = 8192
	}
	if opts.GroupWindow <= 0 {
		opts.GroupWindow = 20 * time.Millisecond
	}
	if opts.GroupMax <= 0 {
		opts.GroupMax = 1000
	}
	if opts.Store.Logger == nil {
		opts.Store.Logger = opts.Logger
	}
	dir, err := repodir.Open(path)
	if err != nil {
		return nil, rep, err
	}
	if rep.Recovery, err = dir.Recover(); err != nil {
		dir.Close()
		return nil, rep, err
	}
	if rep.Recovery.Warning != "" {
		opts.Logger.Warn("refs recovery", "repo", path, "warning", rep.Recovery.Warning)
	}
	st, srep, err := store.Open(ctx, dir, opts.Store)
	rep.Store = srep
	if err != nil {
		dir.Close()
		return nil, rep, err
	}
	refs, err := dir.ReadRefs()
	if err != nil {
		st.Close(ctx)
		dir.Close()
		return nil, rep, err
	}
	stopCtx, stop := context.WithCancel(context.Background())
	r := &Repo{
		dir: dir, meta: dir.Meta(), store: st, log: opts.Logger, opts: opts,
		nodes: tree.NewNodes(st, opts.NodeCache),
		refs:  refs, groups: map[string]*committer{}, digestSem: make(chan struct{}, 1),
		stop: stop, stopCtx: stopCtx,
	}
	r.gateCond = sync.NewCond(&r.gateMu)
	if err := r.loadLocks(); err != nil {
		st.Close(ctx)
		dir.Close()
		return nil, rep, err
	}
	return r, rep, nil
}

// Close stops the group committers, seals the open pack and releases the
// repository. A frozen repository is unfrozen first.
func (r *Repo) Close(ctx context.Context) error {
	r.Unfreeze()
	r.groupMu.Lock()
	if r.closed {
		r.groupMu.Unlock()
		return nil
	}
	r.closed = true
	r.groupMu.Unlock()
	r.stop()
	r.wg.Wait()
	err := r.store.Close(ctx)
	return errors.Join(err, r.dir.Close())
}

// Path returns the repository directory.
func (r *Repo) Path() string { return r.dir.Path() }

// Meta returns the repository metadata and namespace configuration.
func (r *Repo) Meta() repodir.Meta { return r.meta }

// Store returns the pack store.
func (r *Repo) Store() *store.Store { return r.store }

// Nodes returns the tree node store.
func (r *Repo) Nodes() *tree.Nodes { return r.nodes }

// Refs returns a copy of the current refs.
func (r *Repo) Refs() map[string]hash.Hash {
	r.refMu.Lock()
	defer r.refMu.Unlock()
	out := make(map[string]hash.Hash, len(r.refs))
	for k, v := range r.refs {
		out[k] = v
	}
	return out
}

// Ref returns the value of a ref (short or full name), or the zero hash if
// it does not exist.
func (r *Repo) Ref(name string) hash.Hash {
	r.refMu.Lock()
	defer r.refMu.Unlock()
	return r.refs[policy.FullRefName(name)]
}

// ResolveRef resolves a ref name (short or full) or a hex commit hash to a
// commit hash.
func (r *Repo) ResolveRef(ctx context.Context, name string) (hash.Hash, error) {
	if h := r.Ref(name); !h.IsZero() {
		return h, nil
	}
	if h, err := hash.Parse(name); err == nil {
		if _, err := r.ReadCommit(ctx, h); err == nil {
			return h, nil
		}
	}
	return hash.Zero, fmt.Errorf("%w: ref %q", ErrNotFound, name)
}

// RefUpdate is one compare-and-swap ref update. A zero Old means the ref
// must not exist; a zero New deletes the ref.
type RefUpdate struct {
	Ref      string
	Old, New hash.Hash
}

// ConflictError reports a failed compare-and-swap.
type ConflictError struct {
	Ref      string
	Expected hash.Hash
	Current  hash.Hash
	Paths    []string // a refused rebase: the paths changed since Expected
}

func (e *ConflictError) Error() string {
	if len(e.Paths) > 0 {
		return fmt.Sprintf("repo: ref %s is %s, expected %s, and changed %s since", e.Ref, e.Current.Short(), e.Expected.Short(), strings.Join(e.Paths, ", "))
	}
	return fmt.Sprintf("repo: ref %s is %s, expected %s", e.Ref, e.Current.Short(), e.Expected.Short())
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// UpdateOptions describe who updates refs and how changes are checked.
type UpdateOptions struct {
	Principal string // recorded in refs.log; the committer for lock checks
	Reason    string
	// CheckPath, if set, is called for every file a branch update changes
	// (the protocol layer's write ACL check); an error rejects the update.
	CheckPath func(ref string, c PathChange) error

	skipLocks bool // the group committer checked locks per request
}

// UpdateRefs applies ref updates atomically: all old values are checked,
// then one refs.log group is appended and refs is replaced. Objects the new
// values point to must be stored; the open pack is sealed first so that
// they are durable before any ref points to them.
//
// Branch updates (heads/...) are checked file by file: the changed files are
// the diff from the old value, or for a new branch from its merge base with
// the default ref. Each is passed to opts.CheckPath, and checked against
// the namespace's lock policy (locks of the ref, and for a new branch also
// of the default ref).
func (r *Repo) UpdateRefs(ctx context.Context, updates []RefUpdate, opts UpdateOptions) error {
	end, err := r.beginWrite()
	if err != nil {
		return err
	}
	defer end()
	return r.updateRefs(ctx, updates, opts)
}

type refCheck struct {
	changes  []PathChange
	lockRefs []string
}

// updateRefs is UpdateRefs for callers that already registered the write.
func (r *Repo) updateRefs(ctx context.Context, updates []RefUpdate, opts UpdateOptions) error {
	if len(updates) == 0 {
		return nil
	}
	// The objects the refs will point to are checked and the refs written
	// on the same side of a GC's epoch start and condemn step.
	release := r.store.Guard()
	defer release()
	mode, _ := policy.ParseLocks(r.meta.Locks)
	checkLocks := mode.Mode != "off" && !opts.skipLocks
	seen := map[string]bool{}
	checks := make([]*refCheck, len(updates))
	for i := range updates {
		updates[i].Ref = policy.FullRefName(updates[i].Ref)
		u := updates[i]
		if err := policy.ValidRefName(u.Ref); err != nil {
			return err
		}
		if seen[u.Ref] {
			return fmt.Errorf("repo: ref %s updated twice in one transaction", u.Ref)
		}
		seen[u.Ref] = true
		if u.New.IsZero() {
			continue
		}
		ok, err := r.store.HasGuarded(ctx, u.New)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: ref %s → %s", store.ErrMissingReference, u.Ref, u.New.Short())
		}
		if isBranch(u.Ref) && (checkLocks || opts.CheckPath != nil) {
			changes, lockRefs, err := r.refChanges(ctx, u)
			if err != nil {
				return err
			}
			if opts.CheckPath != nil {
				for _, c := range changes {
					if err := opts.CheckPath(u.Ref, c); err != nil {
						return err
					}
				}
			}
			checks[i] = &refCheck{changes: changes, lockRefs: lockRefs}
		}
	}
	if err := r.store.Flush(ctx); err != nil {
		return err
	}

	r.refMu.Lock()
	defer r.refMu.Unlock()
	for _, u := range updates {
		if cur := r.refs[u.Ref]; cur != u.Old {
			return &ConflictError{Ref: u.Ref, Expected: u.Old, Current: cur}
		}
	}
	if checkLocks {
		r.locksMu.Lock()
		now := time.Now()
		for _, c := range checks {
			if c == nil {
				continue
			}
			for _, ch := range c.changes {
				if err := r.lockCheck(c.lockRefs, ch.Path, opts.Principal, mode, now); err != nil {
					r.locksMu.Unlock()
					return err
				}
			}
		}
		r.locksMu.Unlock()
	}
	now := time.Now()
	tx := ulid.Make().String()
	recs := make([]repodir.LogRecord, len(updates))
	next := make(map[string]hash.Hash, len(r.refs)+len(updates))
	for k, v := range r.refs {
		next[k] = v
	}
	for i, u := range updates {
		recs[i] = repodir.LogRecord{Time: now, Tx: tx, Principal: opts.Principal, Ref: u.Ref, Old: u.Old, New: u.New, Reason: opts.Reason}
		if u.New.IsZero() {
			delete(next, u.Ref)
		} else {
			next[u.Ref] = u.New
		}
	}
	if err := r.dir.AppendLog(recs); err != nil {
		return err
	}
	if err := r.dir.WriteRefs(next); err != nil {
		return err
	}
	r.refs = next
	refUpdates.Add(nil, float64(len(updates)))
	return nil
}

var refUpdates = metrics.Default.NewCounter("f1_ref_updates_total", "Ref updates (commits, pushes, ref changes).")

// ReadCommit reads a commit object.
func (r *Repo) ReadCommit(ctx context.Context, h hash.Hash) (*object.Commit, error) {
	p, err := r.payload(ctx, h, object.TypeCommit)
	if err != nil {
		return nil, err
	}
	return object.DecodeAs[*object.Commit](p)
}

func (r *Repo) payload(ctx context.Context, h hash.Hash, want object.Type) ([]byte, error) {
	rec, err := r.store.Get(ctx, h)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: object %s", ErrNotFound, h.Short())
	}
	if err != nil {
		return nil, err
	}
	if rec.Type != want {
		return nil, fmt.Errorf("repo: object %s is a %s, not a %s", h.Short(), rec.Type, want)
	}
	return rec.Payload(nil)
}

// putObject stores an object unless it is already stored.
func (r *Repo) putObject(ctx context.Context, o object.Object) (hash.Hash, error) {
	h, b, err := object.Hash(o)
	if err != nil {
		return hash.Zero, err
	}
	ok, err := r.store.Has(ctx, h)
	if err != nil || ok {
		return h, err
	}
	rec, err := pack.NewRecord(o.Type(), h, b, nil)
	if err != nil {
		return hash.Zero, err
	}
	return h, r.store.Put(ctx, rec)
}

// Ingest stores file content and returns its File hash and digests. size
// is the content length if known, or -1.
func (r *Repo) Ingest(ctx context.Context, rd io.Reader, size int64) (content.Result, error) {
	end, err := r.beginWrite()
	if err != nil {
		return content.Result{}, err
	}
	defer end()
	return content.Ingest(ctx, r.store, rd, size, content.Options{})
}
