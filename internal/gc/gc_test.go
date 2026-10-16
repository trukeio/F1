package gc_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/store"
	"github.com/trukeio/f1/internal/testutil"
)

var ctx = context.Background()

// noGrace collects everything unreachable at once.
var noGrace = gc.Options{Grace: -1}

type testRepo struct {
	t    *testing.T
	path string
	r    *repo.Repo
}

func newRepo(t *testing.T, retention string) *testRepo {
	t.Helper()
	cfg := policy.Default()
	cfg.Retention = retention
	cfg.Locks = "off"
	tr := &testRepo{t: t, path: filepath.Join(t.TempDir(), "ns")}
	if err := repo.Create(tr.path, cfg); err != nil {
		t.Fatal(err)
	}
	tr.open()
	t.Cleanup(func() {
		if tr.r != nil {
			tr.r.Close(ctx)
		}
	})
	return tr
}

func (tr *testRepo) open() {
	tr.t.Helper()
	// Replaced packs are deleted at once, so that sizes are measurable.
	r, _, err := repo.Open(ctx, tr.path, repo.Options{Store: store.Options{DeleteGrace: -1}})
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.r = r
}

// fsck closes the repository, checks it and reopens it.
func (tr *testRepo) fsck() *gc.Report {
	tr.t.Helper()
	if err := tr.r.Close(ctx); err != nil {
		tr.t.Fatal(err)
	}
	tr.r = nil
	rep, err := gc.CheckDir(ctx, tr.path)
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.open()
	return rep
}

func (tr *testRepo) ingest(data []byte) object.Entry {
	tr.t.Helper()
	res, err := tr.r.Ingest(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		tr.t.Fatal(err)
	}
	return object.Entry{Kind: object.KindFile, Target: res.File}
}

// put commits files on main and returns the commit.
func (tr *testRepo) put(files map[string][]byte) (hash.Hash, error) {
	parent := tr.r.Ref("main")
	var changes []repo.Change
	for p, d := range files {
		res, err := tr.r.Ingest(ctx, bytes.NewReader(d), int64(len(d)))
		if err != nil {
			return hash.Zero, err
		}
		changes = append(changes, repo.Change{Op: repo.OpPut, Path: p, Entry: object.Entry{Kind: object.KindFile, Target: res.File}})
	}
	return tr.r.Commit(ctx, repo.CommitRequest{Ref: "main", Parent: &parent, Changes: changes, Author: repo.Person{Name: "t", Email: "t"}})
}

func (tr *testRepo) mustPut(files map[string][]byte) hash.Hash {
	tr.t.Helper()
	h, err := tr.put(files)
	if err != nil {
		tr.t.Fatal(err)
	}
	return h
}

func (tr *testRepo) read(commit hash.Hash, path string) ([]byte, error) {
	rd, _, err := tr.r.Open(ctx, commit, path)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(rd)
}

func (tr *testRepo) mustRead(commit hash.Hash, path string, want []byte) {
	tr.t.Helper()
	got, err := tr.read(commit, path)
	if err != nil || !bytes.Equal(got, want) {
		tr.t.Fatalf("%s at %s: %d bytes, %v (want %d bytes)", path, commit.Short(), len(got), err, len(want))
	}
}

func (tr *testRepo) gc(opts gc.Options) *gc.Result {
	tr.t.Helper()
	res, err := gc.Run(ctx, tr.r, opts)
	if err != nil {
		tr.t.Fatal(err)
	}
	return res
}

func data(seed string, n int) []byte { return testutil.Bytes(seed, n) }

// Unreachable objects are deleted; reachable ones and the whole history
// (retention = all) stay, and fsck is clean.
func TestGCRetentionAll(t *testing.T) {
	tr := newRepo(t, "all")
	v1 := data("v1", 300<<10)
	c1 := tr.mustPut(map[string][]byte{"a.bin": v1, "b.txt": []byte("b")})
	c2 := tr.mustPut(map[string][]byte{"a.bin": data("v2", 300<<10)})
	orphan := tr.ingest(data("never committed", 200<<10))
	tr.r.Store().Flush(ctx)

	// Within the grace period nothing is deleted.
	if res := tr.gc(gc.Options{}); res.DeletedObjects != 0 {
		t.Fatalf("deleted %d objects within the grace period", res.DeletedObjects)
	}
	res := tr.gc(noGrace)
	if res.DeletedObjects == 0 || res.Retained != 2 {
		t.Fatalf("result %+v", res)
	}
	if ok, _ := tr.r.Store().Has(ctx, orphan.Target); ok {
		t.Fatal("orphan File survived")
	}
	tr.mustRead(c1, "a.bin", v1)
	tr.mustRead(c2, "b.txt", []byte("b"))
	if rep := tr.fsck(); !rep.Clean() {
		t.Fatalf("fsck: %v", rep.Findings)
	}
	// A second collection deletes nothing more: what it condemns again
	// is left in mostly live packs.
	if res := tr.gc(noGrace); res.DeletedObjects != 0 || res.Condemned != res.Kept {
		t.Fatalf("second gc: %+v", res)
	}
}

