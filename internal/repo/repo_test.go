package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repodir"
)

var ctx = context.Background()

var alice = Person{Name: "Alice", Email: "alice@example.com"}

func newRepo(t *testing.T, cfg policy.Config) *Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ns")
	if err := Create(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(ctx) })
	return r
}

func flatConfig() policy.Config {
	c := policy.Default()
	c.Layout = policy.LayoutFlat
	return c
}

func fileEntry(t testing.TB, r *Repo, data string) object.Entry {
	t.Helper()
	res, err := r.Ingest(ctx, strings.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return object.Entry{Kind: object.KindFile, Target: res.File}
}

func put(t testing.TB, r *Repo, ref string, files map[string]string) hash.Hash {
	t.Helper()
	parent := r.Ref(ref)
	var changes []Change
	for p, data := range files {
		changes = append(changes, Change{Op: OpPut, Path: p, Entry: fileEntry(t, r, data)})
	}
	h, err := r.Commit(ctx, CommitRequest{Ref: ref, Parent: &parent, Changes: changes, Author: alice, Message: "put"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func read(t testing.TB, r *Repo, commit hash.Hash, path string) string {
	t.Helper()
	rd, _, err := r.Open(ctx, commit, path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCommitAndRead(t *testing.T) {
	r := newRepo(t, policy.Default())
	c1 := put(t, r, "main", map[string]string{"docs/readme.md": "hello", "src/main.go": "package main"})
	if got := read(t, r, c1, "docs/readme.md"); got != "hello" {
		t.Fatalf("read = %q", got)
	}
	c2 := put(t, r, "main", map[string]string{"docs/readme.md": "hello v2"})
	if r.Ref("main") != c2 || r.Ref("heads/main") != c2 {
		t.Fatal("ref not updated")
	}
	if got := read(t, r, c1, "docs/readme.md"); got != "hello" {
		t.Fatal("old commit changed")
	}
	if got := read(t, r, c2, "src/main.go"); got != "package main" {
		t.Fatal("unchanged file lost")
	}
	if _, err := r.Stat(ctx, c2, "docs/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat missing: %v", err)
	}
	if _, _, err := r.Open(ctx, c2, "docs"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Open dir: %v", err)
	}
	h, err := r.ResolveRef(ctx, c1.String())
	if err != nil || h != c1 {
		t.Fatalf("ResolveRef(hash) = %v, %v", h, err)
	}

	var changed []string
	if err := r.Diff(ctx, c1, c2, "", func(c PathChange) error {
		changed = append(changed, c.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(changed) != "[docs/readme.md]" {
		t.Fatalf("Diff = %v", changed)
	}
	log, err := r.Log(ctx, c2, "", 0)
	if err != nil || len(log) != 2 || log[0].Hash != c2 {
		t.Fatalf("Log = %v, %v", log, err)
	}
	log, err = r.Log(ctx, c2, "src/main.go", 0)
	if err != nil || len(log) != 1 || log[0].Hash != c1 {
		t.Fatalf("Log(path) = %d commits, %v", len(log), err)
	}
}

func TestCAS(t *testing.T) {
	r := newRepo(t, policy.Default())
	c1 := put(t, r, "main", map[string]string{"a": "1"})
	stale := hash.Zero
	_, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &stale, Changes: []Change{{Op: OpDelete, Path: "a"}}})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != c1 || !errors.Is(err, ErrConflict) {
		t.Fatalf("stale commit: %v", err)
	}
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "tags/v1", New: c1}, {Ref: "main", Old: c1, New: hash.Zero}}, UpdateOptions{Principal: "test", Reason: "atomic"}); err != nil {
		t.Fatal(err)
	}
	if r.Ref("main") != hash.Zero || r.Ref("tags/v1") != c1 {
		t.Fatalf("refs = %v", r.Refs())
	}
	// An atomic update with one stale ref changes nothing.
	err = r.UpdateRefs(ctx, []RefUpdate{{Ref: "heads/x", New: c1}, {Ref: "tags/v1", Old: hash.Zero, New: c1}}, UpdateOptions{Principal: "test", Reason: "atomic"})
	if !errors.Is(err, ErrConflict) || r.Ref("heads/x") != hash.Zero {
		t.Fatalf("partial atomic update: %v, refs %v", err, r.Refs())
	}
	missing := object.ChunkHash([]byte("nope"))
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "heads/y", New: missing}}, UpdateOptions{Principal: "", Reason: ""}); err == nil {
		t.Fatal("ref to a missing object accepted")
	}
}

