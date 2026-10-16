package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/trukeio/f1/internal/client/textmerge"
	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/policy"
)

// PullResult reports what a pull did.
type PullResult struct {
	Updated   []string // paths changed in the working copy
	Rebased   int      // local commits replayed on the new base
	Dropped   int      // local commits whose changes the server already has
	Folded    int      // local commits turned back into working changes
	Conflicts []string // paths with conflicts
}

// Pull brings the working copy to the branch's current commit.
//
// Local commits are rebased path by path: a change to a path the server
// did not change is kept as is; otherwise it is merged three-way (its old
// value, its new value, the server's value). If a merge has conflicts,
// that commit and the later ones are folded back into working changes
// (their messages are kept for the next commit), and the conflicts are
// left in the working copy. Uncommitted changes are merged the same way.
func (wc *WC) Pull(ctx context.Context) (*PullResult, error) {
	if len(wc.State.Conflicts) > 0 {
		return nil, fmt.Errorf("unresolved conflicts: %s (resolve them, then f1 add or f1 rm)", strings.Join(wc.State.Conflicts, ", "))
	}
	head, err := wc.head(ctx)
	if err != nil {
		return nil, err
	}
	res := &PullResult{}
	if head == wc.State.Base {
		return res, nil
	}
	if head == "" {
		return nil, fmt.Errorf("branch %s no longer exists on the server", wc.Config.Ref)
	}
	remote, err := wc.diff(ctx, wc.State.Base, head)
	if err != nil {
		return nil, err
	}
	work, untracked, err := wc.scan(ctx)
	if err != nil {
		return nil, err
	}
	ls, err := wc.locks(ctx)
	if err != nil {
		return nil, err
	}
	p := &puller{wc: wc, oldBase: wc.State.Base, head: head, res: res}
	defer p.cleanup()

	// Phase 1 computes everything (rebased commits, merges, downloads) into
	// packs and temporary files, without touching the working copy. Phase 2
	// applies the planned changes. A failure in phase 1 leaves the working
	// copy as it was.

	// The new HEAD's value of every path the server changed, updated as
	// local commits are rebased.
	cur := map[string]*Entry{}
	for path, ch := range remote {
		cur[path] = ch.New
	}
	var kept []LocalCommit
	fold := len(wc.State.Commits)
	for i, c := range wc.State.Commits {
		next, changes, ok, err := p.rebase(ctx, &c, cur)
		if err != nil {
			return nil, err
		}
		if !ok {
			fold = i
			break
		}
		cur = next
		if len(changes) == 0 {
			res.Dropped++
			continue
		}
		c.Changes = changes
		kept = append(kept, c)
		res.Rebased++
	}

	// Folded commits: their changes are in the working files; HEAD takes
	// the value from before them.
	foldOld := map[string]*Entry{}
	var messages []string
	for _, c := range wc.State.Commits[fold:] {
		for _, ch := range c.Changes {
			if _, ok := foldOld[ch.Path]; !ok {
				foldOld[ch.Path] = ch.Old
			}
		}
		messages = append(messages, c.Message)
		res.Folded++
	}

	paths := map[string]bool{}
	for path := range cur {
		paths[path] = true
	}
	for path := range work {
		paths[path] = true
	}
	for path := range foldOld {
		paths[path] = true
	}
	isUntracked := map[string]bool{}
	for _, path := range untracked {
		isUntracked[path] = true
	}
	var fromServer []string
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		h, inRemote := cur[path]
		base, folded := foldOld[path]
		if !folded {
			base = indexEntry(wc.Index[path])
		}
		if !inRemote {
			if folded {
				p.do(func() error { wc.setHead(path, base); return nil })
			}
			continue
		}
		mineChanged := folded || work[path] != nil || (isUntracked[path] && h != nil)
		switch {
		case !mineChanged && sameContent(base, h):
			// The working copy already holds the new value: a commit this
			// client pushed and the server rebased (ADR-0014).
			if h != nil && wc.Index[path].File != h.File {
				p.do(func() error {
					ie := wc.Index[path]
					local := ie.localHash()
					ie.Entry = *h
					ie.Local = local
					return nil
				})
			}
		case !mineChanged && h == nil:
			p.do(func() error {
				delete(wc.Index, path)
				return wc.removeWork(path)
			})
			res.Updated = append(res.Updated, path)
		case !mineChanged && p.local(ctx, h):
			tmp, err := p.localToTemp(ctx, h)
			if err != nil {
				return nil, err
			}
			p.place(tmp, path, h, h.File, ls.readOnly(path))
			res.Updated = append(res.Updated, path)
		case !mineChanged:
			fromServer = append(fromServer, path)
			res.Updated = append(res.Updated, path)
		case sameContent(base, h):
			// The new HEAD did not change what the working change is
			// based on.
			p.do(func() error { wc.setHead(path, h); return nil })
		default:
			conflict, err := p.mergeWorking(ctx, path, base, h, ls)
			if err != nil {
				return nil, err
			}
			if conflict {
				res.Conflicts = append(res.Conflicts, path)
			}
			res.Updated = append(res.Updated, path)
		}
	}
	got, err := wc.downloadAll(ctx, head, fromServer)
	if err != nil {
		return nil, err
	}
	for _, path := range fromServer {
		d := got[path]
		p.temps = append(p.temps, d.tmp)
		e := *cur[path]
		e.Size = d.size
		p.place(d.tmp, path, &e, d.local, ls.readOnly(path))
	}

	// Phase 2.
	p.closeFiles()
	for _, a := range p.actions {
		if err := a(); err != nil {
			return nil, fmt.Errorf("updating the working copy failed; it may be partially updated: %w", err)
		}
	}
	wc.State.Base = head
	wc.State.Commits = kept
	wc.State.Conflicts = append(wc.State.Conflicts, res.Conflicts...)
	if len(messages) > 0 {
		wc.State.Message = strings.TrimSpace(strings.Join(append([]string{wc.State.Message}, messages...), "\n\n"))
	}
	if err := wc.save(); err != nil {
		return nil, err
	}
	if len(kept) == 0 {
		if err := wc.objects.removeAll(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func indexEntry(ie *IndexEntry) *Entry {
	if ie == nil {
		return nil
	}
	e := ie.Entry
	return &e
}

// setHead sets a path's HEAD value without changing the working file; its
// content is compared again by the next scan. A path whose HEAD value is
// absent but whose file exists becomes an added path.
func (wc *WC) setHead(path string, e *Entry) {
	if e == nil {
		delete(wc.Index, path)
		if _, err := os.Lstat(wc.abs(path)); err == nil && !slices.Contains(wc.State.Added, path) {
			wc.State.Added = append(wc.State.Added, path)
		}
		return
	}
	wc.Index[path] = &IndexEntry{Entry: *e}
	wc.State.Added = slices.DeleteFunc(wc.State.Added, func(a string) bool { return a == path })
}

type puller struct {
	wc            *WC
	oldBase, head string
	res           *PullResult
	temps         []string       // removed by cleanup (those not placed)
	files         []*os.File     // closed before phase 2
	actions       []func() error // phase 2
}

func (p *puller) do(a func() error) { p.actions = append(p.actions, a) }

// place plans moving a temporary file to path, with e as its HEAD value
// (local: the content's local hash).
func (p *puller) place(tmp, path string, e *Entry, local string, readOnly bool) {
	wc := p.wc
	p.do(func() error {
		fi, err := wc.place(tmp, path, normKind(e.Kind), readOnly)
		if err != nil {
			return err
		}
		ie := &IndexEntry{Entry: *e}
		ie.Local = local
		ie.Size = fi.Size()
		stamp(ie, fi)
		wc.Index[path] = ie
		wc.State.Added = slices.DeleteFunc(wc.State.Added, func(a string) bool { return a == path })
		return nil
	})
}

// rename plans renaming a working file.
func (p *puller) rename(from, to string) {
	wc := p.wc
	p.do(func() error {
		os.Remove(wc.abs(to))
		return os.Rename(wc.abs(from), wc.abs(to))
	})
}

// localToTemp copies content from the local cache to a temporary file.
func (p *puller) localToTemp(ctx context.Context, e *Entry) (string, error) {
	h, err := parseHash(e.File)
	if err != nil {
		return "", err
	}
	r, err := content.Open(ctx, p.wc.objects, h)
	if err != nil {
		return "", err
	}
	return p.toTemp(r)
}

func (p *puller) toTemp(r io.Reader) (string, error) {
	f, err := p.wc.tempFile()
	if err != nil {
		return "", err
	}
	p.temps = append(p.temps, f.Name())
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return f.Name(), err
}

// versionToTemp returns a temporary file holding a version: its download,
// or a copy.
func (p *puller) versionToTemp(v *version) (string, error) {
	if v.tmp != "" {
		return v.tmp, nil
	}
	return p.toTemp(io.NewSectionReader(v.f.R, 0, v.f.N))
}

func (p *puller) cleanup() {
	p.closeFiles()
	for _, t := range p.temps {
		os.Remove(t)
	}
}

// closeFiles closes the files opened for merging (before renames, which
// some systems refuse on open files).
func (p *puller) closeFiles() {
	for _, f := range p.files {
		f.Close()
	}
	p.files = nil
}

// openFile opens a file for merging; cleanup closes it.
func (p *puller) openFile(name string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	p.files = append(p.files, f)
	st, err := f.Stat()
	return f, st, err
}

// local reports whether an entry's content is in the local object cache.
func (p *puller) local(ctx context.Context, e *Entry) bool {
	h, err := parseHash(e.File)
	return err == nil && p.wc.objects.hasContent(ctx, h)
}

// version is one version of a file opened for merging.
type version struct {
	f     textmerge.File
	local string // local hash
	tmp   string // the downloaded file holding it, if any
}

// open opens an entry's content: from the local cache, or downloaded from
// the head or the old base commit (whichever has it at this path).
func (p *puller) open(ctx context.Context, path string, e *Entry) (*version, error) {
	if e == nil {
		return &version{f: textmerge.Bytes(nil)}, nil
	}
	if h, err := parseHash(e.File); err == nil && p.wc.objects.hasContent(ctx, h) {
		r, err := content.Open(ctx, p.wc.objects, h)
		if err != nil {
			return nil, err
		}
		return &version{f: textmerge.File{R: r, N: r.Size()}, local: e.File}, nil
	}
	for _, commit := range []string{p.head, p.oldBase} {
		if commit == "" {
			continue
		}
		info, err := p.wc.api.Stat(ctx, p.wc.Config.Namespace, commit, path)
		if err != nil || info.File != e.File {
			continue
		}
		tmp, local, _, err := p.wc.download(ctx, commit, path)
		if err != nil {
			return nil, err
		}
		p.temps = append(p.temps, tmp)
		f, st, err := p.openFile(tmp)
		if err != nil {
			return nil, err
		}
		return &version{f: textmerge.File{R: f, N: st.Size()}, local: local, tmp: tmp}, nil
	}
	return nil, fmt.Errorf("%s: content %s is neither cached nor on the server", path, e.File[:12])
}

// isText decides whether a path merges as text: the namespace's merge
// globs first (the longest matching glob wins), then the content.
func (p *puller) isText(ctx context.Context, path string, vs ...*version) (bool, error) {
	ns, err := p.wc.namespace(ctx)
	if err != nil {
		return false, err
	}
	best := ""
	mode := ""
	for g, m := range ns.Config.Merge {
		if policy.MatchGlob(g, path) && len(g) >= len(best) {
			best, mode = g, m
		}
	}
	if mode != "" {
		return mode == "text", nil
	}
	for _, v := range vs {
		ok, err := textmerge.IsText(v.f)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// merge3 merges three versions as text into a temporary file. It returns
// the file and the number of conflicts, or ok = false for binary content.
func (p *puller) merge3(ctx context.Context, path string, base, mine, theirs *version) (string, int, bool, error) {
	text, err := p.isText(ctx, path, base, mine, theirs)
	if err != nil || !text {
		return "", 0, false, err
	}
	f, err := p.wc.tempFile()
	if err != nil {
		return "", 0, false, err
	}
	p.temps = append(p.temps, f.Name())
	n, err := textmerge.Merge(ctx, base.f, mine.f, theirs.f, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, false, err
	}
	return f.Name(), n, true, nil
}

// rebase replays a local commit on the new HEAD values in cur (the paths
// the server changed). It returns the updated values and the commit's new
// changes, or ok = false if a change conflicts.
func (p *puller) rebase(ctx context.Context, c *LocalCommit, cur map[string]*Entry) (map[string]*Entry, []PathChange, bool, error) {
	next := maps.Clone(cur)
	var changes []PathChange
	var pw *packWriter
	abort := func() {
		if pw != nil {
			pw.abort()
		}
	}
	for _, ch := range c.Changes {
		theirs, remote := next[ch.Path]
		if !remote {
			changes = append(changes, ch)
			continue
		}
		var merged *Entry
		switch {
		case sameContent(ch.New, theirs), sameContent(ch.Old, ch.New):
			merged = theirs
		case sameContent(ch.Old, theirs):
			merged = ch.New
		case ch.New == nil || theirs == nil || ch.New.Kind == "symlink" || theirs.Kind == "symlink":
			abort()
			return nil, nil, false, nil
		default:
			base, err := p.open(ctx, ch.Path, ch.Old)
			if err != nil {
				abort()
				return nil, nil, false, err
			}
			mine, err := p.open(ctx, ch.Path, ch.New)
			if err != nil {
				abort()
				return nil, nil, false, err
			}
			their, err := p.open(ctx, ch.Path, theirs)
			if err != nil {
				abort()
				return nil, nil, false, err
			}
			if mine.local == their.local {
				merged = theirs
				break
			}
			tmp, conflicts, text, err := p.merge3(ctx, ch.Path, base, mine, their)
			if err != nil {
				abort()
				return nil, nil, false, err
			}
			if !text || conflicts > 0 {
				abort()
				return nil, nil, false, nil
			}
			if pw == nil {
				if pw, err = p.wc.objects.newPack(newID()); err != nil {
					return nil, nil, false, err
				}
			}
			e, err := ingestFile(ctx, pw, tmp)
			if err != nil {
				abort()
				return nil, nil, false, err
			}
			e.Kind = mergedKind(ch.Old, ch.New, theirs)
			merged = e
		}
		next[ch.Path] = merged
		if !sameContent(merged, theirs) {
			changes = append(changes, PathChange{Path: ch.Path, Old: theirs, New: merged})
		}
	}
	if pw != nil {
		if err := pw.commit(); err != nil {
			return nil, nil, false, err
		}
	}
	return next, changes, true, nil
}

// mergedKind keeps a kind change (file ↔ exec) made on either side.
func mergedKind(base, mine, theirs *Entry) string {
	if base != nil && normKind(mine.Kind) != normKind(base.Kind) {
		return mine.Kind
	}
	return normKind(theirs.Kind)
}

func ingestFile(ctx context.Context, pw *packWriter, name string) (*Entry, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	res, err := content.Ingest(ctx, pw, f, st.Size(), content.Options{Workers: 2})
	if err != nil {
		return nil, err
	}
	return &Entry{File: res.File.String(), Size: int64(res.Size)}, nil
}

// mergeWorking plans the merge of the working file at path (changed
// relative to base) with the new HEAD value theirs. Text merges into the
// working file, with conflict markers if needed. Other conflicts keep the
// working version as path.mine and the new version as path.theirs. It
// reports a conflict.
func (p *puller) mergeWorking(ctx context.Context, path string, base, theirs *Entry, ls *lockState) (bool, error) {
	wc := p.wc
	fi, err := os.Lstat(wc.abs(path))
	mineExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	setHead := func(e *Entry) { p.do(func() error { wc.setHead(path, e); return nil }) }
	switch {
	case !mineExists && theirs == nil:
		setHead(nil)
		return false, nil
	case !mineExists:
		// Deleted here, changed there.
		v, err := p.open(ctx, path, theirs)
		if err != nil {
			return false, err
		}
		tmp, err := p.versionToTemp(v)
		if err != nil {
			return false, err
		}
		p.do(func() error { _, err := wc.place(tmp, path+".theirs", "file", false); return err })
		setHead(theirs)
		return true, nil
	case theirs == nil:
		// Changed here, deleted there.
		p.rename(path, path+".mine")
		setHead(nil)
		return true, nil
	}
	mine, err := p.openWorkVersion(path)
	if err != nil {
		return false, err
	}
	their, err := p.open(ctx, path, theirs)
	if err != nil {
		return false, err
	}
	head := *theirs
	head.Local = their.local
	if mine.local == their.local && workKind(fi, theirs.Kind) == normKind(theirs.Kind) {
		p.do(func() error {
			fi, err := os.Lstat(wc.abs(path))
			if err != nil {
				return err
			}
			ie := &IndexEntry{Entry: head}
			ie.Size = fi.Size()
			stamp(ie, fi)
			wc.Index[path] = ie
			wc.State.Added = slices.DeleteFunc(wc.State.Added, func(a string) bool { return a == path })
			return nil
		})
		return false, nil
	}
	if fi.Mode()&os.ModeSymlink == 0 && theirs.Kind != "symlink" {
		basev, err := p.open(ctx, path, base)
		if err != nil {
			return false, err
		}
		tmp, conflicts, text, err := p.merge3(ctx, path, basev, mine, their)
		if err != nil {
			return false, err
		}
		if text {
			kind := workKind(fi, theirs.Kind)
			readOnly := ls.readOnly(path) && fi.Mode().Perm()&0o200 == 0
			p.do(func() error { _, err := wc.place(tmp, path, kind, readOnly); return err })
			setHead(&head)
			return conflicts > 0, nil
		}
	}
	// Binary (or symbolic link) conflict.
	tmp, err := p.versionToTemp(their)
	if err != nil {
		return false, err
	}
	p.rename(path, path+".mine")
	p.do(func() error { _, err := wc.place(tmp, path+".theirs", "file", false); return err })
	setHead(&head)
	return true, nil
}

// openWorkVersion opens the working file at path for merging.
func (p *puller) openWorkVersion(path string) (*version, error) {
	wc := p.wc
	local, fi, err := wc.hashWork(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		rc, _, _, err := wc.openWork(path)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		return &version{f: textmerge.Bytes(b), local: local}, nil
	}
	f, st, err := p.openFile(wc.abs(path))
	if err != nil {
		return nil, err
	}
	return &version{f: textmerge.File{R: f, N: st.Size()}, local: local}, nil
}
