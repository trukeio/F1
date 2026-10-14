package client

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Status codes.
const (
	StatusModified  = 'M'
	StatusAdded     = 'A'
	StatusDeleted   = 'D'
	StatusUntracked = '?'
	StatusConflict  = 'C'
)

// FileStatus is a changed path in the working copy.
type FileStatus struct {
	Path string
	Code byte
}

// workChange is a working file that differs from HEAD (the index).
type workChange struct {
	Path    string
	Deleted bool
	Added   bool   // not in HEAD
	Kind    string // kind of the working file
	Local   string // local hash of the working file, if computed
}

// scan compares the working copy with the index. It returns the changed
// tracked and added paths, and the untracked paths. Files whose stat data
// changed but whose content did not get fresh stat data in the index.
func (wc *WC) scan(ctx context.Context) (map[string]*workChange, []string, error) {
	changes := map[string]*workChange{}
	var untracked []string
	seen := map[string]bool{}
	indexTime := wc.indexTime()
	refreshed := false
	added := map[string]bool{}
	for _, p := range wc.State.Added {
		added[p] = true
	}
	err := filepath.WalkDir(wc.Root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == wc.Root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(name, wc.Root+string(filepath.Separator)))
		if d.IsDir() {
			if rel == MetaDir || !wc.sparseDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if !wc.inSparse(rel) || checkPath(rel) != nil {
			return nil
		}
		seen[rel] = true
		fi, err := d.Info()
		if err != nil {
			return err
		}
		ie := wc.Index[rel]
		if ie == nil {
			if added[rel] {
				changes[rel] = &workChange{Path: rel, Added: true, Kind: workKind(fi, "")}
			} else {
				untracked = append(untracked, rel)
			}
			return nil
		}
		kind := workKind(fi, ie.Kind)
		if kind == normKind(ie.Kind) && fi.Size() == ie.Size && fi.ModTime().UnixNano() == ie.Mtime &&
			inode(fi) == ie.Ino && !racy(ie, indexTime) {
			return nil
		}
		local, fi2, err := wc.hashWork(rel)
		if err != nil {
			return err
		}
		if kind == normKind(ie.Kind) && local == ie.localHash() {
			stamp(ie, fi2)
			refreshed = true
			return nil
		}
		changes[rel] = &workChange{Path: rel, Kind: kind, Local: local}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for p := range wc.Index {
		if !seen[p] && wc.inSparse(p) {
			changes[p] = &workChange{Path: p, Deleted: true}
		}
	}
	if refreshed {
		if err := writeJSON(wc.meta("index.json"), wc.Index); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(untracked)
	return changes, untracked, nil
}

// sparseDir reports whether a directory may contain paths inside the
// sparse patterns.
func (wc *WC) sparseDir(rel string) bool {
	if len(wc.Config.Sparse) == 0 {
		return true
	}
	for _, s := range wc.Config.Sparse {
		if s == "" || rel == s || strings.HasPrefix(rel, s+"/") || strings.HasPrefix(s, rel+"/") {
			return true
		}
	}
	return false
}

func (wc *WC) indexTime() (t time.Time) {
	if fi, err := os.Stat(wc.meta("index.json")); err == nil {
		return fi.ModTime()
	}
	return t
}

// Status lists the changed, added, deleted, untracked and conflicting
// paths, sorted.
func (wc *WC) Status(ctx context.Context) ([]FileStatus, error) {
	changes, untracked, err := wc.scan(ctx)
	if err != nil {
		return nil, err
	}
	var out []FileStatus
	for _, c := range changes {
		code := byte(StatusModified)
		switch {
		case c.Deleted:
			code = StatusDeleted
		case c.Added:
			code = StatusAdded
		}
		out = append(out, FileStatus{Path: c.Path, Code: code})
	}
	for _, p := range untracked {
		out = append(out, FileStatus{Path: p, Code: StatusUntracked})
	}
	for _, p := range wc.State.Conflicts {
		out = append(out, FileStatus{Path: p, Code: StatusConflict})
	}
	slices.SortFunc(out, func(a, b FileStatus) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return int(a.Code) - int(b.Code)
	})
	return out, nil
}
