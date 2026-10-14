package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
)

// maxSymlink bounds a symbolic link target.
const maxSymlink = 4096

// workKind returns the entry kind of a working file.
func workKind(fi os.FileInfo, indexKind string) string {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case runtime.GOOS == "windows":
		// No executable bit: keep the recorded kind.
		if indexKind == "exec" {
			return "exec"
		}
		return "file"
	case fi.Mode()&0o111 != 0:
		return "exec"
	}
	return "file"
}

// normKind maps entry kinds to the kinds a working file can have: an LFS
// file is a regular file in the working copy.
func normKind(k string) string {
	if k == "lfs" {
		return "file"
	}
	return k
}

// openWork opens a working file's content: the file, or a symbolic link's
// target.
func (wc *WC) openWork(p string) (io.ReadCloser, int64, os.FileInfo, error) {
	name := wc.abs(p)
	fi, err := os.Lstat(name)
	if err != nil {
		return nil, 0, nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(name)
		if err != nil {
			return nil, 0, nil, err
		}
		return io.NopCloser(bytes.NewReader([]byte(filepath.ToSlash(target)))), int64(len(target)), fi, nil
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, 0, nil, err
	}
	return f, fi.Size(), fi, nil
}

// hashWork returns the local File hash of a working file.
func (wc *WC) hashWork(p string) (string, os.FileInfo, error) {
	rc, _, fi, err := wc.openWork(p)
	if err != nil {
		return "", nil, err
	}
	defer rc.Close()
	h, _, err := content.FileHash(rc)
	if err != nil {
		return "", nil, err
	}
	return h.String(), fi, nil
}

// stamp records a file's stat data in an index entry. Racy timestamps are
// handled when they are compared (see unchanged).
func stamp(ie *IndexEntry, fi os.FileInfo) {
	ie.Mtime = fi.ModTime().UnixNano()
	ie.Ino = inode(fi)
}

// download fetches a file's content at a commit into a temporary file in
// .f1/tmp and returns its name, local hash and size. The caller removes
// it.
func (wc *WC) download(ctx context.Context, commit, p string) (string, string, int64, error) {
	rc, _, err := wc.api.Get(ctx, wc.Config.Namespace, commit, p, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("download %s: %w", p, err)
	}
	defer rc.Close()
	f, err := wc.tempFile()
	if err != nil {
		return "", "", 0, err
	}
	h, size, err := content.FileHash(io.TeeReader(rc, f))
	if err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", 0, fmt.Errorf("download %s: %w", p, err)
	}
	return f.Name(), h.String(), int64(size), nil
}

func (wc *WC) tempFile() (*os.File, error) {
	if err := os.MkdirAll(wc.meta("tmp"), 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(wc.meta("tmp"), "f-*")
}

// place moves a temporary file with an entry's content to path p, as a
// file of the entry's kind, and returns its stat data. readOnly removes the
// write permission (files that need a lock).
func (wc *WC) place(tmp, p, kind string, readOnly bool) (os.FileInfo, error) {
	if err := checkPath(p); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := wc.mkParents(p); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	name := wc.abs(p)
	// A directory in the way (the path became a file) must be empty.
	if st, err := os.Lstat(name); err == nil && st.IsDir() {
		if err := os.Remove(name); err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("cannot write %s: a directory is in the way", p)
		}
	}
	if kind == "symlink" {
		target, err := os.ReadFile(tmp)
		os.Remove(tmp)
		if err != nil {
			return nil, err
		}
		if len(target) > maxSymlink {
			return nil, fmt.Errorf("%s: symbolic link target too long", p)
		}
		os.Remove(name)
		if err := os.Symlink(filepath.FromSlash(string(target)), name); err != nil {
			// Without symlink support, the target is written as a file.
			if err := os.WriteFile(name, target, 0o644); err != nil {
				return nil, err
			}
		}
		return os.Lstat(name)
	}
	mode := os.FileMode(0o644)
	if kind == "exec" {
		mode = 0o755
	}
	if readOnly {
		mode &^= 0o222
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, name); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return os.Lstat(name)
}

// removeWork deletes a working file and its empty parent directories.
func (wc *WC) removeWork(p string) error {
	if err := os.Remove(wc.abs(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wc.removeEmptyParents(p)
	return nil
}

// --- locks and read-only files ---

// lockState is the namespace's lock policy and the caller's locks.
type lockState struct {
	mode policy.LockMode
	held map[string]bool
}

func (wc *WC) locks(ctx context.Context) (*lockState, error) {
	ns, err := wc.namespace(ctx)
	if err != nil {
		return nil, err
	}
	mode, err := policy.ParseLocks(ns.Config.Locks)
	if err != nil {
		return nil, err
	}
	ls := &lockState{mode: mode, held: map[string]bool{}}
	if mode.Mode != "required" {
		return ls, nil
	}
	me, err := wc.me(ctx)
	if err != nil {
		return nil, err
	}
	list, err := wc.api.Locks(ctx, wc.Config.Namespace, wc.lockRef(), "")
	if err != nil {
		return nil, err
	}
	for _, l := range list {
		if l.Owner == me {
			ls.held[l.Path] = true
		}
	}
	return ls, nil
}

// readOnly reports whether a file is checked out read-only: it needs a
// lock (locks = required) that the caller does not hold.
func (ls *lockState) readOnly(p string) bool {
	if ls == nil || ls.mode.Mode != "required" || ls.held[p] {
		return false
	}
	for _, g := range ls.mode.Required {
		if policy.MatchGlob(g, p) {
			return true
		}
	}
	return false
}

// setWritable adds or removes the write permission of a working file.
func (wc *WC) setWritable(p string, writable bool) error {
	name := wc.abs(p)
	fi, err := os.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	mode := fi.Mode().Perm()
	if writable {
		mode |= 0o200
	} else {
		mode &^= 0o222
	}
	return os.Chmod(name, mode)
}

// lockRef is the ref that locks apply to.
func (wc *WC) lockRef() string {
	if wc.detached() {
		return ""
	}
	return wc.Config.Ref
}

// parseHash parses a hex hash.
func parseHash(s string) (hash.Hash, error) { return hash.Parse(s) }

// racy reports whether a recorded mtime is too close to the time the
// index was written to prove that the file has not changed since.
func racy(ie *IndexEntry, indexTime time.Time) bool {
	return ie.Mtime == 0 || ie.Mtime >= indexTime.Add(-time.Second).UnixNano()
}