// Truncating retention keeps the policy's commits; fsck accepts the
// missing parents of the oldest retained commits.
func TestGCRetentionTruncating(t *testing.T) {
	for _, tc := range []struct {
		retention string
		kept      int
	}{{"none", 1}, {"last:3", 3}, {"days:1", 1}} {
		t.Run(tc.retention, func(t *testing.T) {
			tr := newRepo(t, tc.retention)
			var commits []hash.Hash
			for i := range 6 {
				commits = append(commits, tr.mustPut(map[string][]byte{"f": data(fmt.Sprint("version ", i), 100<<10)}))
			}
			if tc.retention == "days:1" {
				// Every commit is from today: all are kept, until two
				// days later only the head is.
				if res := tr.gc(noGrace); res.Retained != 6 {
					t.Fatalf("days:1 retained %d commits made today", res.Retained)
				}
				opts := noGrace
				opts.Now = time.Now().Add(48 * time.Hour)
				if res := tr.gc(opts); res.Retained != 1 {
					t.Fatalf("days:1 two days later retained %d", res.Retained)
				}
				tc.kept = 1
			} else if res := tr.gc(noGrace); res.Retained != tc.kept {
				t.Fatalf("retained %d, want %d", res.Retained, tc.kept)
			}
			for i, c := range commits {
				_, err := tr.r.ReadCommit(ctx, c)
				if kept := i >= len(commits)-tc.kept; kept != (err == nil) {
					t.Fatalf("commit %d: kept %v, read error %v", i, kept, err)
				}
			}
			tr.mustRead(commits[5], "f", data("version 5", 100<<10))
			rep := tr.fsck()
			if !rep.OK() {
				t.Fatalf("fsck: %v", rep.Findings)
			}
			log, err := tr.r.Log(ctx, commits[5], "", 100)
			if err != nil || len(log) != tc.kept {
				t.Fatalf("log after truncation: %d commits, %v", len(log), err)
			}
		})
	}
}

// Deterministic race: with the collection paused between condemn and
// sweep, a write that deduplicates onto a condemned (unreachable, old)
// chunk stores a fresh copy; after the sweep the file is readable.
func TestGCWriteDuringSweepWindow(t *testing.T) {
	tr := newRepo(t, "none")
	old := data("reused content", 600<<10) // several chunks
	tr.mustPut(map[string][]byte{"f": old})
	tr.mustPut(map[string][]byte{"f": data("other", 10)}) // old is unreachable now
	tr.r.Store().Flush(ctx)

	var commit hash.Hash
	var writeErr error
	opts := noGrace
	opts.AfterCondemn = func() {
		commit, writeErr = tr.put(map[string][]byte{"g": old})
	}
	res := tr.gc(opts)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if res.DeletedObjects == 0 {
		t.Fatalf("nothing deleted: %+v", res)
	}
	tr.mustRead(commit, "g", old)
	if rep := tr.fsck(); !rep.OK() {
		t.Fatalf("fsck: %v", rep.Findings)
	}
	tr.mustRead(commit, "g", old)
}

