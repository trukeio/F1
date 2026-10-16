package repo

import (
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"sync"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

// The commit graph index (ADR-0015): for each commit, its generation
// number, committer time, parents and a filter of the paths it changed.
// Rows are computed when a history query first needs them, kept in memory
// and written to cache.db, from which they are loaded on the next open.
// They are derived data: a rebuilt cache.db recomputes them.

// graphRow is one commit's row. Rows are not modified once published;
// adding the path filter replaces the row.
type graphRow struct {
	gen     uint64
	time    int64
	parents []hash.Hash
	partial bool   // a parent was not stored when the row was computed
	paths   []byte // changed-path filter; nil until computed
}

// Path filter modes, the first byte of graphRow.paths.
const (
	filterAny   = 0 // too many changed paths: any path may have changed
	filterFirst = 1 // paths changed relative to the first parent
	filterRoot  = 2 // paths of the commit's tree (no parent was stored)
)

// Filter parameters are Git's changed-path Bloom filters': 10 bits per
// path, 7 hash functions, at most 512 paths.
const (
	filterBitsPerPath = 10
	filterHashes      = 7
	filterMaxPaths    = 512
)

// graphFlushRows is the number of new rows written to cache.db at once.
const graphFlushRows = 5000

// errGraphReset restarts a query after the graph was found inconsistent.
var errGraphReset = errors.New("repo: commit graph reset")

type graphState struct {
	loaded  bool
	epoch   int // incremented by reset; rows computed in an older epoch are dropped
	rows    map[hash.Hash]*graphRow
	pending []hash.Hash
}

// loadGraph reads the rows of cache.db once. Rows of commits that are no
// longer stored (removed by GC) are deleted.
func (r *Repo) loadGraph(ctx context.Context) error {
	r.graphMu.Lock()
	loaded := r.graph.loaded
	r.graphMu.Unlock()
	if loaded {
		return nil
	}
	rows := map[hash.Hash]*graphRow{}
	var stale []hash.Hash
	var err error
	if e := r.store.Cache().GraphRows(ctx, func(gr cache.GraphRow) {
		if err != nil {
			return
		}
		var ok bool
		if ok, err = r.store.Exists(ctx, gr.Commit); !ok {
			stale = append(stale, gr.Commit)
			return
		}
		rows[gr.Commit] = &graphRow{gen: gr.Generation, time: gr.Time, parents: gr.Parents, partial: gr.Flags&1 != 0, paths: gr.Paths}
	}); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	if err := r.store.Cache().DeleteGraphRows(ctx, stale); err != nil {
		return err
	}
	r.graphMu.Lock()
	defer r.graphMu.Unlock()
	if !r.graph.loaded {
		r.graph.loaded = true
		r.graph.rows = rows
	}
	return nil
}

func (r *Repo) graphRow(h hash.Hash) (*graphRow, int) {
	r.graphMu.Lock()
	defer r.graphMu.Unlock()
	return r.graph.rows[h], r.graph.epoch
}

func (r *Repo) putGraphRow(h hash.Hash, row *graphRow, epoch int) {
	r.graphMu.Lock()
	defer r.graphMu.Unlock()
	if epoch != r.graph.epoch {
		return
	}
	r.graph.rows[h] = row
	r.graph.pending = append(r.graph.pending, h)
}

// flushGraph writes the rows computed since the last flush.
func (r *Repo) flushGraph(ctx context.Context) error {
	r.graphMu.Lock()
	var rows []cache.GraphRow
	for _, h := range r.graph.pending {
		g := r.graph.rows[h]
		if g == nil {
			continue
		}
		flags := 0
		if g.partial {
			flags = 1
		}
		rows = append(rows, cache.GraphRow{Commit: h, Generation: g.gen, Time: g.time, Parents: g.parents, Flags: flags, Paths: g.paths})
	}
	r.graph.pending = nil
	r.graphMu.Unlock()
	return r.store.Cache().PutGraphRows(ctx, rows)
}

// resetGraph drops every row. It is needed when a commit that was missing
// as a parent is stored again, which can break the generation order.
func (r *Repo) resetGraph(ctx context.Context) error {
	r.graphMu.Lock()
	r.graph.epoch++
	r.graph.rows = map[hash.Hash]*graphRow{}
	r.graph.pending = nil
	r.graphMu.Unlock()
	r.log.Warn("commit graph reset: a missing parent commit was stored again")
	return r.store.Cache().ClearGraph(ctx)
}

// retryGraph runs a graph query, and once more after a reset.
func (r *Repo) retryGraph(fn func() error) error {
	err := fn()
	if errors.Is(err, errGraphReset) {
		err = fn()
	}
	return err
}

// ensureGraph computes the rows of start and of every stored ancestor that
// has none, parents first.
func (r *Repo) ensureGraph(ctx context.Context, start hash.Hash) error {
	if err := r.loadGraph(ctx); err != nil {
		return err
	}
	if row, _ := r.graphRow(start); row != nil {
		return nil
	}
	_, epoch := r.graphRow(start)
	commits := map[hash.Hash]*object.Commit{}
	stack := []hash.Hash{start}
	added := 0
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		if row, _ := r.graphRow(h); row != nil {
			stack = stack[:len(stack)-1]
			continue
		}
		c := commits[h]
		if c == nil {
			var err error
			if c, err = r.ReadCommit(ctx, h); err != nil {
				return err
			}
			commits[h] = c
		}
		row := &graphRow{gen: 1, time: c.Committer.Time, parents: c.Parents}
		waiting := false
		for _, p := range c.Parents {
			pr, _ := r.graphRow(p)
			if pr == nil {
				ok, err := r.store.Exists(ctx, p)
				if err != nil {
					return err
				}
				if !ok {
					row.partial = true
					continue
				}
				stack = append(stack, p)
				waiting = true
				continue
			}
			row.gen = max(row.gen, pr.gen+1)
		}
		if waiting {
			continue
		}
		r.putGraphRow(h, row, epoch)
		delete(commits, h)
		stack = stack[:len(stack)-1]
		if added++; added%graphFlushRows == 0 {
			if err := r.flushGraph(ctx); err != nil {
				return err
			}
		}
	}
	return r.flushGraph(ctx)
}

