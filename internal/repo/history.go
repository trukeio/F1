package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
)

// Version is a change of one path in a commit of a first-parent history.
type Version struct {
	Commit hash.Hash
	Time   time.Time // committer time
	PathChange
}

// History calls fn for every file changed below prefix along the
// first-parent history of start, newest commit first; within a commit,
// paths are in byte order. Each commit is compared with its first parent;
// the root commit, and a commit whose parent is no longer stored (truncated
// history), are compared with the empty tree. S3 versioning is built on it.
func (r *Repo) History(ctx context.Context, start hash.Hash, prefix string, fn func(Version) error) error {
	for c := start; !c.IsZero(); {
		co, err := r.ReadCommit(ctx, c)
		if err != nil {
			return err
		}
		parent := hash.Zero
		if len(co.Parents) > 0 {
			if _, err := r.ReadCommit(ctx, co.Parents[0]); err == nil {
				parent = co.Parents[0]
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		t := time.Unix(co.Committer.Time, 0)
		err = r.Diff(ctx, parent, c, prefix, func(pc PathChange) error {
			return fn(Version{Commit: c, Time: t, PathChange: pc})
		})
		if err != nil {
			return err
		}
		c = parent
	}
	return nil
}

var errAllFound = errors.New("all found")

// LastModified returns, for each path of commit, the committer time of the
// newest first-parent commit that changed it (History compares the oldest
// stored commit with the empty tree, so every existing path is found).
// Paths that do not exist are omitted. Used for S3 LastModified of entries
// without attrs.mtime.
func (r *Repo) LastModified(ctx context.Context, commit hash.Hash, paths []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(paths))
	if len(paths) == 0 || commit.IsZero() {
		return out, nil
	}
	pending := make(map[string]bool, len(paths))
	prefix := paths[0]
	for _, p := range paths {
		pending[p] = true
		for !strings.HasPrefix(p, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	err := r.History(ctx, commit, prefix, func(v Version) error {
		if pending[v.Path] {
			delete(pending, v.Path)
			if v.New != nil {
				out[v.Path] = v.Time
			}
			if len(pending) == 0 {
				return errAllFound
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errAllFound) {
		return nil, err
	}
	return out, nil
}