// The touched set: an object found stored during the epoch, and what it
// references, is not condemned.
func TestGCTouchedSet(t *testing.T) {
	tr := newRepo(t, "none")
	old := data("touched", 600<<10)
	c1 := tr.mustPut(map[string][]byte{"f": old})
	tr.mustPut(map[string][]byte{"f": data("other", 10)})
	tr.r.Store().Flush(ctx)
	// Re-point a branch at the old, unreachable commit during the epoch:
	// UpdateRefs checks that the commit is stored (touching it), and the
	// collection keeps the commit's whole tree.
	hr := &hookRepo{Repo: tr.r, onRefs: func() {
		if err := tr.r.UpdateRefs(ctx, []repo.RefUpdate{{Ref: "heads/old", New: c1}}, repo.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}}
	if _, err := gc.Run(ctx, hr, noGrace); err != nil {
		t.Fatal(err)
	}
	tr.mustRead(c1, "f", old)
	if rep := tr.fsck(); !rep.OK() {
		t.Fatalf("fsck: %v", rep.Findings)
	}
}

// hookRepo gives the collection the refs as they were when it first read
// them, then calls onRefs once (after the epoch began, before condemn).
type hookRepo struct {
	*repo.Repo
	onRefs func()
	refs   map[string]hash.Hash
}

func (h *hookRepo) Refs() map[string]hash.Hash {
	if h.refs == nil {
		h.refs = h.Repo.Refs()
		h.onRefs()
	}
	return h.refs
}

// A frozen or held repository is skipped; an archive taken during a
// collection stays valid.
func TestGCSkipsHeld(t *testing.T) {
	tr := newRepo(t, "none")
	tr.mustPut(map[string][]byte{"f": data("1", 100<<10)})
	tr.mustPut(map[string][]byte{"f": data("2", 100<<10)})
	if err := tr.r.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if res := tr.gc(noGrace); res.Skipped == "" || res.DeletedObjects != 0 {
		t.Fatalf("frozen: %+v", res)
	}
	tr.r.Unfreeze()
	release := tr.r.Store().Hold()
	if res := tr.gc(noGrace); res.Skipped == "" {
		t.Fatalf("held: %+v", res)
	}
	release()
	// An archive started while the collection is paused before its sweep:
	// the hold waits for the sweep, then the archive lists the packs.
	var buf bytes.Buffer
	archDone := make(chan error, 1)
	opts := noGrace
	opts.AfterCondemn = func() {
		go func() {
			_, err := tr.r.Archive(ctx, &buf, "ns")
			archDone <- err
		}()
		time.Sleep(50 * time.Millisecond)
	}
	res := tr.gc(opts)
	if err := <-archDone; err != nil {
		t.Fatal(err)
	}
	if res.DeletedObjects == 0 {
		t.Fatalf("collection with a pending archive: %+v", res)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	packs := 0
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".pack") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		p, err := pack.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if err := p.Verify(); err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		packs++
	}
	if packs == 0 {
		t.Fatal("archive has no packs")
	}
}

// A dry run reports exactly what the next collection deletes.
func TestGCDryRun(t *testing.T) {
	tr := newRepo(t, "last:2")
	for i := range 5 {
		tr.mustPut(map[string][]byte{"f": data(fmt.Sprint(i), 200<<10), "g": []byte(fmt.Sprint(i))})
	}
	tr.ingest(data("orphan", 50<<10))
	tr.r.Store().Flush(ctx)
	opts := noGrace
	opts.DryRun = true
	dry := tr.gc(opts)
	if dry.DeletedObjects == 0 {
		t.Fatal("dry run found nothing")
	}
	again := tr.gc(opts)
	real := tr.gc(noGrace)
	if !slices.Equal(dry.Deleted, again.Deleted) || !slices.Equal(dry.Deleted, real.Deleted) ||
		!slices.Equal(dry.PacksDeleted, real.PacksDeleted) || !slices.Equal(dry.PacksRewritten, real.PacksRewritten) {
		t.Fatalf("dry run %d objects, packs %v/%v; real %d objects, packs %v/%v", len(dry.Deleted), dry.PacksDeleted, dry.PacksRewritten,
			len(real.Deleted), real.PacksDeleted, real.PacksRewritten)
	}
}

