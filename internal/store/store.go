// Package store is a repository's pack store: it writes objects into an
// open pack in tmp/, seals packs into packs/, indexes them in cache.db,
// reads objects back, and consolidates small packs.
//
// Every existence check (D9 checks, dedup during ingest, "missing objects"
// answers) goes through Store.Has, so that GC can later cooperate with
// writers in one place (doc/plan.md, "GC algorithm").
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trukeio/f1/internal/backend"
	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/repodir"
)

// Options tune a Store. Zero values use the defaults.
type Options struct {
	PackTarget     int64         // seal an open pack at this size (64 MiB)
	IdleSeal       time.Duration // seal an open pack after this idle time (2 s)
	SmallPack      int64         // packs below this size are "small" (8 MiB)
	SmallPackLimit int           // consolidate when more small packs exist (256)
	DeleteGrace    time.Duration // keep replaced packs this long for in-flight reads (1 h)
	Logger         *slog.Logger
	// Backend overrides the filesystem backend rooted at the repository
	// directory; tests use it to observe reads.
	Backend backend.Backend
}

func (o *Options) defaults() {
	if o.PackTarget <= 0 {
		o.PackTarget = 64 << 20
	}
	if o.IdleSeal <= 0 {
		o.IdleSeal = 2 * time.Second
	}
	if o.SmallPack <= 0 {
		o.SmallPack = 8 << 20
	}
	if o.SmallPackLimit <= 0 {
		o.SmallPackLimit = 256
	}
	if o.DeleteGrace < 0 {
		o.DeleteGrace = 0
	} else if o.DeleteGrace == 0 {
		o.DeleteGrace = time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// Store is a repository's pack store. It is safe for concurrent use.
type Store struct {
	dir   *repodir.Dir
	be    backend.Backend
	cache *cache.DB
	opts  Options
	log   *slog.Logger

	mu     sync.Mutex
	open   *openPack
	idle   *time.Timer
	closed bool

	maint         sync.Mutex  // serializes consolidation
	consolidating atomic.Bool // a background consolidation is running
}

type openPack struct {
	name    string
	path    string
	f       *os.File
	w       *pack.Writer
	index   map[hash.Hash]pack.IndexEntry
	digests []object.DigestRecord
}

// OpenReport describes what Open did to bring cache.db up to date.
type OpenReport struct {
	Rebuilt bool     // cache.db was rebuilt from scratch
	Reason  string   // why it was rebuilt
	Indexed []string // packs that were added to the index
}

// Open opens the pack store of an open repository directory, rebuilding or
// updating cache.db as needed.
func Open(ctx context.Context, dir *repodir.Dir, opts Options) (*Store, OpenReport, error) {
	opts.defaults()
	s := &Store{dir: dir, be: opts.Backend, opts: opts, log: opts.Logger}
	if s.be == nil {
		fsb, err := backend.NewFS(dir.Path())
		if err != nil {
			return nil, OpenReport{}, err
		}
		s.be = fsb
	}
	rep, err := s.openCache(ctx)
	if err != nil {
		return nil, rep, err
	}
	return s, rep, nil
}

func (s *Store) cachePath() string { return s.dir.Join(repodir.CacheFile) }

func (s *Store) openCache(ctx context.Context) (OpenReport, error) {
	var rep OpenReport
	onDisk, err := s.listPacks(ctx)
	if err != nil {
		return rep, err
	}
	// Use the existing cache if it opens and agrees with packs/: every
	// indexed pack must exist with the indexed size.
	var indexed []cache.PackInfo
	var pending map[string]bool
	c, err := cache.Open(s.cachePath())
	if err == nil {
		indexed, err = c.Packs(ctx)
		if err == nil {
			pending, err = c.PendingDeletes(ctx)
		}
		if err == nil {
			for _, p := range indexed {
				if size, ok := onDisk[p.Name]; !ok || size != p.Size {
					err = fmt.Errorf("indexed pack %s is missing or changed", p.Name)
					break
				}
			}
		}
		if err != nil {
			c.Close()
		}
	}
	if err != nil {
		rep.Rebuilt, rep.Reason = true, err.Error()
		if c, err = s.recreateCache(); err != nil {
			return rep, err
		}
		indexed, pending = nil, nil
	}
	s.cache = c

	// Index packs that are on disk but not in the cache: packs sealed just
	// before a crash, or every pack after a rebuild.
	isIndexed := map[string]bool{}
	for _, p := range indexed {
		isIndexed[p.Name] = true
	}
	var missing []string
	for name := range onDisk {
		if !isIndexed[name] && !pending[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	for _, name := range missing {
		np, err := s.readPackIndex(ctx, name, onDisk[name])
		if err != nil {
			return rep, err
		}
		if err := c.AddPacks(ctx, np); err != nil {
			return rep, err
		}
		rep.Indexed = append(rep.Indexed, name)
	}
	if rep.Rebuilt {
		s.log.Warn("cache.db rebuilt", "repo", s.dir.Path(), "reason", rep.Reason, "packs", len(rep.Indexed))
	} else if len(rep.Indexed) > 0 {
		s.log.Info("indexed unindexed packs", "repo", s.dir.Path(), "packs", len(rep.Indexed))
	}
	return rep, nil
}

func (s *Store) recreateCache() (*cache.DB, error) {
	if err := cache.Remove(s.cachePath()); err != nil {
		return nil, err
	}
	return cache.Open(s.cachePath())
}

// listPacks returns the pack files in packs/ by name with their sizes.
func (s *Store) listPacks(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.be.List(ctx, repodir.PacksDir+"/", func(key string, size int64) error {
		name := path.Base(key)
		if path.Dir(key) == repodir.PacksDir && strings.HasSuffix(name, ".pack") {
			out[strings.TrimSuffix(name, ".pack")] = size
		}
		return nil
	})
	return out, err
}

func packKey(name string) string { return repodir.PacksDir + "/" + name + ".pack" }

// readPackIndex reads a sealed pack's footer, and the payloads of its
// DigestRecords, without reading any other record.
func (s *Store) readPackIndex(ctx context.Context, name string, size int64) (cache.NewPack, error) {
	p, err := pack.Open(backend.NewReaderAt(ctx, s.be, packKey(name)), size)
	if err != nil {
		return cache.NewPack{}, fmt.Errorf("store: pack %s: %w", name, err)
	}
	np := cache.NewPack{Name: name, Size: size, Entries: p.Entries()}
	for _, e := range p.Entries() {
		if e.Type != object.TypeDigestRecord {
			continue
		}
		rec, err := p.Read(e)
		if err != nil {
			return cache.NewPack{}, fmt.Errorf("store: pack %s: %w", name, err)
		}
		payload, err := rec.Payload(nil)
		if err != nil {
			return cache.NewPack{}, fmt.Errorf("store: pack %s: %w", name, err)
		}
		d, err := object.DecodeAs[*object.DigestRecord](payload)
		if err != nil {
			return cache.NewPack{}, fmt.Errorf("store: pack %s: %w", name, err)
		}
		np.Digests = append(np.Digests, *d)
	}
	return np, nil
}

// Cache returns the repository's cache.db.
func (s *Store) Cache() *cache.DB { return s.cache }

// Has reports whether an object is stored, in a sealed pack or in the open
// pack. It is the single existence check for writers.
func (s *Store) Has(ctx context.Context, h hash.Hash) (bool, error) {
	s.mu.Lock()
	if s.open != nil {
		if _, ok := s.open.index[h]; ok {
			s.mu.Unlock()
			return true, nil
		}
	}
	s.mu.Unlock()
	return s.cache.Has(ctx, h)
}

// ErrMissingReference is returned by Put when an object references an
// object that is not stored (D9).
var ErrMissingReference = errors.New("store: referenced object is not stored")

// Put stores a record in the open pack. CBOR objects are decoded and every
// object they reference must already be stored (D9). Put does not retain
// rec.Data. The object becomes durable at the next Flush.
func (s *Store) Put(ctx context.Context, rec pack.Record) error {
	var digest *object.DigestRecord
	if rec.Type != object.TypeChunk {
		payload, err := rec.Payload(nil)
		if err != nil {
			return err
		}
		o, err := object.Decode(rec.Type, payload)
		if err != nil {
			return err
		}
		for _, ref := range object.References(o) {
			ok, err := s.Has(ctx, ref)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s %s references %s", ErrMissingReference, rec.Type, rec.Hash.Short(), ref.Short())
			}
		}
		if d, ok := o.(*object.DigestRecord); ok {
			digest = d
		}
	} else if rec.Size == 0 {
		return errors.New("store: empty chunk")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store: closed")
	}
	if s.open == nil {
		if err := s.startPack(); err != nil {
			return err
		}
	}
	if s.open.w.Has(rec.Hash) {
		return nil
	}
	off := s.open.w.Len()
	if _, err := s.open.w.Add(rec); err != nil {
		return err
	}
	s.open.index[rec.Hash] = pack.IndexEntry{Hash: rec.Hash, Type: rec.Type, Flags: rec.Flags, Offset: off, Size: rec.Size, Stored: uint32(len(rec.Data))}
	if digest != nil {
		s.open.digests = append(s.open.digests, *digest)
	}
	if int64(s.open.w.Len()) >= s.opts.PackTarget {
		return s.sealLocked(ctx)
	}
	s.resetIdleLocked()
	return nil
}

func (s *Store) startPack() error {
	op, err := newPackFile(s.dir)
	if err != nil {
		return err
	}
	op.index = map[hash.Hash]pack.IndexEntry{}
	s.open = op
	return nil
}

func (s *Store) resetIdleLocked() {
	if s.idle == nil {
		s.idle = time.AfterFunc(s.opts.IdleSeal, func() {
			if err := s.Flush(context.Background()); err != nil {
				s.log.Error("idle pack seal failed", "repo", s.dir.Path(), "err", err)
			}
		})
		return
	}
	s.idle.Reset(s.opts.IdleSeal)
}

// Flush seals the open pack, if any: everything Put before Flush is durable
// and indexed when it returns (write ordering rule 1).
func (s *Store) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealLocked(ctx)
}

func (s *Store) sealLocked(ctx context.Context) error {
	op := s.open
	if op == nil {
		return nil
	}
	s.open = nil
	if op.w.Count() == 0 {
		op.f.Close()
		os.Remove(op.path)
		return nil
	}
	entries, err := op.w.Finish()
	if err != nil {
		op.f.Close()
		return fmt.Errorf("store: seal: %w", err)
	}
	if err := op.f.Sync(); err != nil {
		op.f.Close()
		return fmt.Errorf("store: seal: %w", err)
	}
	size := int64(op.w.Len())
	if err := op.f.Close(); err != nil {
		return fmt.Errorf("store: seal: %w", err)
	}
	if err := s.publish(ctx, op.path, op.name); err != nil {
		return err
	}
	if err := s.cache.AddPacks(ctx, cache.NewPack{Name: op.name, Size: size, Entries: entries, Digests: op.digests}); err != nil {
		return err
	}
	s.maybeConsolidate()
	return nil
}

// publish moves a sealed pack file from tmp/ into packs/.
func (s *Store) publish(ctx context.Context, src, name string) error {
	if fp, ok := s.be.(interface {
		PutFile(ctx context.Context, key, src string) error
	}); ok {
		return fp.PutFile(ctx, packKey(name), src)
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	if err := s.be.Put(ctx, packKey(name), f, -1); err != nil {
		return err
	}
	return os.Remove(src)
}

// Get returns the stored record of h.
func (s *Store) Get(ctx context.Context, h hash.Hash) (pack.Record, error) {
	s.mu.Lock()
	if s.open != nil {
		if e, ok := s.open.index[h]; ok {
			defer s.mu.Unlock()
			if err := s.open.w.Flush(); err != nil {
				return pack.Record{}, err
			}
			return readRecord(s.open.f, e)
		}
	}
	s.mu.Unlock()
	loc, ok, err := s.cache.Lookup(ctx, h)
	if err != nil {
		return pack.Record{}, err
	}
	if !ok {
		return pack.Record{}, fmt.Errorf("%w: %s", ErrNotFound, h)
	}
	return readRecord(backend.NewReaderAt(ctx, s.be, packKey(loc.Pack)), loc.Entry)
}

// ErrNotFound is returned by Get for an object that is not stored.
var ErrNotFound = errors.New("store: object not found")

func readRecord(r io.ReaderAt, e pack.IndexEntry) (pack.Record, error) {
	buf := make([]byte, pack.RecordHeaderSize+int(e.Stored))
	if _, err := r.ReadAt(buf, int64(e.Offset)); err != nil {
		return pack.Record{}, fmt.Errorf("store: read %s: %w", e.Hash.Short(), err)
	}
	rec, err := pack.ParseRecord(buf)
	if err != nil {
		return pack.Record{}, err
	}
	if rec.Hash != e.Hash || rec.Type != e.Type || rec.Size != e.Size {
		return pack.Record{}, fmt.Errorf("%w: record at %d is not %s", pack.ErrCorrupt, e.Offset, e.Hash.Short())
	}
	return rec, nil
}

// Close seals the open pack and closes the cache.
func (s *Store) Close(ctx context.Context) error {
	s.maint.Lock()
	defer s.maint.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.idle != nil {
		s.idle.Stop()
	}
	err := s.sealLocked(ctx)
	return errors.Join(err, s.cache.Close())
}
