package store

import (
	"container/list"
	"sync"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/pack"
)

// readCache keeps recently read records (stored form), so that hot
// objects (File and FileIndex objects, small files' chunks) are served
// without an index lookup and a pack read (doc/plan.md, "Pack store").
// Objects are immutable, so entries never go stale; a GC sweep clears it
// anyway, so that removed objects are not served.
type readCache struct {
	mu    sync.Mutex
	max   int64 // bytes
	bytes int64
	lru   *list.List // of *cached, most recent first
	byKey map[hash.Hash]*list.Element
}

type cached struct {
	rec pack.Record
}

// maxCachedRecord bounds the records kept: larger ones are streamed reads
// of big files, which would only evict the hot set.
const maxCachedRecord = 64 << 10

func newReadCache(max int64) *readCache {
	return &readCache{max: max, lru: list.New(), byKey: map[hash.Hash]*list.Element{}}
}

func (c *readCache) get(h hash.Hash) (pack.Record, bool) {
	if c.max <= 0 {
		return pack.Record{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[h]
	if !ok {
		return pack.Record{}, false
	}
	c.lru.MoveToFront(e)
	return e.Value.(*cached).rec, true
}

func (c *readCache) put(rec pack.Record) {
	if c.max <= 0 || len(rec.Data) > maxCachedRecord {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[rec.Hash]; ok {
		return
	}
	// Readers share the bytes: an append must not write into them.
	rec.Data = rec.Data[:len(rec.Data):len(rec.Data)]
	c.byKey[rec.Hash] = c.lru.PushFront(&cached{rec})
	c.bytes += int64(len(rec.Data))
	for c.bytes > c.max {
		last := c.lru.Back()
		r := last.Value.(*cached).rec
		c.lru.Remove(last)
		delete(c.byKey, r.Hash)
		c.bytes -= int64(len(r.Data))
	}
}

func (c *readCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Init()
	c.byKey = map[hash.Hash]*list.Element{}
	c.bytes = 0
}
