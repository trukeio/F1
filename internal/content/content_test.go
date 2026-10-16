package content

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/trukeio/f1/internal/backend"
	"github.com/trukeio/f1/internal/chunk"
	f1hash "github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/testutil"
)

// memStore is an in-memory Store that counts Gets per object type and
// checks the D9 ordering of Puts.
type memStore struct {
	t    testing.TB
	mu   sync.Mutex
	recs map[f1hash.Hash]pack.Record
	gets map[object.Type]int
}

func newMemStore(t testing.TB) *memStore {
	return &memStore{t: t, recs: map[f1hash.Hash]pack.Record{}, gets: map[object.Type]int{}}
}

func (s *memStore) Has(_ context.Context, h f1hash.Hash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recs[h]
	return ok, nil
}

func (s *memStore) Put(_ context.Context, rec pack.Record) error {
	payload, err := rec.Payload(nil)
	if err != nil {
		return err
	}
	// D9: every referenced object must already be stored.
	var refs []object.Ref
	switch rec.Type {
	case object.TypeFile:
		f, err := object.DecodeAs[*object.File](payload)
		if err != nil {
			return err
		}
		refs = f.Refs
	case object.TypeFileIndex:
		x, err := object.DecodeAs[*object.FileIndex](payload)
		if err != nil {
			return err
		}
		refs = x.Refs
	case object.TypeDigestRecord:
		d, err := object.DecodeAs[*object.DigestRecord](payload)
		if err != nil {
			return err
		}
		refs = []object.Ref{{Hash: d.File}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range refs {
		if _, ok := s.recs[r.Hash]; !ok {
			s.t.Errorf("D9 violated: %s %s stored before %s", rec.Type, rec.Hash.Short(), r.Hash.Short())
		}
	}
	rec.Data = bytes.Clone(rec.Data)
	s.recs[rec.Hash] = rec
	return nil
}

func (s *memStore) Get(_ context.Context, h f1hash.Hash) (pack.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[h]
	if !ok {
		return pack.Record{}, fmt.Errorf("not found: %s", h.Short())
	}
	s.gets[rec.Type]++
	return rec, nil
}

func (s *memStore) getsOf(t object.Type) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[t]
}

func (s *memStore) resetGets() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = map[object.Type]int{}
}

func (s *memStore) count(t object.Type) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.recs {
		if r.Type == t {
			n++
		}
	}
	return n
}

func ingest(t testing.TB, s Store, data []byte, hint int64) Result {
	t.Helper()
	res, err := Ingest(context.Background(), s, bytes.NewReader(data), hint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readAll(t testing.TB, s Store, h f1hash.Hash) []byte {
	t.Helper()
	r, err := Open(context.Background(), s, h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wantDigests(data []byte) Digests {
	var d Digests
	g := sha1.New()
	fmt.Fprintf(g, "blob %d\x00", len(data))
	g.Write(data)
	copy(d.SHA1Git[:], g.Sum(nil))
	d.SHA256 = sha256.Sum256(data)
	d.MD5 = md5.Sum(data)
	return d
}

func TestIngestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, chunk.MinSize - 1, chunk.MinSize, chunk.MaxSize + 1, 3<<20 + 17}
	for _, size := range sizes {
		for _, hint := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/hint=%v", size, hint), func(t *testing.T) {
				s := newMemStore(t)
				data := testutil.Bytes(fmt.Sprint("content ", size), size)
				h := int64(-1)
				if hint {
					h = int64(size)
				}
				res := ingest(t, s, data, h)
				if res.Size != uint64(size) {
					t.Errorf("Size = %d", res.Size)
				}
				if res.Digests != wantDigests(data) {
					t.Error("digests differ")
				}
				if got := readAll(t, s, res.File); !bytes.Equal(got, data) {
					t.Fatal("content differs")
				}
				if n := s.count(object.TypeDigestRecord); n != 1 {
					t.Errorf("%d digest records, want 1", n)
				}
			})
		}
	}
}

// The Git blob SHA-1 of the empty blob is a well-known constant.
func TestEmptyBlobSHA1(t *testing.T) {
	res := ingest(t, newMemStore(t), nil, 0)
	if got := fmt.Sprintf("%x", res.Digests.SHA1Git); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty blob SHA-1 = %s", got)
	}
}

