package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

// GC support (doc/plan.md, "GC algorithm"; ADR-0011). The collector in
// internal/gc marks live objects; the store is where it cooperates with
// writers, because every existence check goes through Has:
//
//   - From BeginGC until Condemn, every object Has reports stored is
//     recorded in the touched set. The collector treats touched objects
//     (and what they reference) as live, which closes the race where a
//     write deduplicates onto an old, unreachable object.
//   - Condemn publishes the condemned set: from then on Has reports
//     condemned objects as not stored, so a writer that needs one stores a
//     fresh copy. Sealing a pack that holds a fresh copy moves the object's
//     index row to the new pack and takes it out of the condemned set.
//   - Sweep deletes packs without live objects and rewrites packs that are
//     less than half live, or whose garbage references removed objects. It
//     removes index rows only where they still point at the swept pack, so
//     fresh copies survive.
//
// Condemn takes the maintenance lock and EndGC releases it, so neither
// consolidation nor a hold (freeze, archive, backup) interleaves with the
// sweep.

// Errors returned by GC operations.
var (
	ErrHeld      = errors.New("store: repository is held (frozen, being archived or backed up)")
	ErrGCRunning = errors.New("store: a garbage collection is already running")
)

// gcState is guarded by s.gcMu; the touched map's contents also by
// s.touchMu, and condemned by s.condMu. Lock order: s.gcMu, then s.mu, then
// s.condMu or s.touchMu (leaves). Has holds s.gcMu shared for the whole
// check and Condemn holds it exclusively, so every check is atomic with
// Condemn: an object is either touched before the condemned set is
// computed, or reported condemned after it is published.
type gcState struct {
	active    bool
	touched   map[hash.Hash]struct{} // non-nil from BeginGC until Condemn
	condemned map[hash.Hash]struct{} // published by Condemn until EndGC
	maint     bool                   // the GC holds s.maint
}

// BeginGC starts a GC epoch: until Condemn, objects that Has reports as
// stored are recorded as touched.
func (s *Store) BeginGC() error {
	if s.Held() {
		return ErrHeld
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.gc.active {
		return ErrGCRunning
	}
	s.gc = gcState{active: true, touched: map[hash.Hash]struct{}{}}
	return nil
}

// EndGC ends the epoch: the condemned set is dropped (objects that were
// not swept are reported stored again) and the maintenance lock is
// released.
func (s *Store) EndGC() {
	s.gcMu.Lock()
	maint := s.gc.maint
	s.condMu.Lock()
	s.gc = gcState{}
	s.condMu.Unlock()
	s.gcMu.Unlock()
	if maint {
		s.maint.Unlock()
	}
}

// Condemn computes and publishes the condemned set. It takes the
// maintenance lock (fails with ErrHeld if a hold is active), then, with
// existence checks blocked, calls compute with the touched set; compute
// returns the condemned set (old unmarked objects minus everything the
// touched objects reference). With publish, existence checks report the
// condemned objects as not stored from then on; a dry run does not
// publish.
func (s *Store) Condemn(ctx context.Context, compute func(touched map[hash.Hash]struct{}) (map[hash.Hash]struct{}, error), publish bool) (map[hash.Hash]struct{}, error) {
	s.maint.Lock()
	if s.holds > 0 {
		s.maint.Unlock()
		return nil, ErrHeld
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if !s.gc.active {
		s.maint.Unlock()
		return nil, errors.New("store: Condemn outside a GC epoch")
	}
	s.gc.maint = true
	condemned, err := compute(s.gc.touched)
	if err != nil {
		return nil, err
	}
	s.gc.touched = nil
	if publish {
		s.condMu.Lock()
		s.gc.condemned = maps.Clone(condemned)
		s.condMu.Unlock()
	}
	return condemned, nil
}

// condemnedNow reports whether h is in the published condemned set.
func (s *Store) condemnedNow(h hash.Hash) bool {
	s.condMu.Lock()
	defer s.condMu.Unlock()
	_, ok := s.gc.condemned[h]
	return ok
}

// freshCopies returns the entries of a pack being sealed that are fresh
// copies of condemned objects, and takes them out of the condemned set:
// their index rows move to the new pack.
func (s *Store) freshCopies(entries []pack.IndexEntry) map[hash.Hash]bool {
	s.condMu.Lock()
	defer s.condMu.Unlock()
	if len(s.gc.condemned) == 0 {
		return nil
	}
	var out map[hash.Hash]bool
	for _, e := range entries {
		if _, ok := s.gc.condemned[e.Hash]; ok {
			if out == nil {
				out = map[hash.Hash]bool{}
			}
			out[e.Hash] = true
			delete(s.gc.condemned, e.Hash)
		}
	}
	return out
}

// stillCondemned filters a condemned set by the published state: objects
// that got a fresh copy since Condemn are live.
func (s *Store) stillCondemned(h hash.Hash, condemned map[hash.Hash]struct{}) bool {
	if _, ok := condemned[h]; !ok {
		return false
	}
	s.condMu.Lock()
	defer s.condMu.Unlock()
	if s.gc.condemned == nil {
		return true // a dry run
	}
	_, ok := s.gc.condemned[h]
	return ok
}

// OpenEntries returns the objects of the open pack (written, not yet
// sealed): GC roots, since they may reference old objects.
func (s *Store) OpenEntries() []pack.IndexEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		return nil
	}
	out := make([]pack.IndexEntry, 0, len(s.open.index))
	for _, e := range s.open.index {
		out = append(out, e)
	}
	return out
}

