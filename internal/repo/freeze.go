package repo

import (
	"context"
	"errors"
)

// ErrFrozen is returned for writes to a frozen repository. Protocol layers
// map it to S3 503 SlowDown, a Git push error and native 423.
var ErrFrozen = errors.New("repo: repository is frozen for copying; retry after it is unfrozen")

// gate tracks in-flight writes so that Freeze can wait for them.
type gate struct {
	frozen   bool
	inflight int
	release  func() // the store hold taken by Freeze
}

// beginWrite registers a write. It fails fast with ErrFrozen while the
// repository is frozen; otherwise the returned function must be called when
// the write is done.
func (r *Repo) beginWrite() (end func(), err error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.gate.frozen {
		return nil, ErrFrozen
	}
	r.gate.inflight++
	return func() {
		r.gateMu.Lock()
		r.gate.inflight--
		if r.gate.inflight == 0 {
			r.gateCond.Broadcast()
		}
		r.gateMu.Unlock()
	}, nil
}

// Freeze makes the repository read-only so that its directory can be
// copied with any tool: new writes fail with ErrFrozen, in-flight writes
// (including uploads in progress) are waited for, the open pack is sealed,
// cache.db is checkpointed into one file and a store hold stops pack
// consolidation and GC. Freezing a frozen repository is a no-op. The frozen
// state is not persisted: a restarted server starts unfrozen.
func (r *Repo) Freeze(ctx context.Context) error {
	r.gateMu.Lock()
	if r.gate.frozen {
		r.gateMu.Unlock()
		return nil
	}
	r.gate.frozen = true
	for r.gate.inflight > 0 {
		r.gateCond.Wait()
	}
	r.gateMu.Unlock()

	release := r.store.Hold()
	err := r.store.Flush(ctx)
	if err == nil {
		err = r.store.Cache().Checkpoint(ctx)
	}
	if err != nil {
		release()
		r.gateMu.Lock()
		r.gate.frozen = false
		r.gateMu.Unlock()
		return err
	}
	r.gateMu.Lock()
	r.gate.release = release
	r.gateMu.Unlock()
	return nil
}

// Unfreeze makes a frozen repository writable again.
func (r *Repo) Unfreeze() {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if !r.gate.frozen {
		return
	}
	r.gate.frozen = false
	if r.gate.release != nil {
		r.gate.release()
		r.gate.release = nil
	}
}

// Frozen reports whether the repository is frozen.
func (r *Repo) Frozen() bool {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.gate.frozen
}