func TestTreeLayoutRules(t *testing.T) {
	r := newRepo(t, policy.Default())
	put(t, r, "main", map[string]string{"a": "file", "d/x": "1", "d/sub/y": "2"})
	commit := func(changes ...Change) error {
		p := r.Ref("main")
		_, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &p, Changes: changes, Author: alice})
		return err
	}
	f := fileEntry(t, r, "x")
	if err := commit(Change{Op: OpPut, Path: "a/b", Entry: f}); !errors.Is(err, ErrPathConflict) {
		t.Errorf("file under a file: %v", err)
	}
	if err := commit(Change{Op: OpPut, Path: "d", Entry: f}); !errors.Is(err, ErrPathConflict) {
		t.Errorf("file over a directory: %v", err)
	}
	if err := commit(Change{Op: OpDelete, Path: "d"}); !errors.Is(err, ErrIsDir) {
		t.Errorf("delete a directory: %v", err)
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "a/./b", "../a"} {
		if err := commit(Change{Op: OpPut, Path: bad, Entry: f}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q: %v", bad, err)
		}
	}
	// Replacing a file by a directory in one commit is allowed.
	if err := commit(Change{Op: OpDelete, Path: "a"}, Change{Op: OpPut, Path: "a/b", Entry: f}); err != nil {
		t.Fatal(err)
	}
	// Deleting the last file of a directory removes the directory.
	if err := commit(Change{Op: OpDelete, Path: "d/sub/y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stat(ctx, r.Ref("main"), "d/sub"); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty directory not pruned: %v", err)
	}
	// Renames of files and directories.
	if err := commit(Change{Op: OpRename, From: "d/x", Path: "e/x2"}, Change{Op: OpRename, From: "a", Path: "z/a"}); err != nil {
		t.Fatal(err)
	}
	head := r.Ref("main")
	if got := read(t, r, head, "e/x2"); got != "1" {
		t.Errorf("renamed file = %q", got)
	}
	if got := read(t, r, head, "z/a/b"); got != "x" {
		t.Errorf("file in renamed directory = %q", got)
	}
	for _, gone := range []string{"d", "a"} {
		if _, err := r.Stat(ctx, head, gone); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s still exists: %v", gone, err)
		}
	}
	// Deleting a missing path is a no-op.
	if err := commit(Change{Op: OpDelete, Path: "no/such/file"}); err != nil {
		t.Fatal(err)
	}
}

// List returns paths in full-path byte order, for both layouts, with
// prefix, after, delimiter and limit, compared with a naive implementation.
func TestListMatchesNaive(t *testing.T) {
	for _, layout := range []string{policy.LayoutTree, policy.LayoutFlat} {
		t.Run(layout, func(t *testing.T) {
			cfg := policy.Default()
			cfg.Layout = layout
			r := newRepo(t, cfg)
			rng := rand.New(rand.NewPCG(3, 4))
			comps := []string{"a", "a!", "a-b", "a.txt", "a0", "b", "ab", "z~"}
			set := map[string]bool{}
			for range 300 {
				depth := 1 + rng.IntN(4)
				parts := make([]string, depth)
				for i := range parts {
					parts[i] = comps[rng.IntN(len(comps))]
				}
				p := strings.Join(parts, "/")
				// In tree layout, skip paths that conflict with existing ones.
				ok := true
				for q := range set {
					if strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") {
						ok = false
					}
				}
				if ok || layout == policy.LayoutFlat {
					set[p] = true
				}
			}
			files := map[string]string{}
			for p := range set {
				files[p] = p
			}
			head := put(t, r, "main", files)
			all := make([]string, 0, len(set))
			for p := range set {
				all = append(all, p)
			}
			sort.Strings(all)

			for i := range 400 {
				opts := ListOpts{Limit: rng.IntN(30)}
				if rng.IntN(2) == 0 {
					opts.Delimiter = "/"
				}
				if rng.IntN(3) > 0 {
					p := all[rng.IntN(len(all))]
					opts.Prefix = p[:rng.IntN(len(p)+1)]
				}
				if rng.IntN(2) == 0 {
					p := all[rng.IntN(len(all))]
					opts.After = p[:rng.IntN(len(p)+1)]
				}
				want := naiveList(all, opts)
				var got []string
				err := r.List(ctx, head, opts, func(it ListItem) error {
					s := it.Path
					if it.IsPrefix {
						s += " (prefix)"
					}
					got = append(got, s)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("case %d %+v:\n got  %v\n want %v", i, opts, got, want)
				}
			}
		})
	}
}

