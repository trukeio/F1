package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/pkg/f1client"
)

func apiEntry(e *f1client.Entry) *Entry {
	if e == nil {
		return nil
	}
	return &Entry{Kind: e.Kind, File: e.Target}
}

// usable reports whether an entry from the server can be checked out, and
// warns about those that cannot.
func (wc *WC) usable(p, kind string) bool {
	if err := checkPath(p); err != nil {
		wc.printf("warning: skipping %v\n", err)
		return false
	}
	switch kind {
	case "file", "exec", "symlink", "lfs":
		return true
	}
	wc.printf("warning: skipping %s (%s entries are not checked out)\n", p, kind)
	return false
}

// list returns the files of a commit inside the sparse patterns.
func (wc *WC) list(ctx context.Context, commit string) (map[string]Entry, error) {
	out := map[string]Entry{}
	for _, prefix := range wc.prefixes() {
		after := ""
		for {
			resp, err := wc.api.List(ctx, wc.Config.Namespace, commit, prefix, &f1client.ListOptions{After: after, Limit: 10000})
			if err != nil {
				return nil, err
			}
			for _, it := range resp.Items {
				if it.Prefix || !wc.inSparse(it.Path) || !wc.usable(it.Path, it.Kind) {
					continue
				}
				out[it.Path] = Entry{Kind: it.Kind, File: it.File, Size: it.Size}
			}
			if resp.Next == "" {
				break
			}
			after = resp.Next
		}
	}
	return out, nil
}

// diff returns the files that differ between two commits ("" is the empty
// commit) inside the sparse patterns.
func (wc *WC) diff(ctx context.Context, from, to string) (map[string]PathChange, error) {
	out := map[string]PathChange{}
	switch {
	case from == to:
		return out, nil
	case from == "" || to == "":
		files, err := wc.list(ctx, from+to)
		if err != nil {
			return nil, err
		}
		for p, e := range files {
			if from == "" {
				out[p] = PathChange{Path: p, New: &e}
			} else {
				out[p] = PathChange{Path: p, Old: &e}
			}
		}
		return out, nil
	}
	for _, prefix := range wc.prefixes() {
		changes, err := wc.api.Diff(ctx, wc.Config.Namespace, from, to, prefix)
		if err != nil {
			return nil, err
		}
		for _, c := range changes {
			if !wc.inSparse(c.Path) {
				continue
			}
			ch := PathChange{Path: c.Path, Old: apiEntry(c.Old), New: apiEntry(c.New)}
			if ch.Old != nil && !wc.usable(c.Path, ch.Old.Kind) {
				ch.Old = nil
			}
			if ch.New != nil && !wc.usable(c.Path, ch.New.Kind) {
				ch.New = nil
			}
			if ch.Old != nil || ch.New != nil {
				out[c.Path] = ch
			}
		}
	}
	return out, nil
}

// downloaded is a file downloaded into .f1/tmp.
type downloaded struct {
	tmp   string
	local string // local hash
	size  int64
}

// downloadAll downloads files at a commit into temporary files, in
// parallel. On error, the files downloaded so far are removed.
func (wc *WC) downloadAll(ctx context.Context, commit string, paths []string) (map[string]downloaded, error) {
	out := map[string]downloaded{}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range wc.workers {
		wg.Go(func() {
			for p := range work {
				tmp, local, size, err := wc.download(ctx, commit, p)
				if err != nil {
					cancel(err)
					continue
				}
				mu.Lock()
				out[p] = downloaded{tmp, local, size}
				mu.Unlock()
			}
		})
	}
	for _, p := range paths {
		select {
		case work <- p:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		for _, d := range out {
			os.Remove(d.tmp)
		}
		return nil, err
	}
	return out, nil
}

// checkoutAll downloads files at a commit into the working copy and
// records them in the index. Files are placed as they arrive; on error,
// the index holds those placed so far.
func (wc *WC) checkoutAll(ctx context.Context, commit string, files map[string]Entry, ls *lockState) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// Batches bound the temporary space to a batch of files.
	const batch = 256
	for i := 0; i < len(paths); i += batch {
		part := paths[i:min(i+batch, len(paths))]
		got, err := wc.downloadAll(ctx, commit, part)
		if err != nil {
			return err
		}
		for j, p := range part {
			d := got[p]
			fi, err := wc.place(d.tmp, p, normKind(files[p].Kind), ls.readOnly(p))
			if err != nil {
				for _, q := range part[j+1:] {
					os.Remove(got[q].tmp)
				}
				return err
			}
			ie := &IndexEntry{Entry: files[p]}
			ie.Local, ie.Size = d.local, d.size
			stamp(ie, fi)
			wc.Index[p] = ie
		}
	}
	return nil
}

