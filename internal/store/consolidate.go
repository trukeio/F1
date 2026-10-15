package store

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/backend"
	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/repodir"
)

// maybeConsolidate starts a background consolidation after a seal. It is
// called with s.mu held and must not block.
func (s *Store) maybeConsolidate() {
	if !s.consolidating.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.consolidating.Store(false)
		if _, err := s.Consolidate(context.Background(), false); err != nil {
			s.log.Error("pack consolidation failed", "repo", s.dir.Path(), "err", err)
		}
	}()
}

// Consolidation reports what Consolidate did.
type Consolidation struct {
	Merged  []string // small packs replaced
	Created []string // new packs
	Deleted []string // replaced pack files deleted after their grace period
}

// Consolidate merges small sealed packs into packs of up to PackTarget
// bytes when there are more than SmallPackLimit of them, or whenever there
// are at least two if force is set. Objects are copied without any
// liveness check; the index rows move to the new packs in one transaction,
// and the old pack files are deleted after DeleteGrace so that in-flight
// reads complete.
func (s *Store) Consolidate(ctx context.Context, force bool) (Consolidation, error) {
	s.maint.Lock()
	defer s.maint.Unlock()
	return s.consolidate(ctx, force)
}

// consolidate is Consolidate with s.maint held.
func (s *Store) consolidate(ctx context.Context, force bool) (Consolidation, error) {
	var res Consolidation
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.holds > 0 {
		return res, nil
	}

	packs, err := s.cache.Packs(ctx)
	if err != nil {
		return res, err
	}
	var small []string
	for _, p := range packs {
		if p.Size < s.opts.SmallPack {
			small = append(small, p.Name)
		}
	}
	if len(small) < 2 || (!force && len(small) <= s.opts.SmallPackLimit) {
		res.Deleted, err = s.deleteDue(ctx)
		return res, err
	}

	var created []cache.NewPack
	var cur *openPack
	finish := func() error {
		if cur == nil {
			return nil
		}
		np, err := s.sealConsolidated(ctx, cur)
		cur = nil
		if err != nil {
			return err
		}
		created = append(created, np)
		return nil
	}
	defer func() {
		if cur != nil {
			cur.f.Close()
			os.Remove(cur.path)
		}
	}()
	for _, name := range small {
		entries, err := s.cache.ObjectsInPack(ctx, name)
		if err != nil {
			return res, err
		}
		r := backend.NewReaderAt(ctx, s.be, packKey(name))
		for _, e := range entries {
			rec, err := readRecord(r, e)
			if err != nil {
				return res, fmt.Errorf("store: consolidate %s: %w", name, err)
			}
			if cur == nil {
				if cur, err = newPackFile(s.dir); err != nil {
					return res, err
				}
			}
			if _, err := cur.w.Add(rec); err != nil {
				return res, err
			}
			if rec.Type == object.TypeDigestRecord {
				payload, err := rec.Payload(nil)
				if err != nil {
					return res, err
				}
				d, err := object.DecodeAs[*object.DigestRecord](payload)
				if err != nil {
					return res, err
				}
				cur.digests = append(cur.digests, *d)
			}
			if int64(cur.w.Len()) >= s.opts.PackTarget {
				if err := finish(); err != nil {
					return res, err
				}
			}
		}
	}
	if err := finish(); err != nil {
		return res, err
	}
	if err := s.cache.ReplacePacks(ctx, small, created, s.opts.DeleteGrace); err != nil {
		return res, err
	}
	res.Merged = small
	for _, np := range created {
		res.Created = append(res.Created, np.Name)
	}
	s.log.Info("consolidated packs", "repo", s.dir.Path(), "merged", len(small), "created", len(created))
	res.Deleted, err = s.deleteDue(ctx)
	return res, err
}

func newPackFile(dir *repodir.Dir) (*openPack, error) {
	name := ulid.Make().String()
	p := dir.Join(repodir.TmpDir, name+".pack.open")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	w, err := pack.NewWriter(f)
	if err != nil {
		f.Close()
		os.Remove(p)
		return nil, err
	}
	return &openPack{name: name, path: p, f: f, w: w}, nil
}

// sealConsolidated finishes a pack written by Consolidate and moves it into
// packs/, without indexing it yet.
func (s *Store) sealConsolidated(ctx context.Context, op *openPack) (cache.NewPack, error) {
	entries, err := op.w.Finish()
	if err != nil {
		op.f.Close()
		return cache.NewPack{}, err
	}
	size := int64(op.w.Len())
	if err := op.f.Close(); err != nil {
		return cache.NewPack{}, err
	}
	if err := s.publish(ctx, op.path, op.name); err != nil {
		return cache.NewPack{}, err
	}
	return cache.NewPack{Name: op.name, Size: size, Entries: entries, Digests: op.digests}, nil
}

// Hold prevents the store from deleting or replacing pack files until the
// returned release function is called: consolidation (and, later, GC) skips
// the repository while any hold is active. Hold waits for a running
// consolidation to finish, so that when it returns the set of pack files
// only grows. Holds back freeze, archive and backup (doc/plan.md,
// "Repository directory").
func (s *Store) Hold() (release func()) {
	s.maint.Lock()
	s.holds++
	s.maint.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.maint.Lock()
			s.holds--
			s.maint.Unlock()
		})
	}
}

// Held reports whether a hold is active.
func (s *Store) Held() bool {
	s.maint.Lock()
	defer s.maint.Unlock()
	return s.holds > 0
}

// deleteDue deletes replaced pack files whose grace period has passed.
func (s *Store) deleteDue(ctx context.Context) ([]string, error) {
	due, err := s.cache.DuePendingDeletes(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	slices.Sort(due)
	for _, name := range due {
		if err := s.be.Delete(ctx, packKey(name)); err != nil {
			return nil, err
		}
		if err := s.cache.DonePendingDelete(ctx, name); err != nil {
			return nil, err
		}
	}
	return due, nil
}