func TestIngestDedup(t *testing.T) {
	s := newMemStore(t)
	data := testutil.Bytes("dedup", 2<<20)
	a := ingest(t, s, data, int64(len(data)))
	b := ingest(t, s, data, -1)
	if a.File != b.File {
		t.Fatal("same content produced different File hashes")
	}
	if b.NewChunks != 0 {
		t.Fatalf("second ingest stored %d new chunks", b.NewChunks)
	}
	// Repeated content within one file is stored once.
	rep := bytes.Repeat(testutil.Bytes("unit", chunk.MaxSize), 8)
	c := ingest(t, newMemStore(t), rep, -1)
	if c.NewChunks >= c.Chunks {
		t.Fatalf("%d of %d chunks stored for repeated content", c.NewChunks, c.Chunks)
	}
}

func TestIngestEdit(t *testing.T) {
	s := newMemStore(t)
	base := testutil.Bytes("edit", 8<<20)
	ingest(t, s, base, -1)
	edited := append(append(append([]byte{}, base[:4<<20]...), "inserted"...), base[4<<20:]...)
	res := ingest(t, s, edited, -1)
	if res.NewChunks > 3 {
		t.Fatalf("edit stored %d new chunks, want at most 3", res.NewChunks)
	}
}

func TestIngestSizeMismatch(t *testing.T) {
	for _, size := range []int64{2, 4} { // more and fewer bytes than declared
		_, err := Ingest(context.Background(), newMemStore(t), bytes.NewReader([]byte("abc")), size, Options{})
		if !errors.Is(err, ErrSizeMismatch) {
			t.Fatalf("size %d: err = %v", size, err)
		}
	}
}

func TestIngestReadError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader(testutil.Bytes("x", 3<<20)), iotest.ErrReader(boom))
	_, err := Ingest(context.Background(), newMemStore(t), r, -1, Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

type failingStore struct {
	*memStore
	after int
	n     int
}

func (s *failingStore) Put(ctx context.Context, rec pack.Record) error {
	s.n++
	if s.n > s.after {
		return errors.New("disk full")
	}
	return s.memStore.Put(ctx, rec)
}

func TestIngestStoreError(t *testing.T) {
	s := &failingStore{memStore: newMemStore(t), after: 5}
	_, err := Ingest(context.Background(), s, bytes.NewReader(testutil.Bytes("y", 4<<20)), -1, Options{})
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v", err)
	}
}

func TestIngestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Ingest(ctx, newMemStore(t), bytes.NewReader(testutil.Bytes("z", 4<<20)), -1, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// buildIndexed stores a File of n small chunks, so that FileIndex levels
// can be tested without gigabytes of data.
func buildIndexed(t *testing.T, s *memStore, n int) (*object.File, []byte) {
	t.Helper()
	ctx := context.Background()
	var content []byte
	b := object.NewFileBuilder(func(x *object.FileIndex) (f1hash.Hash, error) {
		return putObject(ctx, s, x)
	})
	for i := range n {
		c := fmt.Appendf(nil, "chunk %06d|", i)
		c = append(c, testutil.Bytes(fmt.Sprint(i), i%50)...)
		h := object.ChunkHash(c)
		rec, err := pack.NewRecord(object.TypeChunk, h, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if err := b.Add(object.Ref{Hash: h, Size: uint64(len(c))}); err != nil {
			t.Fatal(err)
		}
		content = append(content, c...)
	}
	f, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return f, content
}

func TestReaderIndexed(t *testing.T) {
	s := newMemStore(t)
	f, want := buildIndexed(t, s, 20000)
	if f.Level == 0 {
		t.Fatal("expected an indexed file")
	}
	got, err := io.ReadAll(OpenFile(context.Background(), s, f))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("content differs")
	}
}

// Random-offset reads fetch and decompress only the chunks that overlap the
// requested range, plus the FileIndex nodes on their path.
func TestReaderRandomAccess(t *testing.T) {
	s := newMemStore(t)
	f, content := buildIndexed(t, s, 20000)
	refs := chunkRefs(f, s)
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		off := rng.IntN(len(content))
		n := min(rng.IntN(300)+1, len(content)-off)
		r := OpenFile(context.Background(), s, f)
		s.resetGets()
		buf := make([]byte, n)
		if _, err := r.ReadAt(buf, int64(off)); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, content[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d) returned wrong bytes", off, n)
		}
		want := overlapping(refs, uint64(off), uint64(n))
		if got := s.getsOf(object.TypeChunk); got != want {
			t.Fatalf("ReadAt(%d, %d) fetched %d chunks, %d overlap", off, n, got, want)
		}
		if got := s.getsOf(object.TypeFileIndex); got > int(f.Level)*2 {
			t.Fatalf("ReadAt fetched %d file index nodes for a level-%d file", got, f.Level)
		}
	}
}