// Clone creates a working copy of a namespace's ref in dir, which must not
// exist or be empty. With sparse patterns (directories), only files below
// them are checked out. ref "" means the namespace's default ref.
func Clone(ctx context.Context, server, namespace, dir, ref string, sparse []string, opts Options) (*WC, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cfg := Config{Server: strings.TrimSuffix(server, "/"), Namespace: namespace, Ref: ref}
	for _, s := range sparse {
		p, err := cleanPattern(s)
		if err != nil {
			return nil, err
		}
		if p == "" {
			cfg.Sparse = nil
			break
		}
		if !slices.Contains(cfg.Sparse, p) {
			cfg.Sparse = append(cfg.Sparse, p)
		}
	}
	wc := newWC(root, cfg, opts)
	ns, err := wc.namespace(ctx)
	if err != nil {
		return nil, err
	}
	if ns.Config.Layout != policy.LayoutTree {
		return nil, fmt.Errorf("namespace %s has layout %s; the native client needs layout = tree (use S3 tools)", namespace, ns.Config.Layout)
	}
	if wc.Config.Ref == "" {
		wc.Config.Ref = ns.Config.DefaultRef
	}
	if !isHash(wc.Config.Ref) {
		wc.Config.Ref = strings.TrimPrefix(wc.Config.Ref, "heads/")
		if err := policy.ValidRefName("heads/" + wc.Config.Ref); err != nil {
			return nil, err
		}
	}
	head, err := wc.head(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(wc.meta(), 0o777); err != nil {
		return nil, err
	}
	if err := wc.lock(); err != nil {
		return nil, err
	}
	if wc.objects, err = openObjects(wc.meta("objects")); err != nil {
		wc.Close()
		return nil, err
	}
	if err := wc.cloneInto(ctx, head); err != nil {
		wc.Close()
		return nil, err
	}
	return wc, nil
}

func (wc *WC) cloneInto(ctx context.Context, head string) error {
	if err := wc.saveConfig(); err != nil {
		return err
	}
	wc.State.Base = head
	if head != "" {
		files, err := wc.list(ctx, head)
		if err != nil {
			return err
		}
		ls, err := wc.locks(ctx)
		if err != nil {
			return err
		}
		if err := wc.checkoutAll(ctx, head, files, ls); err != nil {
			wc.save()
			return err
		}
	}
	return wc.save()
}

// clean returns an error if the working copy has changes, local commits or
// conflicts.
func (wc *WC) clean(ctx context.Context, what string) error {
	if len(wc.State.Commits) > 0 {
		return fmt.Errorf("cannot %s with %d unpushed local commits", what, len(wc.State.Commits))
	}
	if len(wc.State.Conflicts) > 0 {
		return fmt.Errorf("cannot %s with unresolved conflicts", what)
	}
	changes, _, err := wc.scan(ctx)
	if err != nil {
		return err
	}
	if len(changes) > 0 {
		return fmt.Errorf("cannot %s with uncommitted changes", what)
	}
	return nil
}

// apply changes a clean working copy from commit from to commit to.
func (wc *WC) apply(ctx context.Context, from, to string) error {
	changes, err := wc.diff(ctx, from, to)
	if err != nil {
		return err
	}
	ls, err := wc.locks(ctx)
	if err != nil {
		return err
	}
	files := map[string]Entry{}
	for p, ch := range changes {
		if ch.New == nil {
			if err := wc.removeWork(p); err != nil {
				return err
			}
			delete(wc.Index, p)
			continue
		}
		files[p] = *ch.New
	}
	return wc.checkoutAll(ctx, to, files, ls)
}

// Checkout switches the working copy to a branch or a commit hash. The
// working copy must be clean.
func (wc *WC) Checkout(ctx context.Context, ref string) error {
	if err := wc.clean(ctx, "check out"); err != nil {
		return err
	}
	old := wc.Config.Ref
	wc.Config.Ref = strings.TrimPrefix(ref, "heads/")
	head, err := wc.head(ctx)
	if err == nil && head == "" && !wc.detached() {
		// A branch without commits: only an existing branch can be
		// checked out.
		var refs []f1client.Ref
		if refs, err = wc.api.Refs(ctx, wc.Config.Namespace); err == nil {
			found := false
			for _, r := range refs {
				found = found || r.Name == policy.FullRefName(wc.Config.Ref)
			}
			if !found {
				err = fmt.Errorf("no such branch: %s", ref)
			}
		}
	}
	if err != nil {
		wc.Config.Ref = old
		return err
	}
	if err := wc.apply(ctx, wc.State.Base, head); err != nil {
		wc.save()
		return err
	}
	wc.State.Base = head
	if err := wc.saveConfig(); err != nil {
		return err
	}
	return wc.save()
}

// SparseAdd adds a directory to the sparse patterns and checks it out.
func (wc *WC) SparseAdd(ctx context.Context, dir string) error {
	p, err := cleanPattern(dir)
	if err != nil {
		return err
	}
	if len(wc.Config.Sparse) == 0 {
		return errors.New("the working copy is not sparse: everything is checked out")
	}
	if p == "" {
		wc.Config.Sparse = nil
	} else if wc.inSparse(p) {
		return fmt.Errorf("%s is already checked out", p)
	}
	prev := wc.Config.Sparse
	if p != "" {
		// Patterns inside the new one become redundant.
		wc.Config.Sparse = slices.DeleteFunc(slices.Clone(prev), func(s string) bool { return strings.HasPrefix(s, p+"/") })
		wc.Config.Sparse = append(wc.Config.Sparse, p)
	}
	// Check out the new paths at the base commit; local commits cannot
	// touch them, since they were outside the working copy.
	files := map[string]Entry{}
	if wc.State.Base != "" {
		all, err := wc.list(ctx, wc.State.Base)
		if err != nil {
			wc.Config.Sparse = prev
			return err
		}
		for path, e := range all {
			if _, ok := wc.Index[path]; !ok {
				files[path] = e
			}
		}
	}
	ls, err := wc.locks(ctx)
	if err != nil {
		wc.Config.Sparse = prev
		return err
	}
	for path := range files {
		if _, err := os.Lstat(wc.abs(path)); err == nil {
			wc.Config.Sparse = prev
			return fmt.Errorf("%s exists in the working copy; move it away first", path)
		}
	}
	if err := wc.checkoutAll(ctx, wc.State.Base, files, ls); err != nil {
		wc.save()
		return err
	}
	if err := wc.saveConfig(); err != nil {
		return err
	}
	return wc.save()
}

// SparseRemove removes a directory from the sparse patterns and deletes
// its files. Nothing below it may have local changes.
func (wc *WC) SparseRemove(ctx context.Context, dir string) error {
	p, err := cleanPattern(dir)
	if err != nil {
		return err
	}
	if p == "" {
		return errors.New("cannot remove the whole working copy")
	}
	if !slices.Contains(wc.Config.Sparse, p) {
		return fmt.Errorf("%s is not a sparse pattern (patterns: %s)", p, strings.Join(wc.Config.Sparse, ", "))
	}
	if len(wc.Config.Sparse) == 1 {
		return errors.New("cannot remove the last sparse pattern")
	}
	under := func(x string) bool { return x == p || strings.HasPrefix(x, p+"/") }
	changes, _, err := wc.scan(ctx)
	if err != nil {
		return err
	}
	for x := range changes {
		if under(x) {
			return fmt.Errorf("%s has uncommitted changes", x)
		}
	}
	for _, c := range wc.State.Commits {
		for _, ch := range c.Changes {
			if under(ch.Path) {
				return fmt.Errorf("%s has unpushed changes", ch.Path)
			}
		}
	}
	wc.Config.Sparse = slices.DeleteFunc(wc.Config.Sparse, func(s string) bool { return s == p })
	for x := range wc.Index {
		if under(x) && !wc.inSparse(x) {
			if err := wc.removeWork(x); err != nil {
				return err
			}
			delete(wc.Index, x)
		}
	}
	if err := wc.saveConfig(); err != nil {
		return err
	}
	return wc.save()
}
