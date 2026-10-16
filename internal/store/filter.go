package store

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/trukeio/f1/internal/hash"
)

// The existence filter answers most of Has without cache.db, whose queries
// serialize on SQLite's mutexes under concurrent writes (ADR-0012):
//
//   - A Bloom filter of every indexed hash answers "not indexed" for new
//     content with certainty. It is built in the background when the store
//     opens (Has queries cache.db until then), extended on every seal and
//     rebuilt after a GC sweep, whose deletions it cannot remove.
//   - A bounded set of hashes recently confirmed indexed answers repeated
//     checks of existing objects (tree updates check every entry of every
//     changed node). A sweep clears it.
//
// Both describe cache.db only; the GC's condemned set applies on top.

const (
	bloomBitsPerKey = 12
	bloomHashes     = 8
	confirmedMax    = 1 << 16
)

type bloom struct {
	bits []uint64
	n    uint64 // bit count
	keys int    // hashes added
	cap  int    // keys it was sized for
}

func newBloom(capacity int) *bloom {
	capacity = max(capacity, 1<<20)
	n := uint64(capacity * bloomBitsPerKey)
	return &bloom{bits: make([]uint64, (n+63)/64), n: (n + 63) / 64 * 64, cap: capacity}
}

// Object hashes are uniform (BLAKE3), so their bytes serve as the hash
// functions (double hashing).
func (b *bloom) positions(h hash.Hash, fn func(uint64) bool) bool {
	h1 := binary.LittleEndian.Uint64(h[0:8])
	h2 := binary.LittleEndian.Uint64(h[8:16]) | 1
	for i := uint64(0); i < bloomHashes; i++ {
		if !fn((h1 + i*h2) % b.n) {
			return false
		}
	}
	return true
}

func (b *bloom) add(h hash.Hash) {
	b.positions(h, func(p uint64) bool {
		b.bits[p/64] |= 1 << (p % 64)
		return true
	})
	b.keys++
}

func (b *bloom) mayContain(h hash.Hash) bool {
	return b.positions(h, func(p uint64) bool { return b.bits[p/64]&(1<<(p%64)) != 0 })
}

// filter is the store's existence filter.
type filter struct {
	mu        sync.Mutex
	bloom     *bloom // nil until built
	building  bool
	pending   []hash.Hash // added while a build runs
	confirmed map[hash.Hash]struct{}
}

// notIndexed reports whether h is certainly not in cache.db.
func (f *filter) notIndexed(h hash.Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bloom != nil && !f.bloom.mayContain(h)
}

// isConfirmed reports whether h was recently found in cache.db.
func (f *filter) isConfirmed(h hash.Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.confirmed[h]
	return ok
}

func (f *filter) confirm(h hash.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.confirmed) >= confirmedMax || f.confirmed == nil {
		f.confirmed = make(map[hash.Hash]struct{}, confirmedMax)
	}
	f.confirmed[h] = struct{}{}
}

// added records hashes newly indexed in cache.db. It returns true if the
// Bloom filter has outgrown its size and should be rebuilt.
func (f *filter) added(hs []hash.Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.building {
		f.pending = append(f.pending, hs...)
	}
	if f.bloom == nil {
		return false
	}
	for _, h := range hs {
		f.bloom.add(h)
	}
	return f.bloom.keys > 2*f.bloom.cap && !f.building
}

// removed is called after objects were removed from cache.db (a sweep):
// the confirmed set is cleared at once; the caller rebuilds the Bloom
// filter.
func (f *filter) removed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmed = nil
}

// rebuildFilter builds the Bloom filter from cache.db in the background.
// Hashes indexed meanwhile are recorded and added before it is published.
func (s *Store) rebuildFilter() {
	f := &s.filter
	f.mu.Lock()
	if f.building {
		f.mu.Unlock()
		return
	}
	f.building = true
	f.pending = nil
	f.mu.Unlock()
	s.builds.Add(1)
	go func() {
		defer s.builds.Done()
		ctx := context.Background()
		n, err := s.cache.CountObjects(ctx)
		var b *bloom
		if err == nil {
			b = newBloom(2 * n)
			err = s.cache.EachHash(ctx, func(h hash.Hash) { b.add(h) })
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.building = false
		if err != nil {
			s.log.Warn("existence filter not built", "repo", s.dir.Path(), "err", err)
			f.bloom = nil
			f.pending = nil
			return
		}
		for _, h := range f.pending {
			b.add(h)
		}
		f.pending = nil
		f.bloom = b
	}()
}
