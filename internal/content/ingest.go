// Package content ingests file content into objects (chunks, FileIndex,
// File, DigestRecord) and reads it back with random access.
package content

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/trukeio/f1/internal/chunk"
	f1hash "github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

// Store is where Ingest writes objects and Reader reads them.
type Store interface {
	// Has reports whether the object is stored. It is called concurrently.
	Has(ctx context.Context, h f1hash.Hash) (bool, error)
	// Put stores a record. It must not retain rec.Data after returning.
	// Records arrive in dependency order: an object after everything it
	// references (D9).
	Put(ctx context.Context, rec pack.Record) error
	// Get returns the stored record for h.
	Get(ctx context.Context, h f1hash.Hash) (pack.Record, error)
}

// Digests are the external digests of a file's content.
type Digests struct {
	SHA1Git [20]byte // Git blob SHA-1
	SHA256  [32]byte
	MD5     [16]byte
}

// Result describes an ingested file.
type Result struct {
	File      f1hash.Hash // hash of the File object
	Size      uint64
	Digests   Digests
	Chunks    int // chunks in the file
	NewChunks int // chunks that were not stored before
}

// ErrSizeMismatch is returned when the content length differs from the
// size hint given to Ingest.
var ErrSizeMismatch = errors.New("content: size does not match the declared size")

// Options tune Ingest. The zero value uses defaults.
type Options struct {
	// Workers is the number of hashing and compression goroutines
	// (default GOMAXPROCS).
	Workers int
}

// Ingest chunks r, stores every new object and returns the File hash and
// digests. sizeHint is the content length if known, or -1. With a known
// size, every digest is computed in the same pass; otherwise the Git blob
// SHA-1, whose header needs the size, is computed by reading the stored
// content back once.
//
// Chunking, BLAKE3 hashing, zstd compression and the MD5, SHA-1 and SHA-256
// digests run concurrently. Memory use is bounded by a fixed number of
// in-flight chunks, independent of file size (D19).
func Ingest(ctx context.Context, store Store, r io.Reader, sizeHint int64, opts Options) (Result, error) {
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	inflight := 2*workers + 8

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	p := &pipeline{
		ctx:   ctx,
		fail:  cancel,
		store: store,
		bufs:  make(chan []byte, inflight),
		dsts:  make(chan []byte, inflight),
		jobs:  make(chan *job, inflight),
		order: make(chan *job, inflight),
	}
	for range inflight {
		p.bufs <- make([]byte, chunk.MaxSize)
		p.dsts <- make([]byte, 0, chunk.MaxSize)
	}

	digesters := []*digester{
		{h: sha256.New()},
		{h: md5.New()},
	}
	sha1Inline := sizeHint >= 0
	var gitSHA1 hash.Hash
	if sha1Inline {
		gitSHA1 = sha1.New()
		gitSHA1.Write(blobHeader(uint64(sizeHint)))
		digesters = append(digesters, &digester{h: gitSHA1})
	}
	for _, d := range digesters {
		d.in = make(chan *job, inflight)
	}

	var wg sync.WaitGroup
	var total atomic.Uint64
	wg.Go(func() { p.produce(r, digesters, &total) })
	for range workers {
		wg.Go(p.work)
	}
	for _, d := range digesters {
		wg.Go(func() { d.run(ctx, p) })
	}

	res, err := p.write()
	if err != nil {
		cancel(err)
	}
	wg.Wait()
	if err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		return Result{}, err
	}

	if sizeHint >= 0 && uint64(sizeHint) != total.Load() {
		return Result{}, fmt.Errorf("%w: got %d bytes, declared %d", ErrSizeMismatch, total.Load(), sizeHint)
	}
	copy(res.Digests.SHA256[:], digesters[0].h.Sum(nil))
	copy(res.Digests.MD5[:], digesters[1].h.Sum(nil))
	if sha1Inline {
		copy(res.Digests.SHA1Git[:], gitSHA1.Sum(nil))
	} else {
		sum, err := gitBlobSHA1(ctx, store, res.File, res.Size)
		if err != nil {
			return Result{}, err
		}
		res.Digests.SHA1Git = sum
	}

	dr := &object.DigestRecord{File: res.File, SHA1Git: res.Digests.SHA1Git, SHA256: res.Digests.SHA256, MD5: res.Digests.MD5}
	if _, err := putObject(ctx, store, dr); err != nil {
		return Result{}, err
	}
	return res, nil
}

func blobHeader(size uint64) []byte {
	return []byte("blob " + strconv.FormatUint(size, 10) + "\x00")
}