// A commit based on a parent that the collection removed (the head moved
// on, retention = none) is a conflict, as the compare-and-swap would report,
// not a missing object.
func TestGCStaleParentConflict(t *testing.T) {
	tr := newRepo(t, "none")
	old := tr.mustPut(map[string][]byte{"k": data("old", 100<<10)})
	tr.r.Store().Flush(ctx) // the old version in a pack of its own
	tr.mustPut(map[string][]byte{"k": data("new", 100<<10)})
	tr.r.Store().Flush(ctx)
	if res := tr.gc(noGrace); res.DeletedObjects == 0 {
		t.Fatalf("nothing collected: %+v", res)
	}
	if _, err := tr.r.ReadCommit(ctx, old); err == nil {
		t.Fatal("the old head is still stored")
	}
	res, err := tr.r.Ingest(ctx, bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.r.Commit(ctx, repo.CommitRequest{Ref: "main", Parent: &old, Author: repo.Person{Name: "t", Email: "t"},
		Changes: []repo.Change{{Op: repo.OpPut, Path: "c", Entry: object.Entry{Kind: object.KindFile, Target: res.File}}}})
	var ce *repo.ConflictError
	if !errors.As(err, &ce) || ce.Current != tr.r.Ref("main") {
		t.Fatalf("commit on a collected parent: %v, want a conflict", err)
	}
}

// GC under concurrent write load, with writes that reuse old chunks,
// loses no acknowledged data.
func TestGCConcurrentWrites(t *testing.T) {
	tr := newRepo(t, "none")
	contents := make([][]byte, 8)
	for i := range contents {
		contents[i] = data(fmt.Sprint("content ", i), 100<<10+i)
	}
	// History that becomes garbage.
	for i := range 20 {
		tr.mustPut(map[string][]byte{"k": contents[i%len(contents)]})
	}
	var stop atomic.Bool
	type ack struct {
		commit hash.Hash
		path   string
		data   []byte
	}
	var mu sync.Mutex
	var acks []ack
	var failed atomic.Int64
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				d := contents[(w+i)%len(contents)]
				p := fmt.Sprintf("w%d/f%d", w, i%5)
				c, err := tr.put(map[string][]byte{p: d})
				if err != nil {
					// Only a lost CAS race or a reference the collection
					// condemned under the writer may fail; neither was
					// acknowledged.
					var ce *repo.ConflictError
					if !errors.As(err, &ce) && !errors.Is(err, store.ErrMissingReference) {
						t.Error(err)
						return
					}
					failed.Add(1)
					continue
				}
				mu.Lock()
				acks = append(acks, ack{c, p, d})
				mu.Unlock()
			}
		})
	}
	for range 5 {
		tr.gc(noGrace)
		time.Sleep(20 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if len(acks) == 0 {
		t.Fatal("no write succeeded")
	}
	t.Logf("%d acknowledged writes, %d failed", len(acks), failed.Load())
	// The newest acknowledged version of every path is readable at the
	// head (retention = none keeps only the head commit).
	head := tr.r.Ref("main")
	latest := map[string][]byte{}
	for _, a := range acks {
		latest[a.path] = a.data
	}
	for p, d := range latest {
		got, err := tr.read(head, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		// Another writer's later commit may hold another version of
		// this path; every version is one of the contents.
		if !slices.ContainsFunc(contents, func(c []byte) bool { return bytes.Equal(c, got) }) {
			t.Fatalf("%s: unknown content (%d bytes, wanted like %d)", p, len(got), len(d))
		}
	}
	tr.gc(noGrace)
	for p := range latest {
		if _, err := tr.read(head, p); err != nil {
			t.Fatalf("%s after a final gc: %v", p, err)
		}
	}
	if rep := tr.fsck(); !rep.OK() {
		t.Fatalf("fsck: %v", rep.Findings)
	}
}

// retention = none with 1,000,000 overwrites of 1,000 keys: after GC the
// storage is within 2× the live data.
func TestGCOverwrites(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a million versions")
	}
	keys, rounds := 1000, 1000
	if raceEnabled {
		rounds = 10
	}
	cfg := policy.Default()
	cfg.Retention = "none"
	cfg.Layout = policy.LayoutFlat
	cfg.Locks = "off"
	tr := &testRepo{t: t, path: filepath.Join(t.TempDir(), "ns")}
	if err := repo.Create(tr.path, cfg); err != nil {
		t.Fatal(err)
	}
	tr.open()
	defer func() { tr.r.Close(ctx) }()
	for round := range rounds {
		parent := tr.r.Ref("main")
		changes := make([]repo.Change, keys)
		for k := range keys {
			d := []byte(fmt.Sprintf("key %d round %d %s", k, round, strings.Repeat("x", 64)))
			res, err := tr.r.Ingest(ctx, bytes.NewReader(d), int64(len(d)))
			if err != nil {
				t.Fatal(err)
			}
			changes[k] = repo.Change{Op: repo.OpPut, Path: fmt.Sprintf("key-%04d", k), Entry: object.Entry{Kind: object.KindFile, Target: res.File}}
		}
		if _, err := tr.r.Commit(ctx, repo.CommitRequest{Ref: "main", Parent: &parent, Changes: changes, Author: repo.Person{Name: "t"}}); err != nil {
			t.Fatal(err)
		}
	}
	before := packBytes(t, tr.r)
	res := tr.gc(noGrace)
	after := packBytes(t, tr.r)
	live := liveBytes(t, tr.r)
	t.Logf("%d overwrites: %d MiB before GC, %d KiB after, %d KiB live; %d objects deleted in %s",
		keys*rounds, before>>20, after>>10, live>>10, res.DeletedObjects, res.Duration)
	if after > 2*live {
		t.Fatalf("storage %d bytes after GC, live data %d", after, live)
	}
	head := tr.r.Ref("main")
	got, err := tr.read(head, "key-0999")
	if err != nil || !strings.HasPrefix(string(got), fmt.Sprintf("key 999 round %d ", rounds-1)) {
		t.Fatalf("latest version: %q, %v", got, err)
	}
}

// packBytes is the size of the indexed packs.
func packBytes(t *testing.T, r *repo.Repo) int64 {
	t.Helper()
	packs, err := r.Store().Cache().Packs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, p := range packs {
		n += p.Size
	}
	return n
}

// liveBytes is the stored size of the objects reachable from the head
// commit, with the DigestRecords of its Files.
func liveBytes(t *testing.T, r *repo.Repo) int64 {
	t.Helper()
	c := r.Store().Cache()
	seen := map[hash.Hash]bool{}
	var n int64
	stack := []hash.Hash{r.Ref("main")}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		loc, ok, err := c.Lookup(ctx, h)
		if err != nil || !ok {
			t.Fatalf("live object %s: %v", h.Short(), err)
		}
		n += int64(loc.Entry.Stored) + 32 // record header
		if loc.Entry.Type == object.TypeChunk {
			continue
		}
		rec, err := r.Store().Get(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := rec.Payload(nil)
		o, err := object.Decode(rec.Type, payload)
		if err != nil {
			t.Fatal(err)
		}
		refs := object.References(o)
		if co, ok := o.(*object.Commit); ok {
			refs = []hash.Hash{co.Tree}
		}
		if _, ok := o.(*object.File); ok {
			if d, ok, _ := c.Digest(ctx, h); ok {
				dh, _, _ := object.Hash(&d)
				refs = append(refs, dh)
			}
		}
		stack = append(stack, refs...)
	}
	return n
}

// fsck in a last:N namespace accepts the missing parents of the oldest
// retained commits, and reports any other missing object.
func TestFsckTruncatedHistory(t *testing.T) {
	tr := newRepo(t, "last:2")
	for i := range 4 {
		tr.mustPut(map[string][]byte{"f": data(fmt.Sprint(i), 50<<10)})
	}
	tr.gc(noGrace)
	if rep := tr.fsck(); !rep.Clean() {
		t.Fatalf("fsck after truncation: %v", rep.Findings)
	}
	// Add a pack with a commit whose tree is missing (and whose parent is
	// missing, which is allowed here).
	c := &object.Commit{Tree: hash.Plain([]byte("missing tree")), Parents: []hash.Hash{hash.Plain([]byte("missing parent"))},
		Author: object.Person{Name: []byte("t"), Email: []byte("t"), TZ: "+0000"}, Committer: object.Person{Name: []byte("t"), Email: []byte("t"), TZ: "+0000"}}
	h, b, err := object.Hash(c)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := pack.NewRecord(object.TypeCommit, h, b, nil)
	var buf bytes.Buffer
	w, _ := pack.NewWriter(&buf)
	w.Add(rec)
	w.Finish()
	tr.r.Close(ctx)
	tr.r = nil
	if err := os.WriteFile(filepath.Join(tr.path, "packs", ulid.Make().String()+".pack"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := gc.CheckDir(ctx, tr.path)
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, f := range rep.Findings {
		if f.Severity == gc.Error {
			errs = append(errs, f.Message)
		}
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "references missing object "+c.Tree.Short()) {
		t.Fatalf("fsck errors: %v", errs)
	}
	tr.open()
}