// storedParents returns the parents of a commit that are stored, in order,
// with their rows. A stored parent whose row was computed while it was
// missing breaks the generation order, and resets the graph.
func (r *Repo) storedParents(ctx context.Context, row *graphRow) ([]hash.Hash, []*graphRow, error) {
	var hs []hash.Hash
	var rows []*graphRow
	for _, p := range row.parents {
		ok, err := r.store.Exists(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		pr, _ := r.graphRow(p)
		if pr == nil {
			if !row.partial {
				// A row that was dropped meanwhile (reset).
				return nil, nil, errGraphReset
			}
			if err := r.resetGraph(ctx); err != nil {
				return nil, nil, err
			}
			return nil, nil, errGraphReset
		}
		hs = append(hs, p)
		rows = append(rows, pr)
	}
	return hs, rows, nil
}

// pathFilter returns the commit's changed-path filter, computing it if
// needed.
func (r *Repo) pathFilter(ctx context.Context, h hash.Hash, row *graphRow) ([]byte, error) {
	if row.paths != nil {
		return row.paths, nil
	}
	c, err := r.ReadCommit(ctx, h)
	if err != nil {
		return nil, err
	}
	mode := byte(filterRoot)
	base := hash.Zero
	if len(c.Parents) > 0 {
		ok, err := r.store.Exists(ctx, c.Parents[0])
		if err != nil {
			return nil, err
		}
		if ok {
			mode, base = filterFirst, c.Parents[0]
		}
	}
	var paths []string
	seen := map[string]bool{}
	errMany := errors.New("too many paths")
	add := func(p string) error {
		if seen[p] {
			return nil
		}
		if len(paths) == filterMaxPaths {
			return errMany
		}
		seen[p] = true
		paths = append(paths, p)
		return nil
	}
	err = r.Diff(ctx, base, h, "", func(pc PathChange) error {
		if err := add(pc.Path); err != nil {
			return err
		}
		if r.meta.Layout != policy.LayoutFlat {
			for i := strings.LastIndexByte(pc.Path, '/'); i > 0; i = strings.LastIndexByte(pc.Path[:i], '/') {
				if err := add(pc.Path[:i]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	var f []byte
	switch {
	case errors.Is(err, errMany):
		f = []byte{filterAny}
	case err != nil:
		return nil, err
	default:
		f = buildFilter(mode, paths)
	}
	nr := *row
	nr.paths = f
	_, epoch := r.graphRow(h)
	r.putGraphRow(h, &nr, epoch)
	return f, nil
}

// prefetchWindow is the number of filters prefetchFilters computes at
// most: commits a walk may stop before reaching cost at most this much.
const prefetchWindow = 256

// prefetchFilters computes, with one worker per CPU, the missing path
// filters of h and of the commits a walk from h and the queue q reaches
// first (a breadth-first approximation of the walk's order).
func (r *Repo) prefetchFilters(ctx context.Context, h hash.Hash, q graphQueue) error {
	var todo []hash.Hash
	seen := map[hash.Hash]bool{}
	frontier := []hash.Hash{h}
	for _, it := range q {
		frontier = append(frontier, it.h)
	}
	for len(frontier) > 0 && len(todo) < prefetchWindow {
		c := frontier[0]
		frontier = frontier[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		row, _ := r.graphRow(c)
		if row == nil {
			continue
		}
		if row.paths == nil {
			todo = append(todo, c)
		}
		frontier = append(frontier, row.parents...)
	}
	work := make(chan hash.Hash)
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(todo)) {
		wg.Go(func() {
			for c := range work {
				row, _ := r.graphRow(c)
				if row == nil || row.paths != nil {
					continue
				}
				if _, err := r.pathFilter(ctx, c, row); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		})
	}
	for _, c := range todo {
		work <- c
	}
	close(work)
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func buildFilter(mode byte, paths []string) []byte {
	bits := max(64, len(paths)*filterBitsPerPath)
	f := make([]byte, 1+(bits+7)/8)
	f[0] = mode
	m := uint64(len(f)-1) * 8
	for _, p := range paths {
		h1, h2 := filterHash(p)
		for i := range uint64(filterHashes) {
			b := (h1 + i*h2) % m
			f[1+b/8] |= 1 << (b % 8)
		}
	}
	return f
}

// filterMayContain reports whether the filter may contain path; false
// means the commit certainly did not change it.
func filterMayContain(f []byte, path string) bool {
	if f[0] == filterAny {
		return true
	}
	m := uint64(len(f)-1) * 8
	h1, h2 := filterHash(path)
	for i := range uint64(filterHashes) {
		b := (h1 + i*h2) % m
		if f[1+b/8]&(1<<(b%8)) == 0 {
			return false
		}
	}
	return true
}

func filterHash(path string) (uint64, uint64) {
	h := hash.Plain([]byte(path))
	return binary.LittleEndian.Uint64(h[0:8]), binary.LittleEndian.Uint64(h[8:16]) | 1
}

// graphItem is a commit in a history walk queue.
type graphItem struct {
	h    hash.Hash
	key1 int64 // primary order, descending (time or generation)
}

type graphQueue []graphItem

func (q graphQueue) Len() int { return len(q) }
func (q graphQueue) Less(i, j int) bool {
	if q[i].key1 != q[j].key1 {
		return q[i].key1 > q[j].key1
	}
	return hash.Compare(q[i].h, q[j].h) < 0
}
func (q graphQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *graphQueue) Push(x any)   { *q = append(*q, x.(graphItem)) }
func (q *graphQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// logPath is Log with a path, using the commit graph: commits are walked
// by committer time through their rows, and a commit whose path filter
// excludes the path is skipped without reading trees.
func (r *Repo) logPath(ctx context.Context, start hash.Hash, path string, limit int) ([]CommitInfo, error) {
	var out []CommitInfo
	err := r.retryGraph(func() error {
		out = nil
		if err := r.ensureGraph(ctx, start); err != nil {
			return err
		}
		defer r.flushGraph(ctx)
		row, _ := r.graphRow(start)
		if row == nil {
			return errGraphReset
		}
		q := &graphQueue{{start, row.time}}
		seen := map[hash.Hash]bool{start: true}
		for q.Len() > 0 && (limit <= 0 || len(out) < limit) {
			it := heap.Pop(q).(graphItem)
			row, _ := r.graphRow(it.h)
			if row == nil {
				return errGraphReset
			}
			parents, prows, err := r.storedParents(ctx, row)
			if err != nil {
				return err
			}
			for i, p := range parents {
				if !seen[p] {
					seen[p] = true
					heap.Push(q, graphItem{p, prows[i].time})
				}
			}
			if row.paths == nil {
				// Compute the filters of the commits the walk reaches next
				// in parallel; a filter needs a tree diff.
				if err := r.prefetchFilters(ctx, it.h, *q); err != nil {
					return err
				}
				if row, _ = r.graphRow(it.h); row == nil {
					return errGraphReset
				}
			}
			f, err := r.pathFilter(ctx, it.h, row)
			if err != nil {
				return err
			}
			// The filter answers only if it was computed against what
			// Log compares with now: the first parent, or nothing.
			usable := f[0] == filterFirst && len(parents) > 0 && parents[0] == row.parents[0] ||
				f[0] == filterRoot && len(parents) == 0
			if usable && !filterMayContain(f, path) {
				continue
			}
			c, err := r.ReadCommit(ctx, it.h)
			if err != nil {
				return err
			}
			include, err := r.changedPath(ctx, c, parents, path)
			if err != nil {
				return err
			}
			if include {
				out = append(out, CommitInfo{it.h, c})
			}
		}
		return nil
	})
	return out, err
}

// changedPath reports whether commit c changed path relative to every one
// of its stored parents (for a root, whether the path exists).
func (r *Repo) changedPath(ctx context.Context, c *object.Commit, parents []hash.Hash, path string) (bool, error) {
	cur, err := r.entryAt(ctx, c.Tree, path)
	if err != nil {
		return false, err
	}
	if len(parents) == 0 {
		return cur != nil, nil
	}
	for _, p := range parents {
		pc, err := r.ReadCommit(ctx, p)
		if err != nil {
			return false, err
		}
		prev, err := r.entryAt(ctx, pc.Tree, path)
		if err != nil {
			return false, err
		}
		if sameEntry(cur, prev) {
			return false, nil
		}
	}
	return true, nil
}

// mergeBaseGraph walks from a and b by descending generation. The first
// commit reached from both is a common ancestor of the highest
// generation, so no other common ancestor descends from it: a best common
// ancestor.
func (r *Repo) mergeBaseGraph(ctx context.Context, a, b hash.Hash) (hash.Hash, error) {
	if a == b {
		return a, nil
	}
	var base hash.Hash
	err := r.retryGraph(func() error {
		base = hash.Zero
		for _, h := range []hash.Hash{a, b} {
			if err := r.ensureGraph(ctx, h); err != nil {
				return err
			}
		}
		flags := map[hash.Hash]uint8{a: 1, b: 2}
		q := &graphQueue{}
		for _, h := range []hash.Hash{a, b} {
			row, _ := r.graphRow(h)
			if row == nil {
				return errGraphReset
			}
			heap.Push(q, graphItem{h, int64(row.gen)})
		}
		for q.Len() > 0 {
			it := heap.Pop(q).(graphItem)
			f := flags[it.h]
			if f == 3 {
				base = it.h
				return nil
			}
			row, _ := r.graphRow(it.h)
			if row == nil {
				return errGraphReset
			}
			parents, prows, err := r.storedParents(ctx, row)
			if err != nil {
				return err
			}
			for i, p := range parents {
				if flags[p] == 0 {
					heap.Push(q, graphItem{p, int64(prows[i].gen)})
				}
				flags[p] |= f
			}
		}
		return nil
	})
	return base, err
}
