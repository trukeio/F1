package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

// objectStore is the working copy's object cache: one pack file per local
// commit in .f1/objects, holding the content the commit adds. It is needed
// until the commit is pushed (and, while any commit is unpushed, by later
// commits whose chunks deduplicate against it). It implements
// content.Store for reads; writes go through a packWriter.
type objectStore struct {
	dir   string
	mu    sync.Mutex
	packs map[string]*openPack
}

type openPack struct {
	f *os.File
	r *pack.Reader
}

var errNotCached = errors.New("object is not in the local cache")

func openObjects(dir string) (*objectStore, error) {
	s := &objectStore{dir: dir, packs: map[string]*openPack{}}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".pack"); ok {
			if err := s.open(name); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *objectStore) open(name string) error {
	f, err := os.Open(filepath.Join(s.dir, name+".pack"))
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r, err := pack.Open(f, st.Size())
	if err != nil {
		f.Close()
		return fmt.Errorf("local object cache %s: %w", name, err)
	}
	s.mu.Lock()
	s.packs[name] = &openPack{f: f, r: r}
	s.mu.Unlock()
	return nil
}

// Close closes the pack files.
func (s *objectStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, p := range s.packs {
		p.f.Close()
		delete(s.packs, name)
	}
	return nil
}

// Has implements content.Store.
func (s *objectStore) Has(_ context.Context, h hash.Hash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.packs {
		if _, ok := p.r.Lookup(h); ok {
			return true, nil
		}
	}
	return false, nil
}

// Get implements content.Store.
func (s *objectStore) Get(_ context.Context, h hash.Hash) (pack.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.packs {
		if e, ok := p.r.Lookup(h); ok {
			return p.r.Read(e)
		}
	}
	return pack.Record{}, fmt.Errorf("%w: %s", errNotCached, h.Short())
}

// Put implements content.Store; the cache is written through packWriter.
func (s *objectStore) Put(context.Context, pack.Record) error {
	return errors.New("local object cache: read only")
}

// hasContent reports whether a File and everything it references are in
// the cache. Chunks are looked up, not read.
func (s *objectStore) hasContent(ctx context.Context, file hash.Hash) bool {
	return s.walk(ctx, file, func(hash.Hash, bool) error { return nil }) == nil
}

// walk calls fn for a File and every object it references, each object
// after the objects it references (D9 order), once. Only File and
// FileIndex objects are read; chunks are looked up.
func (s *objectStore) walk(ctx context.Context, file hash.Hash, fn func(h hash.Hash, chunk bool) error) error {
	seen := map[hash.Hash]bool{}
	var visit func(h hash.Hash, chunk bool) error
	visit = func(h hash.Hash, chunk bool) error {
		if seen[h] {
			return nil
		}
		seen[h] = true
		if chunk {
			if ok, _ := s.Has(ctx, h); !ok {
				return fmt.Errorf("%w: chunk %s", errNotCached, h.Short())
			}
			return fn(h, true)
		}
		rec, err := s.Get(ctx, h)
		if err != nil {
			return err
		}
		payload, err := rec.Payload(nil)
		if err != nil {
			return err
		}
		o, err := object.Decode(rec.Type, payload)
		if err != nil {
			return err
		}
		var level uint64
		var refs []hash.Hash
		switch o := o.(type) {
		case *object.File:
			level = o.Level
			for _, r := range o.Refs {
				refs = append(refs, r.Hash)
			}
		case *object.FileIndex:
			level = o.Level
			for _, r := range o.Refs {
				refs = append(refs, r.Hash)
			}
		default:
			return fmt.Errorf("local object cache: %s is a %s, not file content", h.Short(), rec.Type)
		}
		for _, r := range refs {
			if err := visit(r, level == 0); err != nil {
				return err
			}
		}
		return fn(h, false)
	}
	return visit(file, false)
}

// remove deletes the cache (when no local commit needs it).
func (s *objectStore) removeAll() error {
	s.Close()
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// packWriter adds one pack to the cache. It implements content.Store for
// Ingest: Has sees the cache and the pack being written.
type packWriter struct {
	s    *objectStore
	name string
	tmp  *os.File
	w    *pack.Writer
	mu   sync.Mutex
}

func (s *objectStore) newPack(name string) (*packWriter, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.dir, name+".tmp-*")
	if err != nil {
		return nil, err
	}
	w, err := pack.NewWriter(f)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &packWriter{s: s, name: name, tmp: f, w: w}, nil
}

func (p *packWriter) Has(ctx context.Context, h hash.Hash) (bool, error) {
	p.mu.Lock()
	in := p.w.Has(h)
	p.mu.Unlock()
	if in {
		return true, nil
	}
	return p.s.Has(ctx, h)
}

func (p *packWriter) Put(_ context.Context, rec pack.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.w.Add(rec)
	return err
}

func (p *packWriter) Get(ctx context.Context, h hash.Hash) (pack.Record, error) {
	return pack.Record{}, fmt.Errorf("%w: %s (pack being written)", errNotCached, h.Short())
}

// commit finishes the pack, makes it durable and adds it to the cache.
func (p *packWriter) commit() error {
	if _, err := p.w.Finish(); err != nil {
		p.abort()
		return err
	}
	if err := p.tmp.Sync(); err != nil {
		p.abort()
		return err
	}
	if err := p.tmp.Close(); err != nil {
		os.Remove(p.tmp.Name())
		return err
	}
	final := filepath.Join(p.s.dir, p.name+".pack")
	if err := os.Rename(p.tmp.Name(), final); err != nil {
		os.Remove(p.tmp.Name())
		return err
	}
	if err := syncDir(p.s.dir); err != nil {
		return err
	}
	return p.s.open(p.name)
}

func (p *packWriter) abort() {
	p.tmp.Close()
	os.Remove(p.tmp.Name())
}
