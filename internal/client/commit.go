package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/content"
)

// ErrNothingToCommit is returned by Commit without changes.
var ErrNothingToCommit = errors.New("nothing to commit")

// Add schedules untracked files for the next commit and marks conflicts
// as resolved. A directory adds every untracked file below it.
func (wc *WC) Add(ctx context.Context, paths []string) error {
	for _, p := range paths {
		if p != "" && !wc.inSparse(p) {
			return fmt.Errorf("%s is outside the sparse checkout", p)
		}
		fi, err := os.Lstat(wc.abs(p))
		if err != nil {
			return err
		}
		if fi.IsDir() {
			err := filepath.WalkDir(wc.abs(p), func(name string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := wc.Rel(name)
				if err != nil && name != wc.Root {
					return err
				}
				if d.IsDir() {
					if rel == MetaDir || (rel != "" && !wc.sparseDir(rel)) {
						return filepath.SkipDir
					}
					return nil
				}
				if (d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0) && wc.inSparse(rel) {
					wc.addOne(rel)
				}
				return nil
			})
			if err != nil {
				return err
			}
			continue
		}
		wc.addOne(p)
	}
	return wc.save()
}

func (wc *WC) addOne(p string) {
	if wc.resolve(p) {
		return
	}
	if wc.Index[p] == nil && !slices.Contains(wc.State.Added, p) {
		wc.State.Added = append(wc.State.Added, p)
	}
}

// resolve marks a conflict as resolved and removes the .mine and .theirs
// files a binary conflict left. It reports whether p was in conflict.
func (wc *WC) resolve(p string) bool {
	i := slices.Index(wc.State.Conflicts, p)
	if i < 0 {
		return false
	}
	wc.State.Conflicts = slices.Delete(wc.State.Conflicts, i, i+1)
	os.Remove(wc.abs(p + ".mine"))
	os.Remove(wc.abs(p + ".theirs"))
	if _, err := os.Lstat(wc.abs(p)); err == nil && wc.Index[p] == nil && !slices.Contains(wc.State.Added, p) {
		wc.State.Added = append(wc.State.Added, p)
	}
	return true
}

// Remove deletes files from the working copy (and from the next commit),
// and resolves conflicts on them as deletions.
func (wc *WC) Remove(ctx context.Context, paths []string) error {
	for _, p := range paths {
		if p == "" {
			return errors.New("cannot remove the working copy root")
		}
		fi, err := os.Lstat(wc.abs(p))
		if err == nil && fi.IsDir() {
			if err := os.RemoveAll(wc.abs(p)); err != nil {
				return err
			}
		} else if err := wc.removeWork(p); err != nil {
			return err
		}
		wc.resolve(p)
		wc.State.Added = slices.DeleteFunc(wc.State.Added, func(a string) bool { return a == p || strings.HasPrefix(a, p+"/") })
	}
	return wc.save()
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// Commit records the working changes as a local commit, to be sent by
// Push. The content of changed files is stored in the local object cache.
func (wc *WC) Commit(ctx context.Context, message string) (*LocalCommit, error) {
	if len(wc.State.Conflicts) > 0 {
		return nil, fmt.Errorf("unresolved conflicts: %s (resolve them, then f1 add or f1 rm)", strings.Join(wc.State.Conflicts, ", "))
	}
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("empty commit message")
	}
	changes, _, err := wc.scan(ctx)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, ErrNothingToCommit
	}
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	c := &LocalCommit{ID: newID(), Message: message, Time: time.Now()}
	pw, err := wc.objects.newPack(c.ID)
	if err != nil {
		return nil, err
	}
	newIndex := map[string]*IndexEntry{}
	for _, p := range paths {
		ch := changes[p]
		var old *Entry
		if ie := wc.Index[p]; ie != nil {
			e := ie.Entry
			old = &e
		}
		if ch.Deleted {
			c.Changes = append(c.Changes, PathChange{Path: p, Old: old})
			continue
		}
		ie, err := wc.ingest(ctx, pw, p)
		if err != nil {
			pw.abort()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		e := ie.Entry
		c.Changes = append(c.Changes, PathChange{Path: p, Old: old, New: &e})
		newIndex[p] = ie
	}
	if err := pw.commit(); err != nil {
		return nil, err
	}
	for _, ch := range c.Changes {
		if ch.New == nil {
			delete(wc.Index, ch.Path)
		} else {
			wc.Index[ch.Path] = newIndex[ch.Path]
		}
	}
	wc.State.Added = slices.DeleteFunc(wc.State.Added, func(p string) bool { return changes[p] != nil })
	wc.State.Commits = append(wc.State.Commits, *c)
	wc.State.Message = ""
	if err := wc.save(); err != nil {
		return nil, err
	}
	return c, nil
}

// ingest stores a working file's content in a pack and returns its index
// entry.
func (wc *WC) ingest(ctx context.Context, pw *packWriter, p string) (*IndexEntry, error) {
	rc, size, fi, err := wc.openWork(p)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	res, err := content.Ingest(ctx, pw, rc, size, content.Options{Workers: 2})
	if err != nil {
		return nil, err
	}
	ie := &IndexEntry{Entry: Entry{Kind: workKind(fi, wc.indexKind(p)), File: res.File.String(), Size: int64(res.Size)}}
	stamp(ie, fi)
	return ie, nil
}

func (wc *WC) indexKind(p string) string {
	if ie := wc.Index[p]; ie != nil {
		return ie.Kind
	}
	return ""
}
