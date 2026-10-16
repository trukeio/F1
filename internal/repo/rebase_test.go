package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repodir"
)

// rebased commits changes based on parent with Rebase set.
func rebased(r *Repo, parent hash.Hash, changes ...Change) (hash.Hash, hash.Hash, error) {
	return r.CommitOn(ctx, CommitRequest{Ref: "main", Parent: &parent, Changes: changes, Author: alice, Message: "rebased", Rebase: true})
}

// The rebase rule of ADR-0014: a write applies on the moved head unless a
// path it names, or a parent directory of one, changed since its parent.
func TestRebaseRule(t *testing.T) {
	cases := []struct {
		name    string
		layout  string
		initial map[string]string
		other   []Change // the concurrent write that moves the head
		then    []Change // a second concurrent write, if any
		mine    []Change
		ok      bool
	}{
		{name: "disjoint files", initial: map[string]string{"a": "1", "b": "1"},
			other: []Change{{Op: OpPut, Path: "a", Entry: object.Entry{}}}, mine: []Change{{Op: OpPut, Path: "b"}}, ok: true},
		{name: "same file", initial: map[string]string{"a": "1"},
			other: []Change{{Op: OpPut, Path: "a"}}, mine: []Change{{Op: OpPut, Path: "a"}}},
		{name: "deleted by other", initial: map[string]string{"a": "1"},
			other: []Change{{Op: OpDelete, Path: "a"}}, mine: []Change{{Op: OpPut, Path: "a"}}},
		{name: "created by both", initial: map[string]string{"x": "1"},
			other: []Change{{Op: OpPut, Path: "new"}}, mine: []Change{{Op: OpPut, Path: "new"}}},
		{name: "sibling in a directory", initial: map[string]string{"d/a": "1", "d/b": "1"},
			other: []Change{{Op: OpPut, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "d/b"}}, ok: true},
		{name: "change below a deleted directory", initial: map[string]string{"d/a": "1", "d/b": "1"},
			other: []Change{{Op: OpDelete, Path: "d/a"}}, mine: []Change{{Op: OpDelete, Path: "d"}}},
		{name: "directory deleted, new file in it", initial: map[string]string{"d/a": "1", "x": "1"},
			other: []Change{{Op: OpDelete, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "d/new"}}},
		{name: "directory replaced by a file", initial: map[string]string{"d/a": "1"},
			other: []Change{{Op: OpDelete, Path: "d/a"}}, then: []Change{{Op: OpPut, Path: "d"}}, mine: []Change{{Op: OpPut, Path: "d/b"}}},
		{name: "file replaced by a directory", initial: map[string]string{"d": "1", "x": "1"},
			other: []Change{{Op: OpDelete, Path: "d"}}, then: []Change{{Op: OpPut, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "x"}}, ok: true},
		{name: "file in a new directory", initial: map[string]string{"x": "1"},
			other: []Change{{Op: OpPut, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "d/b"}}, ok: true},
		{name: "rename source changed", initial: map[string]string{"a": "1", "y": "1"},
			other: []Change{{Op: OpPut, Path: "a"}}, mine: []Change{{Op: OpRename, From: "a", Path: "b"}}},
		{name: "rename target created", initial: map[string]string{"a": "1"},
			other: []Change{{Op: OpPut, Path: "b"}}, mine: []Change{{Op: OpRename, From: "a", Path: "b"}}},
		{name: "rename, disjoint", initial: map[string]string{"a": "1", "c": "1"},
			other: []Change{{Op: OpPut, Path: "c"}}, mine: []Change{{Op: OpRename, From: "a", Path: "b"}}, ok: true},
		{name: "flat keys are opaque", layout: policy.LayoutFlat, initial: map[string]string{"d/a": "1"},
			other: []Change{{Op: OpDelete, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "d/b"}}, ok: true},
		{name: "flat, same key", layout: policy.LayoutFlat, initial: map[string]string{"d/a": "1"},
			other: []Change{{Op: OpPut, Path: "d/a"}}, mine: []Change{{Op: OpPut, Path: "d/a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := policy.Default()
			if tc.layout != "" {
				cfg.Layout = tc.layout
			}
			r := newRepo(t, cfg)
			parent := put(t, r, "main", tc.initial)
			fill := func(cs []Change, data string) []Change {
				out := append([]Change(nil), cs...)
				for i := range out {
					if out[i].Op == OpPut {
						out[i].Entry = fileEntry(t, r, data+out[i].Path)
					}
				}
				return out
			}
			head, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &parent, Changes: fill(tc.other, "other "), Author: alice})
			if err != nil {
				t.Fatal(err)
			}
			if tc.then != nil {
				if head, err = r.Commit(ctx, CommitRequest{Ref: "main", Parent: &head, Changes: fill(tc.then, "then "), Author: alice}); err != nil {
					t.Fatal(err)
				}
			}
			mine := fill(tc.mine, "mine ")
			h, base, err := rebased(r, parent, mine...)
			if !tc.ok {
				var ce *ConflictError
				if !errors.As(err, &ce) || ce.Current != head || len(ce.Paths) == 0 {
					t.Fatalf("got %v, want a conflict with paths", err)
				}
				if r.Ref("main") != head {
					t.Fatal("ref moved")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if base != head || r.Ref("main") != h {
				t.Fatalf("base %s, head %s", base.Short(), head.Short())
			}
			// The result is the head with my changes applied.
			want, err := r.commitTree(ctx, head)
			if err != nil {
				t.Fatal(err)
			}
			if want, err = r.applyChanges(ctx, want, mine); err != nil {
				t.Fatal(err)
			}
			if got, _ := r.commitTree(ctx, h); got != want {
				t.Fatal("rebased tree differs from head + changes")
			}
			c, _ := r.ReadCommit(ctx, h)
			if len(c.Parents) != 1 || c.Parents[0] != head {
				t.Fatalf("parents = %v", c.Parents)
			}
		})
	}
}

func TestRebaseDetails(t *testing.T) {
	r := newRepo(t, policy.Default())
	c1 := put(t, r, "main", map[string]string{"a": "1", "b": "1"})

	// Without Rebase, a moved head is a conflict as before.
	c2 := put(t, r, "main", map[string]string{"a": "2"})
	e := fileEntry(t, r, "b2")
	if _, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &c1, Changes: []Change{{Op: OpPut, Path: "b", Entry: e}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("strict commit on a moved head: %v", err)
	}
	// With Rebase and an unmoved head, nothing is rebased.
	h, base, err := rebased(r, c2, Change{Op: OpPut, Path: "b", Entry: e})
	if err != nil || base != c2 {
		t.Fatalf("base = %s, %v", base.Short(), err)
	}
	// An attrs-only change is a change.
	withCT := e
	withCT.Attrs = &object.Attrs{ContentType: "text/plain"}
	if _, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &h, Changes: []Change{{Op: OpPut, Path: "b", Entry: withCT}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rebased(r, h, Change{Op: OpDelete, Path: "b"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebase over an attrs change: %v", err)
	}
	// A ref that must be new is never rebased.
	zero := hash.Zero
	if _, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &zero, Rebase: true, Changes: []Change{{Op: OpDelete, Path: "a"}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebase of a new ref: %v", err)
	}
	// The ref log records the rebase.
	head := r.Ref("main")
	if _, _, err := rebased(r, c1, Change{Op: OpPut, Path: "c", Entry: e}); err != nil {
		t.Fatal(err)
	}
	var last repodir.LogRecord
	if err := r.dir.ReadLog(func(rec repodir.LogRecord) error { last = rec; return nil }); err != nil {
		t.Fatal(err)
	}
	if last.Reason != "commit (rebased)" || last.Old != head {
		t.Fatalf("last ref log record = %+v", last)
	}
}

// Writers on disjoint paths all succeed; on one path, one per round does.
func TestConcurrentRebase(t *testing.T) {
	r := newRepo(t, policy.Default())
	base := put(t, r, "main", map[string]string{"shared": "x"})
	const workers = rebaseAttempts
	entries := make([]object.Entry, workers)
	for i := range entries {
		entries[i] = fileEntry(t, r, fmt.Sprint("worker ", i))
	}
	for round := range 10 {
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for w := range workers {
			wg.Go(func() {
				_, _, errs[w] = rebased(r, base, Change{Op: OpPut, Path: fmt.Sprintf("r%d/w%d", round, w), Entry: entries[w]})
			})
		}
		wg.Wait()
		for w, err := range errs {
			if err != nil {
				t.Fatalf("round %d, worker %d: %v", round, w, err)
			}
		}
	}
	for round := range 10 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		head := r.Ref("main")
		// Distinct content per round: rewriting the current content would
		// be a no-op that lets another writer through.
		values := make([]object.Entry, workers)
		for w := range values {
			values[w] = fileEntry(t, r, fmt.Sprint("round ", round, " worker ", w))
		}
		for w := range workers {
			wg.Go(func() {
				_, _, err := rebased(r, head, Change{Op: OpPut, Path: "shared", Entry: values[w]})
				switch {
				case err == nil:
					mu.Lock()
					wins++
					mu.Unlock()
				case !errors.Is(err, ErrConflict):
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d winners on one path", round, wins)
		}
	}
	n := 0
	if err := r.List(ctx, r.Ref("main"), ListOpts{}, func(ListItem) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1+10*workers {
		t.Fatalf("%d files, want %d", n, 1+10*workers)
	}
}
