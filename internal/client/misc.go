package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/client/textmerge"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/pkg/f1client"
)

// LogEntry is a commit in the history.
type LogEntry struct {
	Hash    string // server commit hash; empty for a local commit
	LocalID string // local commit id
	Author  string
	Time    time.Time
	Message string
}

// Log returns the history, newest first: unpushed local commits, then the
// server's history from the base commit. A path limits it to commits that
// change the path (server commits) or touch it (local commits).
func (wc *WC) Log(ctx context.Context, path string, limit int) ([]LogEntry, error) {
	var out []LogEntry
	for i := len(wc.State.Commits) - 1; i >= 0; i-- {
		c := wc.State.Commits[i]
		if path != "" && !slices.ContainsFunc(c.Changes, func(ch PathChange) bool {
			return ch.Path == path || strings.HasPrefix(ch.Path, path+"/")
		}) {
			continue
		}
		out = append(out, LogEntry{LocalID: c.ID, Time: c.Time, Message: c.Message})
	}
	if wc.State.Base == "" || (limit > 0 && len(out) >= limit) {
		return out, nil
	}
	n := 0
	if limit > 0 {
		n = limit - len(out)
	}
	commits, err := wc.api.Log(ctx, wc.Config.Namespace, wc.State.Base, path, n)
	if err != nil {
		return nil, err
	}
	for _, c := range commits {
		out = append(out, LogEntry{Hash: c.Hash, Author: c.Author.Name, Time: time.Unix(c.Author.Time, 0), Message: c.Message})
	}
	return out, nil
}

// Diff writes the uncommitted changes below the given paths (all if none)
// as unified diffs, comparing with HEAD.
func (wc *WC) Diff(ctx context.Context, paths []string, w io.Writer) error {
	changes, _, err := wc.scan(ctx)
	if err != nil {
		return err
	}
	var list []string
	for p := range changes {
		if len(paths) == 0 || slices.ContainsFunc(paths, func(x string) bool { return x == "" || p == x || strings.HasPrefix(p, x+"/") }) {
			list = append(list, p)
		}
	}
	sort.Strings(list)
	pl := &puller{wc: wc, oldBase: wc.State.Base}
	defer pl.cleanup()
	for _, p := range list {
		ch := changes[p]
		oldName, newName := "a/"+p, "b/"+p
		old, err := pl.open(ctx, p, indexEntry(wc.Index[p]))
		if err != nil {
			return err
		}
		if wc.Index[p] == nil {
			oldName = "/dev/null"
		}
		cur := &version{f: textmerge.Bytes(nil)}
		if ch.Deleted {
			newName = "/dev/null"
		} else if cur, err = pl.openWorkVersion(p); err != nil {
			return err
		}
		text, err := pl.isText(ctx, p, old, cur)
		if err != nil {
			return err
		}
		if !text {
			fmt.Fprintf(w, "Binary files %s and %s differ\n", oldName, newName)
		} else if err := textmerge.Unified(ctx, old.f, cur.f, oldName, newName, w); err != nil {
			return err
		}
		pl.closeFiles()
	}
	return nil
}

// Lock locks paths on the server. Files that need a lock become
// writable.
func (wc *WC) Lock(ctx context.Context, paths []string) error {
	for _, p := range paths {
		if _, err := wc.api.Lock(ctx, wc.Config.Namespace, wc.lockRef(), p, 0); err != nil {
			return fmt.Errorf("lock %s: %w", p, err)
		}
		if err := wc.setWritable(p, true); err != nil {
			return err
		}
	}
	return nil
}

// Unlock removes the caller's locks on paths; force removes anyone's
// (needs admin on the path). Files that need a lock become read-only.
func (wc *WC) Unlock(ctx context.Context, paths []string, force bool) error {
	me, err := wc.me(ctx)
	if err != nil {
		return err
	}
	ls, err := wc.locks(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		list, err := wc.api.Locks(ctx, wc.Config.Namespace, wc.lockRef(), p)
		if err != nil {
			return err
		}
		found := false
		for _, l := range list {
			if l.Path != p || (!force && l.Owner != me) {
				continue
			}
			if err := wc.api.Unlock(ctx, wc.Config.Namespace, l.ID, force); err != nil {
				return fmt.Errorf("unlock %s: %w", p, err)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("%s is not locked by %s", p, me)
		}
		delete(ls.held, p)
		if ls.readOnly(p) {
			if err := wc.setWritable(p, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// Locks lists the locks of the working copy's ref.
func (wc *WC) Locks(ctx context.Context) ([]f1client.Lock, error) {
	return wc.api.Locks(ctx, wc.Config.Namespace, wc.lockRef(), "")
}

// Branches lists the namespace's branches (short names).
func (wc *WC) Branches(ctx context.Context) ([]string, error) {
	refs, err := wc.api.Refs(ctx, wc.Config.Namespace)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if name, ok := strings.CutPrefix(r.Name, "heads/"); ok {
			out = append(out, name)
		}
	}
	return out, nil
}

// CreateBranch creates a branch at the working copy's base commit (local
// commits are not included until pushed).
func (wc *WC) CreateBranch(ctx context.Context, name string) error {
	name = strings.TrimPrefix(name, "heads/")
	if err := policy.ValidRefName("heads/" + name); err != nil {
		return err
	}
	if wc.State.Base == "" {
		return errors.New("the branch has no commit yet")
	}
	return wc.api.UpdateRef(ctx, wc.Config.Namespace, "heads/"+name, "", wc.State.Base)
}
