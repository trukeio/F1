package repo

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

// commitAt commits changes with a given committer time.
func commitAt(t *testing.T, r *Repo, at time.Time, changes ...Change) hash.Hash {
	t.Helper()
	parent := r.Ref("main")
	h, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &parent, Changes: changes, Author: alice, Message: "c", Time: at})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHistoryAndLastModified(t *testing.T) {
	for _, cfg := range []policy.Config{policy.Default(), flatConfig()} {
		t.Run(cfg.Layout, func(t *testing.T) {
			r := newRepo(t, cfg)
			t0 := time.Unix(1_700_000_000, 0)
			at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Hour) }
			putC := func(p, data string) Change { return Change{Op: OpPut, Path: p, Entry: fileEntry(t, r, data)} }
			c1 := commitAt(t, r, at(1), putC("a/x", "1"), putC("a/y", "1"), putC("b", "1"))
			c2 := commitAt(t, r, at(2), putC("a/x", "2"))
			c3 := commitAt(t, r, at(3), Change{Op: OpDelete, Path: "a/y"}, putC("c", "1"))

			var got []string
			if err := r.History(ctx, c3, "a/", func(v Version) error {
				kind := "put"
				if v.New == nil {
					kind = "delete"
				}
				got = append(got, fmt.Sprintf("%s %s %s", v.Commit.Short(), kind, v.Path))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := []string{c3.Short() + " delete a/y", c2.Short() + " put a/x", c1.Short() + " put a/x", c1.Short() + " put a/y"}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}

			lm, err := r.LastModified(ctx, c3, []string{"a/x", "b", "c", "a/y", "nope"})
			if err != nil {
				t.Fatal(err)
			}
			if !lm["a/x"].Equal(at(2)) || !lm["b"].Equal(at(1)) || !lm["c"].Equal(at(3)) || len(lm) != 3 {
				t.Fatalf("LastModified = %v", lm)
			}
			// At an older commit.
			if lm, err = r.LastModified(ctx, c1, []string{"a/x"}); err != nil || !lm["a/x"].Equal(at(1)) {
				t.Fatalf("LastModified at c1 = %v, %v", lm, err)
			}
		})
	}
}

// Concat joins Files by chunk list; the result reads as the concatenated
// content, and Digests computes and stores its DigestRecord on demand.
func TestConcatAndDigests(t *testing.T) {
	r := newRepo(t, flatConfig())
	parts := []string{strings.Repeat("first part ", 30000), "", "short", strings.Repeat("third ", 70000)}
	var files []hash.Hash
	for _, p := range parts {
		res, err := r.Ingest(ctx, strings.NewReader(p), int64(len(p)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, res.File)
	}
	file, size, err := r.Concat(ctx, files)
	if err != nil {
		t.Fatal(err)
	}
	whole := strings.Join(parts, "")
	if size != uint64(len(whole)) {
		t.Fatalf("size %d, want %d", size, len(whole))
	}
	commitAt(t, r, time.Now(), Change{Op: OpPut, Path: "joined", Entry: fileEntryFor(file)})
	if got := read(t, r, r.Ref("main"), "joined"); got != whole {
		t.Fatalf("content differs: %d bytes", len(got))
	}
	if _, ok, _ := r.Store().Cache().Digest(ctx, file); ok {
		t.Fatal("concatenated File has a DigestRecord before Digests")
	}
	d, err := r.Digests(ctx, file)
	if err != nil || d.SHA256 != sha256.Sum256([]byte(whole)) {
		t.Fatalf("digests = %x, %v", d.SHA256, err)
	}
	if _, ok, _ := r.Store().Cache().Digest(ctx, file); !ok {
		t.Fatal("Digests did not store the DigestRecord")
	}
	// DigestLater on another File completes before Close.
	f2, _, err := r.Concat(ctx, files[2:])
	if err != nil {
		t.Fatal(err)
	}
	r.DigestLater(f2)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok, _ := r.Store().Cache().Digest(ctx, f2); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background digest did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fileEntryFor(file hash.Hash) object.Entry {
	return object.Entry{Kind: object.KindFile, Target: file}
}