// chunkRefs expands a File into its chunk list.
func chunkRefs(f *object.File, s *memStore) []object.Ref {
	var refs []object.Ref
	var expand func(level uint64, rs []object.Ref)
	expand = func(level uint64, rs []object.Ref) {
		for _, r := range rs {
			if level == 0 {
				refs = append(refs, r)
				continue
			}
			p, _ := s.recs[r.Hash].Payload(nil)
			x, _ := object.DecodeAs[*object.FileIndex](p)
			expand(level-1, x.Refs)
		}
	}
	expand(f.Level, f.Refs)
	return refs
}

// overlapping counts the chunks overlapping [off, off+n).
func overlapping(refs []object.Ref, off, n uint64) int {
	count := 0
	var pos uint64
	for _, r := range refs {
		if pos < off+n && pos+r.Size > off {
			count++
		}
		pos += r.Size
	}
	return count
}

func TestReaderSeek(t *testing.T) {
	s := newMemStore(t)
	data := testutil.Bytes("seek", 1<<20)
	res := ingest(t, s, data, -1)
	r, err := Open(context.Background(), s, res.File)
	if err != nil {
		t.Fatal(err)
	}
	if err := iotest.TestReader(r, data); err != nil {
		t.Fatal(err)
	}
	if pos, err := r.Seek(-10, io.SeekEnd); err != nil || pos != int64(len(data)-10) {
		t.Fatalf("Seek = %d, %v", pos, err)
	}
	tail, _ := io.ReadAll(r)
	if !bytes.Equal(tail, data[len(data)-10:]) {
		t.Fatal("tail differs")
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
}

// packStore writes records into one pack file and reads them back from
// memory; it stands in for the phase 2 pack store in benchmarks.
type packStore struct {
	mu   sync.Mutex
	w    *pack.Writer
	f    *os.File
	seen map[f1hash.Hash]bool
}

func (s *packStore) Has(_ context.Context, h f1hash.Hash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[h], nil
}

func (s *packStore) Put(_ context.Context, rec pack.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[rec.Hash] = true
	_, err := s.w.Add(rec)
	return err
}

func (s *packStore) Get(context.Context, f1hash.Hash) (pack.Record, error) {
	return pack.Record{}, errors.New("not supported")
}

// BenchmarkIngest measures single-stream ingest into a pack file that is
// then sealed into the fs backend, with every digest computed inline.
func BenchmarkIngest(b *testing.B) {
	// Half random, half text-like, so compression does real work.
	data := testutil.Bytes("bench", 256<<20)
	text := bytes.Repeat([]byte("lorem ipsum dolor sit amet, consectetur adipiscing elit "), 1<<20/56+1)
	for off := 0; off+len(text) <= len(data); off += 2 << 20 {
		copy(data[off:], text[:1<<20])
	}
	dir := b.TempDir()
	be, err := backend.NewFS(filepath.Join(dir, "repo"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	i := 0
	for b.Loop() {
		f, err := os.Create(filepath.Join(dir, "open.pack"))
		if err != nil {
			b.Fatal(err)
		}
		w, err := pack.NewWriter(f)
		if err != nil {
			b.Fatal(err)
		}
		s := &packStore{w: w, f: f, seen: map[f1hash.Hash]bool{}}
		if _, err := Ingest(context.Background(), s, bytes.NewReader(data), int64(len(data)), Options{}); err != nil {
			b.Fatal(err)
		}
		if _, err := w.Finish(); err != nil {
			b.Fatal(err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			b.Fatal(err)
		}
		if err := be.Put(context.Background(), fmt.Sprintf("packs/%d.pack", i), f, -1); err != nil {
			b.Fatal(err)
		}
		f.Close()
		i++
	}
}

// FileHash computes the same File hash as Ingest.
func TestFileHash(t *testing.T) {
	for _, size := range []int{0, 1, chunk.MinSize, chunk.MaxSize + 1, smallFile, smallFile + 1, 3<<20 + 17} {
		data := testutil.Bytes(fmt.Sprint("hash ", size), size)
		res := ingest(t, newMemStore(t), data, int64(size))
		h, n, err := FileHash(bytes.NewReader(data))
		if err != nil || h != res.File || n != uint64(size) {
			t.Fatalf("size %d: FileHash = %s, %d, %v; Ingest = %s", size, h.Short(), n, err, res.File.Short())
		}
	}
}
