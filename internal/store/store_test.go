package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/backend"
	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/testutil"
)

var ctx = context.Background()

func newDir(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ns")
	if err := repodir.Create(p, policy.Default()); err != nil {
		t.Fatal(err)
	}
	return p
}

type opened struct {
	dir *repodir.Dir
	s   *Store
	rep OpenReport
}

func open(t *testing.T, path string, opts Options) opened {
	t.Helper()
	d, err := repodir.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := Open(ctx, d, opts)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	return opened{d, s, rep}
}

func (o opened) close(t *testing.T) {
	t.Helper()
	if err := o.s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	o.dir.Close()
}

func chunkRecord(t testing.TB, data []byte) pack.Record {
	t.Helper()
	rec, err := pack.NewRecord(object.TypeChunk, object.ChunkHash(data), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func objRecord(t testing.TB, o object.Object) pack.Record {
	t.Helper()
	h, b, err := object.Hash(o)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := pack.NewRecord(o.Type(), h, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func payload(t testing.TB, s *Store, h hash.Hash) []byte {
	t.Helper()
	rec, err := s.Get(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	p, err := rec.Payload(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPutGetFlushReopen(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	data := []byte("hello world")
	rec := chunkRecord(t, data)
	if err := o.s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if ok, _ := o.s.Has(ctx, rec.Hash); !ok {
		t.Fatal("Has is false for an object in the open pack")
	}
	if got := payload(t, o.s, rec.Hash); !bytes.Equal(got, data) {
		t.Fatal("Get from the open pack returned wrong data")
	}
	if err := o.s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(path, repodir.PacksDir)); len(entries) != 1 {
		t.Fatalf("%d pack files after flush, want 1", len(entries))
	}
	if got := payload(t, o.s, rec.Hash); !bytes.Equal(got, data) {
		t.Fatal("Get from a sealed pack returned wrong data")
	}
	o.close(t)

	o = open(t, path, Options{})
	defer o.close(t)
	if o.rep.Rebuilt || len(o.rep.Indexed) != 0 {
		t.Fatalf("reopen report = %+v", o.rep)
	}
	if got := payload(t, o.s, rec.Hash); !bytes.Equal(got, data) {
		t.Fatal("Get after reopen returned wrong data")
	}
	if _, err := o.s.Get(ctx, object.ChunkHash([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
}

func TestD9(t *testing.T) {
	o := open(t, newDir(t), Options{})
	defer o.close(t)
	missing := object.ChunkHash([]byte("not stored"))
	f := &object.File{Size: 10, Refs: []object.Ref{{Hash: missing, Size: 10}}}
	if err := o.s.Put(ctx, objRecord(t, f)); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("Put of File with a missing chunk: %v", err)
	}
	c := chunkRecord(t, []byte("0123456789"))
	if err := o.s.Put(ctx, c); err != nil {
		t.Fatal(err)
	}
	f.Refs[0].Hash = c.Hash
	if err := o.s.Put(ctx, objRecord(t, f)); err != nil {
		t.Fatalf("Put after storing the chunk: %v", err)
	}
}

func TestPackTargetSeals(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{PackTarget: 1 << 20})
	defer o.close(t)
	for i := range 40 {
		if err := o.s.Put(ctx, chunkRecord(t, testutil.Bytes(fmt.Sprint(i), 100<<10))); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(path, repodir.PacksDir))
	if len(entries) < 3 {
		t.Fatalf("%d sealed packs, want several", len(entries))
	}
}

// fill ingests n files and flushes after each, producing n small packs.
func fill(t *testing.T, s *Store, n int) []content.Result {
	t.Helper()
	var out []content.Result
	for i := range n {
		data := testutil.Bytes(fmt.Sprint("file ", i), 50<<10+i)
		res, err := content.Ingest(ctx, s, bytes.NewReader(data), int64(len(data)), content.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
	}
	return out
}

func checkReadable(t *testing.T, s *Store, files []content.Result) {
	t.Helper()
	for i, f := range files {
		r, err := content.Open(ctx, s, f.File)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, testutil.Bytes(fmt.Sprint("file ", i), 50<<10+i)) {
			t.Fatalf("file %d differs", i)
		}
		d, ok, err := s.Cache().Digest(ctx, f.File)
		if err != nil || !ok || d.SHA256 != f.Digests.SHA256 {
			t.Fatalf("digest of file %d: %v %v", i, ok, err)
		}
	}
}

// countingBackend records every ranged read.
type countingBackend struct {
	backend.Backend
	mu    sync.Mutex
	reads map[string][][2]int64
}

func (c *countingBackend) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	c.mu.Lock()
	c.reads[key] = append(c.reads[key], [2]int64{off, n})
	c.mu.Unlock()
	return c.Backend.Get(ctx, key, off, n)
}

// Deleting cache.db rebuilds it from pack footers and DigestRecords only:
// no chunk payload is read.
func TestCacheRebuildReadsNoChunks(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	files := fill(t, o.s, 5)
	o.close(t)
	if err := os.Remove(filepath.Join(path, repodir.CacheFile)); err != nil {
		t.Fatal(err)
	}

	fsb, _ := backend.NewFS(path)
	cb := &countingBackend{Backend: fsb, reads: map[string][][2]int64{}}
	d, err := repodir.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := Open(ctx, d, Options{Backend: cb})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Indexed) != 5 {
		t.Fatalf("report = %+v, want 5 packs indexed", rep)
	}
	// Every read during open must avoid chunk records.
	for key, ranges := range cb.reads {
		f, _ := os.Open(filepath.Join(path, filepath.FromSlash(key)))
		st, _ := f.Stat()
		p, err := pack.Open(f, st.Size())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Entries() {
			if e.Type != object.TypeChunk {
				continue
			}
			start, end := int64(e.Offset), int64(e.Offset)+pack.RecordHeaderSize+int64(e.Stored)
			for _, r := range ranges {
				if r[0] < end && r[0]+r[1] > start {
					t.Fatalf("open read chunk %s in %s", e.Hash.Short(), key)
				}
			}
		}
		f.Close()
	}
	checkReadable(t, s, files)
	s.Close(ctx)
	d.Close()
}

func TestCorruptCacheRebuilt(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	files := fill(t, o.s, 3)
	o.close(t)
	cachePath := filepath.Join(path, repodir.CacheFile)
	os.Remove(cachePath + "-wal")
	os.Remove(cachePath + "-shm")
	if err := os.WriteFile(cachePath, bytes.Repeat([]byte("garbage!"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	o = open(t, path, Options{})
	defer o.close(t)
	if !o.rep.Rebuilt || len(o.rep.Indexed) != 3 {
		t.Fatalf("report = %+v", o.rep)
	}
	checkReadable(t, o.s, files)
}

// A pack sealed into packs/ but not indexed (crash between the rename and
// the cache transaction) is indexed on open.
func TestUnindexedPackIndexed(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	files := fill(t, o.s, 2)
	o.close(t)

	var buf bytes.Buffer
	w, _ := pack.NewWriter(&buf)
	data := []byte("orphan pack content")
	w.Add(chunkRecord(t, data))
	w.Finish()
	if err := os.WriteFile(filepath.Join(path, repodir.PacksDir, "01ORPHAN.pack"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	o = open(t, path, Options{})
	defer o.close(t)
	if o.rep.Rebuilt || len(o.rep.Indexed) != 1 || o.rep.Indexed[0] != "01ORPHAN" {
		t.Fatalf("report = %+v", o.rep)
	}
	if got := payload(t, o.s, object.ChunkHash(data)); !bytes.Equal(got, data) {
		t.Fatal("orphan pack object not readable")
	}
	checkReadable(t, o.s, files)
}

// A pack that disappeared from packs/ forces a full rebuild.
func TestMissingPackForcesRebuild(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	fill(t, o.s, 3)
	o.close(t)
	entries, _ := os.ReadDir(filepath.Join(path, repodir.PacksDir))
	os.Remove(filepath.Join(path, repodir.PacksDir, entries[0].Name()))
	o = open(t, path, Options{})
	defer o.close(t)
	if !o.rep.Rebuilt || len(o.rep.Indexed) != 2 {
		t.Fatalf("report = %+v", o.rep)
	}
}

func TestConsolidation(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{SmallPackLimit: 1 << 30, DeleteGrace: -1})
	files := fill(t, o.s, 30)
	res, err := o.s.Consolidate(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Merged) != 0 {
		t.Fatal("consolidated below the limit")
	}
	res, err = o.s.Consolidate(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Merged) != 30 || len(res.Created) != 1 || len(res.Deleted) != 30 {
		t.Fatalf("consolidation = merged %d, created %d, deleted %d", len(res.Merged), len(res.Created), len(res.Deleted))
	}
	entries, _ := os.ReadDir(filepath.Join(path, repodir.PacksDir))
	if len(entries) != 1 {
		t.Fatalf("%d pack files after consolidation, want 1", len(entries))
	}
	checkReadable(t, o.s, files)
	o.close(t)

	o = open(t, path, Options{})
	defer o.close(t)
	if o.rep.Rebuilt || len(o.rep.Indexed) != 0 {
		t.Fatalf("reopen after consolidation: %+v", o.rep)
	}
	checkReadable(t, o.s, files)
}

// Automatic consolidation keeps the number of small packs bounded, and a
// grace period keeps replaced packs on disk until it passes.
func TestAutoConsolidation(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{SmallPackLimit: 8})
	files := fill(t, o.s, 40)
	// Seals trigger consolidation in the background; wait for the last run.
	for o.s.consolidating.Load() {
		time.Sleep(time.Millisecond)
	}
	pending, _ := o.s.Cache().PendingDeletes(ctx)
	if len(pending) == 0 {
		t.Fatal("no automatic consolidation, or replaced packs were not kept for the grace period")
	}
	// Seals skipped while a run was in progress may have left more small
	// packs than the limit; the next run brings them back under it.
	if _, err := o.s.Consolidate(ctx, false); err != nil {
		t.Fatal(err)
	}
	packs, err := o.s.Cache().Packs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) > 8+1 {
		t.Fatalf("%d indexed packs after auto consolidation, want at most 9", len(packs))
	}
	checkReadable(t, o.s, files)
	o.close(t)
	// Packs pending deletion are not re-indexed on open.
	o = open(t, path, Options{})
	defer o.close(t)
	if len(o.rep.Indexed) != 0 || o.rep.Rebuilt {
		t.Fatalf("reopen: %+v", o.rep)
	}
}

// A hold stops consolidation until released.
func TestHoldBlocksConsolidation(t *testing.T) {
	o := open(t, newDir(t), Options{SmallPackLimit: 1 << 30, DeleteGrace: -1})
	defer o.close(t)
	files := fill(t, o.s, 5)
	release := o.s.Hold()
	if !o.s.Held() {
		t.Fatal("Held is false")
	}
	res, err := o.s.Consolidate(ctx, true)
	if err != nil || len(res.Merged) != 0 {
		t.Fatalf("consolidation while held: %+v, %v", res, err)
	}
	release()
	release() // idempotent
	if res, err = o.s.Consolidate(ctx, true); err != nil || len(res.Merged) != 5 {
		t.Fatalf("consolidation after release: %+v, %v", res, err)
	}
	checkReadable(t, o.s, files)
}
