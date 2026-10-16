package repo

import (
	"container/heap"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

// refLogPath is Log with a path before ADR-0015: a time-ordered walk that
// compares the path's entry with every parent.
func refLogPath(r *Repo, start hash.Hash, path string, limit int) ([]hash.Hash, error) {
	var out []hash.Hash
	q := &commitQueue{}
	seen := map[hash.Hash]bool{start: true}
	c, err := r.ReadCommit(ctx, start)
	if err != nil {
		return nil, err
	}
	heap.Push(q, CommitInfo{start, c})
	for q.Len() > 0 && (limit <= 0 || len(out) < limit) {
		ci := heap.Pop(q).(CommitInfo)
		var parents []hash.Hash
		for _, p := range ci.Commit.Parents {
			pc, err := r.ReadCommit(ctx, p)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			parents = append(parents, p)
			if !seen[p] {
				seen[p] = true
				heap.Push(q, CommitInfo{p, pc})
			}
		}
		include, err := r.changedPath(ctx, ci.Commit, parents, path)
		if err != nil {
			return nil, err
		}
		if include {
			out = append(out, ci.Hash)
		}
	}
	return out, nil
}

// ancestors returns the stored commits reachable from h, h included.
func ancestors(t *testing.T, r *Repo, h hash.Hash) map[hash.Hash]bool {
	t.Helper()
	out := map[hash.Hash]bool{}
	stack := []hash.Hash{h}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[c] {
			continue
		}
		co, err := r.ReadCommit(ctx, c)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[c] = true
		stack = append(stack, co.Parents...)
	}
	return out
}

// checkMergeBase checks that MergeBase(a, b) is a best common ancestor:
// common to both, and not an ancestor of another common ancestor.
func checkMergeBase(t *testing.T, r *Repo, a, b hash.Hash) {
	t.Helper()
	got, err := r.MergeBase(ctx, a, b)
	if err != nil {
		t.Fatal(err)
	}
	aa, ab := ancestors(t, r, a), ancestors(t, r, b)
	var common []hash.Hash
	for h := range aa {
		if ab[h] {
			common = append(common, h)
		}
	}
	if len(common) == 0 {
		if !got.IsZero() {
			t.Fatalf("merge base %s of unrelated commits", got.Short())
		}
		return
	}
	if !aa[got] || !ab[got] {
		t.Fatalf("merge base %s is not a common ancestor", got.Short())
	}
	for _, c := range common {
		if c != got && ancestors(t, r, c)[got] {
			t.Fatalf("merge base %s is an ancestor of common ancestor %s", got.Short(), c.Short())
		}
	}
}

// randomHistory writes n commits on top of tips, with merges, random
// changes to files in a few directories, and committer times that are
// mostly but not always increasing. It returns every commit written.
func randomHistory(t *testing.T, r *Repo, rng *rand.Rand, n int, tips []hash.Hash) []hash.Hash {
	t.Helper()
	var all []hash.Hash
	now := int64(1_700_000_000)
	for i := range n {
		var parents []hash.Hash
		if len(tips) > 0 {
			parents = append(parents, tips[rng.IntN(len(tips))])
			if len(tips) > 1 && rng.IntN(5) == 0 {
				if p := tips[rng.IntN(len(tips))]; p != parents[0] {
					parents = append(parents, p)
				}
			}
		}
		base := hash.Zero
		if len(parents) > 0 {
			base = parents[0]
		}
		root, err := r.commitTree(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		var changes []Change
		for range 1 + rng.IntN(3) {
			p := fmt.Sprintf("d%d/e%d/f%d", rng.IntN(3), rng.IntN(3), rng.IntN(4))
			if rng.IntN(6) == 0 {
				changes = append(changes, Change{Op: OpDelete, Path: p})
			} else {
				changes = append(changes, Change{Op: OpPut, Path: p, Entry: fileEntry(t, r, fmt.Sprint(p, i))})
			}
		}
		if root, err = r.applyChanges(ctx, root, changes); err != nil {
			t.Fatal(err)
		}
		now += int64(rng.IntN(100)) - 20
		c := &object.Commit{Tree: root, Parents: parents, Message: []byte(fmt.Sprint(i)),
			Author: object.Person{Name: []byte("a"), Time: now, TZ: "+0000"}, Committer: object.Person{Name: []byte("a"), Time: now, TZ: "+0000"}}
		h, err := r.putObject(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, h)
		// The new commit replaces a parent as a tip, or starts a branch.
		if len(parents) > 0 && rng.IntN(4) != 0 {
			tips[slices.Index(tips, parents[0])] = h
		} else {
			tips = append(tips, h)
		}
	}
	if err := r.store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return all
}

var graphPaths = []string{"d0", "d1/e2", "d0/e0/f0", "d2/e1/f3", "d1/e1/f1", "nope"}

func compareLogs(t *testing.T, r *Repo, head hash.Hash) {
	t.Helper()
	for _, p := range graphPaths {
		for _, limit := range []int{0, 3} {
			want, err := refLogPath(r, head, p, limit)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Log(ctx, head, p, limit)
			if err != nil {
				t.Fatal(err)
			}
			var gh []hash.Hash
			for _, c := range got {
				gh = append(gh, c.Hash)
			}
			if !slices.Equal(gh, want) {
				t.Fatalf("Log(%s, %q, %d) = %d commits, want %d", head.Short(), p, limit, len(gh), len(want))
			}
		}
	}
}

// The graph gives the same answers as the walks it replaces.
func TestGraphMatchesWalks(t *testing.T) {
	for seed := range uint64(6) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := newRepo(t, policy.Default())
			rng := rand.New(rand.NewPCG(seed, 7))
			all := randomHistory(t, r, rng, 150, nil)
			// A second, unrelated history.
			other := randomHistory(t, r, rng, 20, nil)
			for _, h := range []hash.Hash{all[len(all)-1], all[100], all[37]} {
				compareLogs(t, r, h)
			}
			for range 40 {
				checkMergeBase(t, r, all[rng.IntN(len(all))], all[rng.IntN(len(all))])
			}
			checkMergeBase(t, r, all[len(all)-1], other[len(other)-1])
			// More history on top of computed rows.
			more := randomHistory(t, r, rng, 50, []hash.Hash{all[len(all)-1], all[120]})
			compareLogs(t, r, more[len(more)-1])
			checkMergeBase(t, r, more[len(more)-1], more[10])
		})
	}
}