// PackTime returns the creation time of a pack from its ULID name. ok is
// false for a name that is not a ULID.
func PackTime(name string) (time.Time, bool) {
	id, err := ulid.ParseStrict(name)
	if err != nil {
		return time.Time{}, false
	}
	return ulid.Time(id.Time()), true
}

// SweepResult reports a sweep.
type SweepResult struct {
	Deleted        []hash.Hash // objects removed, sorted
	DeletedBytes   int64       // their stored size
	Kept           int         // condemned objects left in packs that stay
	PacksDeleted   []string    // packs without live objects
	PacksRewritten []string    // packs less than half live, rewritten
	PacksCreated   []string    // packs written by the rewrite
}

// Sweep removes the condemned objects from the packs: a pack without live
// objects is deleted, a pack less than half live (by stored bytes) is
// rewritten with its live objects, other packs keep their condemned
// objects until a later GC. Old pack files are deleted after DeleteGrace.
// It must follow Condemn in the same epoch. A dry run computes the same
// result without changing anything.
func (s *Store) Sweep(ctx context.Context, condemned map[hash.Hash]struct{}, dryRun bool) (SweepResult, error) {
	var res SweepResult
	s.gcMu.RLock()
	ok := s.gc.maint
	s.gcMu.RUnlock()
	if !ok {
		return res, errors.New("store: Sweep before Condemn")
	}
	packs, err := s.cache.Packs(ctx)
	if err != nil {
		return res, err
	}
	type packEntries struct {
		name    string
		entries []pack.IndexEntry
	}
	var all []packEntries
	for _, p := range packs {
		entries, err := s.cache.ObjectsInPack(ctx, p.Name)
		if err != nil {
			return res, err
		}
		all = append(all, packEntries{p.Name, entries})
	}
	// Objects that got a fresh copy since Condemn are live.
	dead := map[hash.Hash]struct{}{}
	for h := range condemned {
		if s.stillCondemned(h, condemned) {
			dead[h] = struct{}{}
		}
	}

	type plan struct {
		name       string
		live, dead []pack.IndexEntry
		action     int // 0 keep, 1 delete, 2 rewrite
	}
	var plans []plan
	forced := map[string]bool{} // kept packs whose garbage must go
	refs := map[hash.Hash][]hash.Hash{}
	for {
		// Decide per pack. A condemned object left in a kept pack stays
		// stored, and every stored object's references must stay stored
		// (D9, garbage included): a kept pack whose garbage references
		// objects being removed is rewritten as well. Removing more never
		// requires keeping more, so this converges.
		plans = plans[:0]
		removed := map[hash.Hash]bool{}
		for _, pe := range all {
			pl := plan{name: pe.name}
			var liveBytes, total int64
			for _, e := range pe.entries {
				n := int64(pack.RecordHeaderSize) + int64(e.Stored)
				total += n
				if _, ok := dead[e.Hash]; ok {
					pl.dead = append(pl.dead, e)
				} else {
					pl.live = append(pl.live, e)
					liveBytes += n
				}
			}
			if len(pl.dead) == 0 {
				continue
			}
			switch {
			case len(pl.live) == 0:
				pl.action = 1
			case 2*liveBytes < total || forced[pe.name]:
				pl.action = 2
			}
			if pl.action != 0 {
				for _, e := range pl.dead {
					removed[e.Hash] = true
				}
			}
			plans = append(plans, pl)
		}
		changed := false
		for _, pl := range plans {
			if pl.action != 0 {
				continue
			}
		scan:
			for _, e := range pl.dead {
				if e.Type == object.TypeChunk {
					continue
				}
				rs, ok := refs[e.Hash]
				if !ok {
					if rs, err = s.references(ctx, e.Hash); err != nil {
						return res, err
					}
					refs[e.Hash] = rs
				}
				for _, r := range rs {
					if removed[r] {
						forced[pl.name] = true
						changed = true
						break scan
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	var rewrite []plan
	swept := false
	for _, pl := range plans {
		switch pl.action {
		case 0:
			res.Kept += len(pl.dead)
			continue
		case 1:
			res.PacksDeleted = append(res.PacksDeleted, pl.name)
		case 2:
			res.PacksRewritten = append(res.PacksRewritten, pl.name)
			rewrite = append(rewrite, pl)
		}
		swept = true
		for _, e := range pl.dead {
			res.Deleted = append(res.Deleted, e.Hash)
			res.DeletedBytes += int64(e.Stored)
		}
	}
	slices.SortFunc(res.Deleted, hash.Compare)
	if dryRun || !swept {
		return res, nil
	}

	// Deleted packs, in batches of whole packs, then the rewritten packs:
	// each new pack and the packs it replaces in one transaction.
	var batch []string
	rows := 0
	for _, pl := range plans {
		if pl.action != 1 {
			continue
		}
		batch = append(batch, pl.name)
		if rows += len(pl.dead); rows >= repackRows {
			if err := s.cache.SweepPacks(ctx, batch, nil, s.opts.DeleteGrace); err != nil {
				return res, err
			}
			batch, rows = nil, 0
		}
	}
	if len(batch) > 0 {
		if err := s.cache.SweepPacks(ctx, batch, nil, s.opts.DeleteGrace); err != nil {
			return res, err
		}
	}
	var sources []repackSource
	for _, pl := range rewrite {
		sources = append(sources, repackSource{pl.name, pl.live})
	}
	err = s.repack(ctx, sources, func(old []string, np cache.NewPack) error {
		if err := s.cache.SweepPacks(ctx, old, []cache.NewPack{np}, s.opts.DeleteGrace); err != nil {
			return err
		}
		res.PacksCreated = append(res.PacksCreated, np.Name)
		return nil
	})
	if err != nil {
		return res, err
	}
	// Removed hashes stay in the Bloom filter until it is rebuilt; the
	// confirmed set forgets them now.
	s.filter.removed()
	s.rebuildFilter()
	s.reads.clear()
	if _, err := s.deleteDue(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// references returns what a stored object references.
func (s *Store) references(ctx context.Context, h hash.Hash) ([]hash.Hash, error) {
	rec, err := s.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if rec.Type == object.TypeChunk {
		return nil, nil
	}
	payload, err := rec.Payload(nil)
	if err != nil {
		return nil, err
	}
	o, err := object.Decode(rec.Type, payload)
	if err != nil {
		return nil, err
	}
	return object.References(o), nil
}

// ConsolidateHeld is Consolidate for a caller that holds the maintenance
// lock (the GC, after its sweep).
func (s *Store) ConsolidateHeld(ctx context.Context) (Consolidation, error) {
	return s.consolidate(ctx, false)
}