func gitBlobSHA1(ctx context.Context, store Store, file f1hash.Hash, size uint64) ([20]byte, error) {
	var sum [20]byte
	r, err := Open(ctx, store, file)
	if err != nil {
		return sum, err
	}
	h := sha1.New()
	h.Write(blobHeader(size))
	if _, err := io.Copy(h, r); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// putObject encodes and stores o unless it is already stored.
func putObject(ctx context.Context, store Store, o object.Object) (f1hash.Hash, error) {
	h, b, err := object.Hash(o)
	if err != nil {
		return f1hash.Zero, err
	}
	ok, err := store.Has(ctx, h)
	if err != nil || ok {
		return h, err
	}
	rec, err := pack.NewRecord(o.Type(), h, b, nil)
	if err != nil {
		return f1hash.Zero, err
	}
	return h, store.Put(ctx, rec)
}

type job struct {
	data []byte // chunk bytes in a pool buffer
	refs atomic.Int32
	done chan struct{}
	hash f1hash.Hash
	dup  bool
	rec  pack.Record
	err  error
}

type pipeline struct {
	ctx   context.Context
	fail  context.CancelCauseFunc
	store Store
	bufs  chan []byte // chunk buffers; bound the number of in-flight chunks
	dsts  chan []byte // record buffers
	jobs  chan *job   // to workers
	order chan *job   // to the writer, in file order
}

// release returns a job's chunk buffer to the pool when its last user is
// done.
func (p *pipeline) release(j *job) {
	if j.refs.Add(-1) == 0 {
		p.bufs <- j.data[:cap(j.data)]
	}
}

// produce chunks r and fans the chunks out. A read error cancels the
// pipeline before the channels are closed, so the writer never mistakes a
// failed read for the end of the input.
func (p *pipeline) produce(r io.Reader, digesters []*digester, total *atomic.Uint64) {
	defer close(p.jobs)
	defer close(p.order)
	defer func() {
		for _, d := range digesters {
			close(d.in)
		}
	}()
	c := chunk.New(r)
	for {
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			p.fail(fmt.Errorf("content: read: %w", err))
			return
		}
		total.Add(uint64(len(b)))
		var buf []byte
		select {
		case buf = <-p.bufs:
		case <-p.ctx.Done():
			return
		}
		j := &job{data: buf[:copy(buf, b)], done: make(chan struct{})}
		j.refs.Store(int32(1 + len(digesters)))
		if !send(p.ctx, p.order, j) || !send(p.ctx, p.jobs, j) {
			return
		}
		for _, d := range digesters {
			if !send(p.ctx, d.in, j) {
				return
			}
		}
	}
}

func send(ctx context.Context, ch chan<- *job, j *job) bool {
	select {
	case ch <- j:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *pipeline) work() {
	for j := range p.jobs {
		p.process(j)
		p.release(j)
		close(j.done)
	}
}

func (p *pipeline) process(j *job) {
	if p.ctx.Err() != nil {
		j.err = p.ctx.Err()
		return
	}
	j.hash = object.ChunkHash(j.data)
	ok, err := p.store.Has(p.ctx, j.hash)
	if err != nil {
		j.err = err
		return
	}
	if ok {
		j.dup = true
		return
	}
	var dst []byte
	select {
	case dst = <-p.dsts:
	case <-p.ctx.Done():
		j.err = p.ctx.Err()
		return
	}
	j.rec, j.err = pack.NewRecord(object.TypeChunk, j.hash, j.data, dst)
}

// write consumes jobs in file order, stores new chunks and builds the File.
func (p *pipeline) write() (Result, error) {
	var res Result
	builder := object.NewFileBuilder(func(x *object.FileIndex) (f1hash.Hash, error) {
		return putObject(p.ctx, p.store, x)
	})
	stored := map[f1hash.Hash]bool{}
	for j := range p.order {
		select {
		case <-j.done:
		case <-p.ctx.Done():
			return res, context.Cause(p.ctx)
		}
		if j.err != nil {
			return res, j.err
		}
		size := uint64(len(j.data))
		if !j.dup && !stored[j.hash] {
			if err := p.store.Put(p.ctx, j.rec); err != nil {
				return res, err
			}
			stored[j.hash] = true
			res.NewChunks++
		}
		if j.rec.Data != nil {
			p.dsts <- j.rec.Data[:0]
		}
		res.Chunks++
		res.Size += size
		if err := builder.Add(object.Ref{Hash: j.hash, Size: size}); err != nil {
			return res, err
		}
	}
	if err := p.ctx.Err(); err != nil {
		return res, context.Cause(p.ctx)
	}
	f, err := builder.Finish()
	if err != nil {
		return res, err
	}
	res.File, err = putObject(p.ctx, p.store, f)
	return res, err
}

type digester struct {
	h  hash.Hash
	in chan *job
}

func (d *digester) run(ctx context.Context, p *pipeline) {
	for j := range d.in {
		if ctx.Err() == nil {
			d.h.Write(j.data)
		}
		p.release(j)
	}
}
