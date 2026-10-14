package repo

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

func commitAs(t *testing.T, r *Repo, ref, principal string, files map[string]string) error {
	t.Helper()
	parent := r.Ref(ref)
	var changes []Change
	for p, d := range files {
		changes = append(changes, Change{Op: OpPut, Path: p, Entry: fileEntry(t, r, d)})
	}
	_, err := r.Commit(ctx, CommitRequest{Ref: ref, Parent: &parent, Changes: changes, Author: alice, Principal: principal})
	return err
}

func TestRequiredLocks(t *testing.T) {
	cfg := policy.Default()
	cfg.Locks = "required:*.sch,hw/*.brd"
	r := newRepo(t, cfg)
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"notes.txt": "x"}); err != nil {
		t.Fatalf("unlocked path: %v", err)
	}
	var le *LockedError
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"deep/dir/top.sch": "x"}); !errors.As(err, &le) || le.Owner != "" {
		t.Fatalf("required lock missing: %v", err)
	}
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"hw/board.brd": "x"}); !errors.As(err, &le) {
		t.Fatalf("required lock with a path glob: %v", err)
	}
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"other/board.brd": "x"}); err != nil {
		t.Fatalf("path glob matched outside its directory: %v", err)
	}
	if _, err := r.LockPath(ctx, "main", "deep/dir/top.sch", "user:alice", 0); err != nil {
		t.Fatal(err)
	}
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"deep/dir/top.sch": "x"}); err != nil {
		t.Fatalf("with the lock: %v", err)
	}
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"deep/dir/top.sch": "y"}); !errors.As(err, &le) || le.Owner != "user:alice" {
		t.Fatalf("another principal: %v", err)
	}
}

func TestExclusiveLocks(t *testing.T) {
	r := newRepo(t, policy.Default()) // locks = exclusive
	commitAs(t, r, "main", "user:alice", map[string]string{"a.psd": "1", "b.txt": "1"})
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"a.psd": "2"}); err != nil {
		t.Fatalf("unlocked path: %v", err)
	}
	l, err := r.LockPath(ctx, "main", "a.psd", "user:alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LockPath(ctx, "main", "a.psd", "user:bob", 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second lock: %v", err)
	}
	var le *LockedError
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"a.psd": "3"}); !errors.As(err, &le) {
		t.Fatalf("other principal on a locked path: %v", err)
	}
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"b.txt": "3"}); err != nil {
		t.Fatalf("other path: %v", err)
	}
	if err := commitAs(t, r, "main", "user:alice", map[string]string{"a.psd": "4"}); err != nil {
		t.Fatalf("owner: %v", err)
	}
	// A deletion of a locked path is a change too.
	parent := r.Ref("main")
	if _, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &parent, Principal: "user:bob", Changes: []Change{{Op: OpDelete, Path: "a.psd"}}}); !errors.As(err, &le) {
		t.Fatalf("delete of a locked path: %v", err)
	}

	// A new branch whose commits touch a path locked on the default ref is
	// checked against its merge base with main.
	base := r.Ref("main")
	e := fileEntry(t, r, "branch edit")
	c := &object.Commit{Tree: hash.Zero, Parents: []hash.Hash{base}}
	root, err := r.applyChanges(ctx, mustTree(t, r, base), []Change{{Op: OpPut, Path: "a.psd", Entry: e}})
	if err != nil {
		t.Fatal(err)
	}
	c.Tree = root
	h, err := r.putObject(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "feature", New: h}}, UpdateOptions{Principal: "user:bob"}); !errors.As(err, &le) {
		t.Fatalf("new branch touching a locked path: %v", err)
	}
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "feature", New: h}}, UpdateOptions{Principal: "user:alice"}); err != nil {
		t.Fatalf("new branch by the lock owner: %v", err)
	}

	// Unlock: only the owner, or with force.
	if _, err := r.Unlock(ctx, l.ID, "user:bob", false); !errors.Is(err, ErrNotLockOwner) {
		t.Fatalf("unlock by another principal: %v", err)
	}
	if _, err := r.Unlock(ctx, l.ID, "user:bob", true); err != nil {
		t.Fatalf("forced unlock: %v", err)
	}
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"a.psd": "5"}); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func mustTree(t *testing.T, r *Repo, commit hash.Hash) hash.Hash {
	t.Helper()
	root, err := r.commitTree(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLockExpiryAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns")
	if err := Create(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	commitAs(t, r, "main", "user:alice", map[string]string{"f": "1"})
	if _, err := r.LockPath(ctx, "main", "f", "user:alice", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LockPath(ctx, "main", "g", "user:alice", 0); err != nil {
		t.Fatal(err)
	}
	r.Close(ctx)
	r, _, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if n := len(r.Locks("main", "")); n != 2 {
		t.Fatalf("%d locks after reopen, want 2", n)
	}
	// The TTL leaves room for a slow reopen under load.
	time.Sleep(1100 * time.Millisecond)
	if locks := r.Locks("main", ""); len(locks) != 1 || locks[0].Path != "g" {
		t.Fatalf("locks after expiry: %+v", locks)
	}
	if err := commitAs(t, r, "main", "user:bob", map[string]string{"f": "2"}); err != nil {
		t.Fatalf("commit after the lock expired: %v", err)
	}
	if _, err := r.LockPath(ctx, "main", "f", "user:bob", 0); err != nil {
		t.Fatalf("locking an expired lock's path: %v", err)
	}
}

// Group commit checks locks per request, with each request's principal.
func TestGroupCommitLocks(t *testing.T) {
	r := newRepo(t, policy.Default())
	commitAs(t, r, "main", "user:alice", map[string]string{"locked": "1"})
	if _, err := r.LockPath(ctx, "main", "locked", "user:alice", 0); err != nil {
		t.Fatal(err)
	}
	type res struct {
		who string
		err error
	}
	out := make(chan res, 3)
	for _, who := range []string{"user:alice", "user:bob"} {
		go func() {
			_, err := r.Submit(ctx, SubmitRequest{Ref: "main", Principal: who, Change: Change{Op: OpPut, Path: "locked", Entry: fileEntry(t, r, who)}})
			out <- res{who, err}
		}()
	}
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Ref: "main", Principal: "user:bob", Change: Change{Op: OpPut, Path: "free", Entry: fileEntry(t, r, "bob")}})
		out <- res{"user:bob free", err}
	}()
	for range 3 {
		got := <-out
		var le *LockedError
		switch got.who {
		case "user:bob":
			if !errors.As(got.err, &le) {
				t.Errorf("bob on a locked path: %v", got.err)
			}
		default:
			if got.err != nil {
				t.Errorf("%s: %v", got.who, got.err)
			}
		}
	}
}
