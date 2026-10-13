package repo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repodir"
)

// Lock is a file lock on an exact path of a ref.
type Lock = repodir.Lock

// LockedError reports a change to a path locked by another principal, or
// to a path that needs a lock the committer does not hold.
type LockedError struct {
	Ref, Path string
	Owner     string // the holder; empty if the path needs a lock nobody holds
}

func (e *LockedError) Error() string {
	if e.Owner == "" {
		return fmt.Sprintf("repo: %s requires a lock (locks = required); lock it before committing", e.Path)
	}
	return fmt.Sprintf("repo: path is locked by %s: %s", e.Owner, e.Path)
}

// ErrLockHeld is returned when locking a path another principal has locked.
var ErrLockHeld = errors.New("repo: path is already locked")

// ErrNotLockOwner is returned when unlocking another principal's lock
// without force.
var ErrNotLockOwner = errors.New("repo: lock belongs to another principal")

func (r *Repo) loadLocks() error {
	locks, err := r.dir.ReadLocks()
	if err != nil {
		return err
	}
	r.locks = locks
	return nil
}

// live returns the locks that have not expired, as of now.
func liveLocks(locks []Lock, now time.Time) []Lock {
	out := locks[:0:0]
	for _, l := range locks {
		if l.Expires.IsZero() || now.Before(l.Expires) {
			out = append(out, l)
		}
	}
	return out
}

// LockPath locks path on ref for owner. ttl 0 means no expiry. Locking a
// path the owner already holds refreshes the lock's expiry.
func (r *Repo) LockPath(ctx context.Context, ref, p, owner string, ttl time.Duration) (Lock, error) {
	end, err := r.beginWrite()
	if err != nil {
		return Lock{}, err
	}
	defer end()
	ref = policy.FullRefName(ref)
	if err := policy.ValidRefName(ref); err != nil {
		return Lock{}, err
	}
	if err := r.CheckPath(p); err != nil {
		return Lock{}, err
	}
	if owner == "" {
		return Lock{}, errors.New("repo: a lock needs an owner")
	}
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	now := time.Now()
	locks := liveLocks(r.locks, now)
	var expires time.Time
	if ttl > 0 {
		expires = now.Add(ttl)
	}
	for i, l := range locks {
		if l.Ref != ref || l.Path != p {
			continue
		}
		if l.Owner != owner {
			return Lock{}, fmt.Errorf("%w: %s by %s", ErrLockHeld, p, l.Owner)
		}
		locks[i].Expires = expires
		if err := r.dir.WriteLocks(locks); err != nil {
			return Lock{}, err
		}
		r.locks = locks
		return locks[i], nil
	}
	l := Lock{ID: ulid.Make().String(), Ref: ref, Path: p, Owner: owner, Created: now.UTC(), Expires: expires}
	locks = append(locks, l)
	if err := r.dir.WriteLocks(locks); err != nil {
		return Lock{}, err
	}
	r.locks = locks
	return l, nil
}

// Unlock removes a lock. Only its owner may remove it, unless force is set
// (administrators).
func (r *Repo) Unlock(ctx context.Context, id, by string, force bool) (Lock, error) {
	end, err := r.beginWrite()
	if err != nil {
		return Lock{}, err
	}
	defer end()
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	locks := liveLocks(r.locks, time.Now())
	for i, l := range locks {
		if l.ID != id {
			continue
		}
		if l.Owner != by && !force {
			return Lock{}, fmt.Errorf("%w: %s holds %s", ErrNotLockOwner, l.Owner, l.Path)
		}
		locks = append(locks[:i], locks[i+1:]...)
		if err := r.dir.WriteLocks(locks); err != nil {
			return Lock{}, err
		}
		r.locks = locks
		return l, nil
	}
	return Lock{}, fmt.Errorf("%w: lock %s", ErrNotFound, id)
}

// Locks returns the live locks of a ref ("" for all refs) whose path starts
// with prefix.
func (r *Repo) Locks(ref, prefix string) []Lock {
	if ref != "" {
		ref = policy.FullRefName(ref)
	}
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	var out []Lock
	for _, l := range liveLocks(r.locks, time.Now()) {
		if (ref == "" || l.Ref == ref) && strings.HasPrefix(l.Path, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// lockCheck checks one changed path against the lock policy for principal.
// refs are the refs whose locks apply (the updated ref, and for a new
// branch also the default ref). Callers hold locksMu.
func (r *Repo) lockCheck(refs []string, p, principal string, mode policy.LockMode, now time.Time) error {
	if mode.Mode == "off" {
		return nil
	}
	held := false
	for _, l := range r.locks {
		if l.Path != p || !containsStr(refs, l.Ref) || (!l.Expires.IsZero() && !now.Before(l.Expires)) {
			continue
		}
		if l.Owner != principal {
			return &LockedError{Ref: l.Ref, Path: p, Owner: l.Owner}
		}
		held = true
	}
	if mode.Mode == "required" && !held && RequiresLock(mode, p) {
		return &LockedError{Ref: refs[0], Path: p}
	}
	return nil
}

// RequiresLock reports whether a path matches the "required" globs. A glob
// without "/" matches the file name in any directory (as svn:needs-lock
// patterns do); a glob with "/" matches the whole path.
func RequiresLock(mode policy.LockMode, p string) bool {
	for _, g := range mode.Required {
		target := p
		if !strings.Contains(g, "/") {
			target = path.Base(p)
		}
		if ok, _ := path.Match(g, target); ok {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// refChanges computes the files an update changes, for lock and ACL checks:
// the diff from the old value, or for a new branch from its merge base with
// the default ref (the empty tree if there is none). It also returns the
// refs whose locks apply.
func (r *Repo) refChanges(ctx context.Context, u RefUpdate) ([]PathChange, []string, error) {
	base := u.Old
	lockRefs := []string{u.Ref}
	if base.IsZero() {
		def := policy.FullRefName(r.meta.DefaultRef)
		if tip := r.Ref(def); !tip.IsZero() && def != u.Ref {
			mb, err := r.MergeBase(ctx, u.New, tip)
			if err != nil {
				return nil, nil, err
			}
			base = mb
			lockRefs = append(lockRefs, def)
		}
	}
	var changes []PathChange
	err := r.Diff(ctx, base, u.New, "", func(c PathChange) error {
		changes = append(changes, c)
		return nil
	})
	return changes, lockRefs, err
}

// isBranch reports whether a ref is a branch, whose updates are checked
// path by path.
func isBranch(ref string) bool { return strings.HasPrefix(ref, "heads/") }

// changePaths returns the paths a change touches, for lock checks.
func changePaths(c Change) []string {
	if c.Op == OpRename {
		return []string{c.From, c.Path}
	}
	return []string{c.Path}
}