func naiveList(sorted []string, o ListOpts) []string {
	var out []string
	last := ""
	for _, p := range sorted {
		if !strings.HasPrefix(p, o.Prefix) {
			continue
		}
		item := p
		if o.Delimiter != "" {
			if i := strings.Index(p[len(o.Prefix):], o.Delimiter); i >= 0 {
				cp := p[:len(o.Prefix)+i+1]
				if cp == last {
					continue
				}
				last = cp
				item = cp + " (prefix)"
				if cp <= o.After {
					continue
				}
				out = append(out, item)
				if o.Limit > 0 && len(out) == o.Limit {
					return out
				}
				continue
			}
		}
		if p <= o.After {
			continue
		}
		out = append(out, item)
		if o.Limit > 0 && len(out) == o.Limit {
			return out
		}
	}
	return out
}

func TestMergeBase(t *testing.T) {
	r := newRepo(t, policy.Default())
	base := put(t, r, "main", map[string]string{"f": "0"})
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "feature", New: base}}, UpdateOptions{Principal: "", Reason: "branch"}); err != nil {
		t.Fatal(err)
	}
	m := put(t, r, "main", map[string]string{"f": "main"})
	f := put(t, r, "feature", map[string]string{"g": "feature"})
	mb, err := r.MergeBase(ctx, m, f)
	if err != nil || mb != base {
		t.Fatalf("MergeBase = %v, %v; want %v", mb, err, base)
	}
	if mb, _ := r.MergeBase(ctx, m, base); mb != base {
		t.Fatal("MergeBase with an ancestor")
	}
}