// Rows are written to cache.db and loaded on the next open.
func TestGraphPersists(t *testing.T) {
	r := newRepo(t, policy.Default())
	all := randomHistory(t, r, rand.New(rand.NewPCG(1, 1)), 80, nil)
	head := all[len(all)-1]
	compareLogs(t, r, head)
	want := len(ancestors(t, r, head))
	path := r.Path()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r2, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close(ctx)
	if err := r2.loadGraph(ctx); err != nil {
		t.Fatal(err)
	}
	withFilter := 0
	for _, row := range r2.graph.rows {
		if row.paths != nil {
			withFilter++
		}
	}
	if len(r2.graph.rows) != want || withFilter != want {
		t.Fatalf("%d rows, %d with filters, want %d", len(r2.graph.rows), withFilter, want)
	}
	compareLogs(t, r2, head)
}

// Truncated history: commits removed by GC end the walks, and their rows
// are dropped.
func TestGraphTruncatedHistory(t *testing.T) {
	cfg := policy.Default()
	cfg.Retention = "last:5"
	r := newRepo(t, cfg)
	var heads []hash.Hash
	for i := range 20 {
		heads = append(heads, put(t, r, "main", map[string]string{fmt.Sprintf("d/f%d", i%4): fmt.Sprint(i), "keep": "k"}))
	}
	head := heads[len(heads)-1]
	// Rows for the whole history, then GC removes most of it.
	compareLogs(t, r, head)
	if _, err := r.Log(ctx, head, "keep", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := gc.Run(ctx, r, gc.Options{Grace: -1}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.store.Exists(ctx, heads[0]); ok {
		t.Fatal("GC kept the oldest commit")
	}
	log, err := r.Log(ctx, head, "keep", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The oldest retained commit is now a root, and "keep" exists there.
	if len(log) != 1 || log[0].Hash != heads[len(heads)-5] {
		t.Fatalf("log of keep = %d commits", len(log))
	}
	for _, p := range []string{"d/f0", "d/f3", "d", "keep"} {
		want, err := refLogPath(r, head, p, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Log(ctx, head, p, 0)
		if err != nil || len(got) != len(want) {
			t.Fatalf("Log(%q) = %d, want %d, %v", p, len(got), len(want), err)
		}
	}
	checkMergeBase(t, r, head, heads[len(heads)-3])
	// Reopening drops the rows of collected commits.
	path := r.Path()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r2, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close(ctx)
	if err := r2.loadGraph(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(r2.graph.rows); n != 5 {
		t.Fatalf("%d rows after GC, want 5", n)
	}
}

// Branch creation uses the merge base with the default branch for lock
// checks; with the graph it no longer walks the whole history.
func TestBranchCreationMergeBase(t *testing.T) {
	r := newRepo(t, policy.Default())
	var head hash.Hash
	for i := range 30 {
		head = put(t, r, "main", map[string]string{fmt.Sprintf("f%d", i%3): fmt.Sprint(i)})
	}
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "heads/topic", New: head}}, UpdateOptions{Principal: "test", Reason: "branch"}); err != nil {
		t.Fatal(err)
	}
	mb, err := r.MergeBase(ctx, head, head)
	if err != nil || mb != head {
		t.Fatalf("merge base of a commit with itself = %s, %v", mb.Short(), err)
	}
}

func TestPathFilter(t *testing.T) {
	var paths []string
	for i := range 300 {
		paths = append(paths, fmt.Sprintf("dir%d/file%d.txt", i%17, i))
	}
	f := buildFilter(filterFirst, paths)
	for _, p := range paths {
		if !filterMayContain(f, p) {
			t.Fatalf("filter lost %q", p)
		}
	}
	fp := 0
	for i := range 10000 {
		if filterMayContain(f, fmt.Sprintf("other/%d", i)) {
			fp++
		}
	}
	// 10 bits per path and 7 hashes give about 0.8 % false positives.
	if fp > 200 {
		t.Fatalf("%d false positives in 10,000", fp)
	}
	if !filterMayContain([]byte{filterAny}, "anything") {
		t.Fatal("an any-path filter excluded a path")
	}
}