// N goroutines race on one ref with the same expected parent: exactly one
// wins each round.
func TestConcurrentCAS(t *testing.T) {
	r := newRepo(t, policy.Default())
	put(t, r, "main", map[string]string{"init": "x"})
	const workers, rounds = 8, 20
	entries := make([]object.Entry, workers)
	for i := range entries {
		entries[i] = fileEntry(t, r, fmt.Sprint("worker ", i))
	}
	for round := range rounds {
		head := r.Ref("main")
		var wins atomic.Int32
		var wg sync.WaitGroup
		for w := range workers {
			wg.Go(func() {
				_, err := r.Commit(ctx, CommitRequest{Ref: "main", Parent: &head, Author: alice,
					Changes: []Change{{Op: OpPut, Path: fmt.Sprintf("r%d/w%d", round, w), Entry: entries[w]}}})
				switch {
				case err == nil:
					wins.Add(1)
				case !errors.Is(err, ErrConflict):
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("round %d: %d winners", round, wins.Load())
		}
	}
}

// Concurrent submissions are batched into few commits and none is lost;
// preconditions see earlier changes of the same batch.
func TestGroupCommit(t *testing.T) {
	cfg, _ := policy.Preset("s3")
	r := newRepo(t, cfg)
	const n = 300
	var wg sync.WaitGroup
	commits := map[hash.Hash]bool{}
	var mu sync.Mutex
	for i := range n {
		wg.Go(func() {
			e := fileEntry(t, r, fmt.Sprint("object ", i))
			h, err := r.Submit(ctx, SubmitRequest{Ref: "main", Change: Change{Op: OpPut, Path: fmt.Sprintf("k%04d", i), Entry: e}, Author: alice})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			commits[h] = true
			mu.Unlock()
		})
	}
	wg.Wait()
	head := r.Ref("main")
	count := 0
	if err := r.List(ctx, head, ListOpts{}, func(ListItem) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("%d objects after %d puts", count, n)
	}
	t.Logf("%d puts in %d commits", n, len(commits))
	if len(commits) >= n/2 {
		t.Errorf("%d commits for %d concurrent puts: not batched", len(commits), n)
	}

	// If-None-Match: * — only the first of several creators succeeds.
	ifNoneMatch := func(cur *object.Entry) error {
		if cur != nil {
			return errors.New("precondition failed")
		}
		return nil
	}
	var ok, failed atomic.Int32
	for i := range 10 {
		wg.Go(func() {
			e := fileEntry(t, r, fmt.Sprint("creator ", i))
			_, err := r.Submit(ctx, SubmitRequest{Ref: "main", Change: Change{Op: OpPut, Path: "only-once", Entry: e}, Precondition: ifNoneMatch})
			if err == nil {
				ok.Add(1)
			} else {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 || failed.Load() != 9 {
		t.Fatalf("conditional creates: %d ok, %d failed", ok.Load(), failed.Load())
	}
}

// One invalid change in a batch fails only its own request.
func TestGroupCommitIsolatesErrors(t *testing.T) {
	r := newRepo(t, policy.Default())
	put(t, r, "main", map[string]string{"file": "x"})
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Go(func() {
			e := fileEntry(t, r, fmt.Sprint(i))
			p := fmt.Sprintf("ok/%d", i)
			if i == 7 {
				p = "file/under-a-file"
			}
			_, errs[i] = r.Submit(ctx, SubmitRequest{Ref: "main", Change: Change{Op: OpPut, Path: p, Entry: e}})
		})
	}
	wg.Wait()
	for i, err := range errs {
		if (i == 7) != (err != nil) {
			t.Errorf("request %d: %v", i, err)
		}
	}
}

func TestOfflineCommandsRefuseOpenRepo(t *testing.T) {
	r := newRepo(t, policy.Default())
	if _, err := gc.CheckDir(ctx, r.Path()); !errors.Is(err, repodir.ErrLocked) {
		t.Fatalf("fsck on an open repository: %v", err)
	}
}

func TestFsck(t *testing.T) {
	r := newRepo(t, policy.Default())
	for i := range 5 {
		put(t, r, "main", map[string]string{fmt.Sprintf("d%d/f", i): strings.Repeat(fmt.Sprint(i), 100000)})
	}
	path := r.Path()
	r.Close(ctx)
	rep, err := gc.CheckDir(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() || rep.Reachable == 0 || rep.Objects == 0 {
		t.Fatalf("fsck of a healthy repository: %+v", rep)
	}
	// Corrupt one byte of a pack.
	packs, _ := filepath.Glob(filepath.Join(path, repodir.PacksDir, "*.pack"))
	data, _ := os.ReadFile(packs[0])
	data[len(data)/2] ^= 0xff
	os.WriteFile(packs[0], data, 0o600)
	rep, err = gc.CheckDir(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatalf("fsck missed a corrupt pack: %+v", rep.Findings)
	}
}

func TestReopenAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns")
	if err := Create(path, flatConfig()); err != nil {
		t.Fatal(err)
	}
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := put(t, r, "main", map[string]string{"x/y": "data"})
	r.Close(ctx)
	r, _, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if r.Ref("main") != c || read(t, r, c, "x/y") != "data" {
		t.Fatal("state lost across reopen")
	}
	if bytes.Contains([]byte(r.Meta().UUID), []byte(" ")) {
		t.Fatal("bad uuid")
	}
}

// BenchmarkSmallPuts measures group-committed small puts with 64 concurrent
// writers (target: >= 500 puts/s per namespace on SSD).
func BenchmarkSmallPuts(b *testing.B) {
	cfg, _ := policy.Preset("s3")
	path := filepath.Join(b.TempDir(), "ns")
	if err := Create(path, cfg); err != nil {
		b.Fatal(err)
	}
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close(ctx)
	var next atomic.Int64
	b.SetParallelism(64 / max(1, runtime.GOMAXPROCS(0)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next.Add(1)
			data := fmt.Sprintf("small object %d", i)
			res, err := r.Ingest(ctx, strings.NewReader(data), int64(len(data)))
			if err != nil {
				b.Fatal(err)
			}
			_, err = r.Submit(ctx, SubmitRequest{Ref: "main", Change: Change{Op: OpPut, Path: fmt.Sprintf("bucket/key-%08d", i),
				Entry: object.Entry{Kind: object.KindFile, Target: res.File}}})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "puts/s")
}
