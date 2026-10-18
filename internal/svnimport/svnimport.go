// Package svnimport imports a Subversion dump into an F1 repository
// (ADR-0019): one commit per revision on each ref the revision changes,
// with refs taken from the trunk/branches/tags layout, and authors, dates
// and revision numbers kept.
//
// Nodes are applied in dump order. File changes of a revision collect in
// a per-ref overlay and are written to the ref's tree together; directory
// copies and removals are applied to the tree at once, after the overlay.
// Commit objects are written as revisions end, and refs move at
// checkpoints, each one atomic update that never splits a revision.
package svnimport

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/svndump"
)

// Layout says where branches live in the SVN repository.
type Layout struct {
	Trunk    string   // the default branch; default "trunk"
	Branches []string // directories whose children are branches; default "branches"
	Tags     []string // directories whose children are tags; default "tags"
	Root     bool     // the whole repository is the default branch; no other refs
}

// Options control an import.
type Options struct {
	Layout  Layout
	Authors map[string]repo.Person // SVN user name → author
	// Principal is recorded in refs.log; default "system:svn-import".
	Principal string
	// Refs move after this many revisions or bytes of content ingested
	// since the last checkpoint, and at the end. Defaults: 1,000 and 1 GiB.
	CheckpointRevisions int
	CheckpointBytes     int64
	// Progress, if set, is called after each revision.
	Progress func(rev int)
}

// Report describes an import.
type Report struct {
	UUID           string            `json:"uuid"`
	Revisions      int               `json:"revisions"`     // revisions read, revision 0 excluded
	LastRevision   int               `json:"last_revision"` // the last revision read
	Commits        int               `json:"commits"`
	Bytes          int64             `json:"bytes"`             // file content ingested
	Refs           map[string]string `json:"refs"`              // SVN path → ref, for live refs
	Renamed        map[string]string `json:"renamed,omitempty"` // SVN path → ref, where the name had to change
	Deleted        []string          `json:"deleted,omitempty"` // refs kept under svn-deleted/
	TagChanges     []string          `json:"tag_changes,omitempty"`
	UnknownAuthors []string          `json:"unknown_authors,omitempty"`
	NeedsLock      []string          `json:"needs_lock,omitempty"` // globs of files with svn:needs-lock
	Externals      []string          `json:"externals,omitempty"`  // paths with svn:externals
	Ignores        []string          `json:"ignores,omitempty"`    // paths with svn:ignore
	DroppedProps   map[string]int    `json:"dropped_props,omitempty"`
}

// maxSymlink bounds the text of an svn:special file ("link <target>").
const maxSymlink = 64 << 10

// Import reads a dump and writes its revisions to rp, which must be an
// empty namespace with layout tree.
func Import(ctx context.Context, rp *repo.Repo, dump io.Reader, opts Options) (*Report, error) {
	if rp.Meta().Layout != policy.LayoutTree {
		return nil, errors.New("svnimport: the namespace needs layout = tree")
	}
	if len(rp.Refs()) > 0 {
		return nil, errors.New("svnimport: the namespace is not empty")
	}
	lay, err := newLayout(opts.Layout)
	if err != nil {
		return nil, err
	}
	if opts.Principal == "" {
		opts.Principal = "system:svn-import"
	}
	if opts.CheckpointRevisions <= 0 {
		opts.CheckpointRevisions = 1000
	}
	if opts.CheckpointBytes <= 0 {
		opts.CheckpointBytes = 1 << 30
	}
	dr, err := svndump.NewReader(dump)
	if err != nil {
		return nil, err
	}
	def := strings.TrimPrefix(policy.FullRefName(rp.Meta().DefaultRef), "heads/")
	im := &importer{
		ctx: ctx, rp: rp, opts: opts, lay: lay, dr: dr, defaultBranch: def,
		rep:       &Report{Refs: map[string]string{}, Renamed: map[string]string{}, DroppedProps: map[string]int{}},
		revs:      map[string][]mark{},
		heads:     map[string]*head{},
		names:     map[string]string{},
		want:      map[string]hash.Hash{},
		committed: rp.Refs(),
		trees:     map[hash.Hash]hash.Hash{},
		needsLock: map[string]bool{},
		authors:   map[string]bool{},
		rev:       -1,
	}
	if err := im.run(); err != nil {
		return im.rep, err
	}
	return im.rep, nil
}

// mark records a ref's commit as of a revision; a zero commit means the
// ref did not exist.
type mark struct {
	rev    int
	commit hash.Hash
}

// head is a live ref.
type head struct {
	name    string // the F1 ref
	tag     bool
	commit  hash.Hash
	created int // the revision that created it
}

// edit is a ref being changed in the current revision.
type edit struct {
	parent hash.Hash // the commit the new one builds on; zero for none
	root   hash.Hash // the tree so far, without files
	files  map[string]*object.Entry
}

type importer struct {
	ctx           context.Context
	rp            *repo.Repo
	opts          Options
	lay           layout
	dr            *svndump.Reader
	rep           *Report
	defaultBranch string

	revs      map[string][]mark    // SVN ref path → its commits by revision
	heads     map[string]*head     // SVN ref path → live ref
	names     map[string]string    // F1 ref → SVN ref path, for live refs
	want      map[string]hash.Hash // ref values since the last checkpoint (zero deletes)
	committed map[string]hash.Hash // ref values at the last checkpoint
	trees     map[hash.Hash]hash.Hash
	needsLock map[string]bool
	authors   map[string]bool // unknown authors

	sinceRevs  int
	sinceBytes int64

	// The current revision.
	rev      int
	revProps map[string]string
	lastTime time.Time
	edits    map[string]*edit
	touched  map[string]bool
}

func (im *importer) run() error {
	for {
		rec, err := im.dr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch rec := rec.(type) {
		case *svndump.Revision:
			if err := im.endRevision(); err != nil {
				return err
			}
			if rec.Number <= im.rev {
				return fmt.Errorf("svnimport: revision %d follows revision %d", rec.Number, im.rev)
			}
			im.rev, im.revProps = rec.Number, rec.Props
			im.edits, im.touched = map[string]*edit{}, map[string]bool{}
		case *svndump.Node:
			if im.rev < 0 {
				return errors.New("svnimport: a node before the first revision")
			}
			if err := im.node(rec); err != nil {
				return fmt.Errorf("svnimport: r%d %s: %w", im.rev, rec.Path, err)
			}
		}
	}
	if err := im.endRevision(); err != nil {
		return err
	}
	if err := im.checkpoint(); err != nil {
		return err
	}
	im.finishReport()
	return nil
}

func (im *importer) node(n *svndump.Node) error {
	c, err := im.lay.classify(n.Path)
	if err != nil {
		return err
	}
	switch c.kind {
	case container:
		if n.Action == svndump.ActionDelete || n.Action == svndump.ActionReplace {
			for p := range im.heads {
				if under(p, n.Path) {
					im.deleteRef(p)
				}
			}
		}
		if n.Action == svndump.ActionDelete {
			return nil
		}
		if n.Kind == svndump.KindFile {
			return errors.New("a file outside the branches of the layout (see --root, --trunk, --branches and --tags)")
		}
		if n.CopyFromRev >= 0 {
			return errors.New("a copy onto a directory of the layout is not supported")
		}
		im.dirProps(n)
		return nil

	case refRoot:
		if n.Kind == svndump.KindFile {
			return errors.New("a file where the layout expects a branch or tag directory")
		}
		if n.Action == svndump.ActionDelete || n.Action == svndump.ActionReplace {
			im.deleteRef(c.ref)
		}
		switch n.Action {
		case svndump.ActionDelete:
			return nil
		case svndump.ActionAdd, svndump.ActionReplace:
			if err := im.createRef(c, n); err != nil {
				return err
			}
		}
		im.dirProps(n)
		return nil
	}

	e, err := im.editFor(c)
	if err != nil {
		return err
	}
	if n.Action == svndump.ActionDelete || n.Action == svndump.ActionReplace {
		if err := im.remove(e, c.rel); err != nil {
			return err
		}
		if n.Action == svndump.ActionDelete {
			return nil
		}
	}
	if n.Kind == svndump.KindDir {
		if n.CopyFromRev >= 0 {
			src, err := im.copySource(n)
			if err != nil {
				return err
			}
			if src.Kind != object.KindDir {
				return fmt.Errorf("the copy source %s@%d is not a directory", n.CopyFromPath, n.CopyFromRev)
			}
			if err := im.flush(e); err != nil {
				return err
			}
			if e.root, err = im.rp.EditTree(im.ctx, e.root, []repo.Change{{Op: repo.OpPut, Path: c.rel, Entry: src}}); err != nil {
				return err
			}
		}
		// A directory without a copy exists implicitly, by its files.
		im.dirProps(n)
		return nil
	}
	return im.file(e, c.rel, n)
}

// fileState is what F1 keeps of a file's SVN properties.
type fileState struct {
	exec, special bool
	contentType   string
}

func (im *importer) file(e *edit, rel string, n *svndump.Node) error {
	var start *object.Entry
	switch {
	case n.Action == svndump.ActionChange:
		var err error
		if start, err = im.stat(e, rel); err != nil {
			return err
		}
		if start == nil || start.Kind == object.KindDir {
			return errors.New("a change to a file that does not exist")
		}
	case n.CopyFromRev >= 0:
		src, err := im.copySource(n)
		if err != nil {
			return err
		}
		if src.Kind == object.KindDir {
			return fmt.Errorf("the copy source %s@%d is a directory", n.CopyFromPath, n.CopyFromRev)
		}
		start = &src
	}
	var st, was fileState
	if start != nil {
		was.exec = start.Kind == object.KindExec
		was.special = start.Kind == object.KindSymlink
		if start.Attrs != nil {
			was.contentType = start.Attrs.ContentType
		}
	}
	st = was
	if n.Props != nil {
		if !n.PropDelta {
			st = fileState{}
		}
		for k, v := range n.Props {
			switch k {
			case "svn:executable":
				st.exec = true
			case "svn:special":
				st.special = true
			case "svn:mime-type":
				st.contentType = v
			case "svn:needs-lock":
				im.needsLock[lockGlob(n.Path)] = true
			default:
				im.rep.DroppedProps[k]++
			}
		}
		for _, k := range n.DeletedProps {
			switch k {
			case "svn:executable":
				st.exec = false
			case "svn:special":
				st.special = false
			case "svn:mime-type":
				st.contentType = ""
			}
		}
	}

	var target hash.Hash
	var err error
	switch {
	case n.HasText:
		target, err = im.text(n, start, st.special)
	case start != nil && was.special == st.special:
		target = start.Target
	case start != nil:
		target, err = im.reform(start, st.special)
	default:
		target, err = im.ingest(bytes.NewReader(nil), 0)
	}
	if err != nil {
		return err
	}
	ent := &object.Entry{Kind: object.KindFile, Target: target}
	switch {
	case st.special:
		ent.Kind = object.KindSymlink
	case st.exec:
		ent.Kind = object.KindExec
	}
	if st.contentType != "" {
		ent.Attrs = &object.Attrs{ContentType: st.contentType}
	}
	e.files[rel] = ent
	return nil
}

// text ingests a node's text, applying a delta against start's content if
// it is one, and checks the dump's checksums. A special file's text is
// "link <target>"; F1 stores the target.
func (im *importer) text(n *svndump.Node, start *object.Entry, special bool) (hash.Hash, error) {
	check := svndump.NewChecker(n.TextContentMD5, n.TextContentSHA1)
	src, size := n.Text, n.TextLength
	var wait func() error
	if n.TextDelta {
		base, err := im.base(start, n.TextDeltaBaseMD5)
		if err != nil {
			return hash.Zero, err
		}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := svndump.Apply(pw, n.Text, base)
			pw.CloseWithError(err)
			done <- err
		}()
		wait = func() error {
			pr.CloseWithError(errors.New("svnimport: the text was not read to its end"))
			return <-done
		}
		src, size = pr, -1
	}
	src = io.TeeReader(src, check)
	var target hash.Hash
	var err error
	if special {
		var link []byte
		if link, err = io.ReadAll(io.LimitReader(src, maxSymlink+1)); err == nil {
			target, err = im.symlink(link)
		}
	} else {
		target, err = im.ingest(src, size)
	}
	if wait != nil {
		if werr := wait(); err == nil {
			err = werr
		}
	}
	if err != nil {
		return hash.Zero, err
	}
	return target, check.Check()
}

// base returns the delta base: start's content in its SVN form, checked
// against the dump's MD5 when it gives one.
func (im *importer) base(start *object.Entry, wantMD5 string) (io.ReaderAt, error) {
	if start == nil {
		return bytes.NewReader(nil), nil
	}
	if start.Kind == object.KindSymlink {
		r, err := im.rp.OpenFile(im.ctx, start.Target)
		if err != nil {
			return nil, err
		}
		target, err := io.ReadAll(io.LimitReader(r, maxSymlink))
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(append([]byte("link "), target...)), nil
	}
	if wantMD5 != "" {
		d, err := im.rp.Digests(im.ctx, start.Target)
		if err != nil {
			return nil, err
		}
		if got := hex.EncodeToString(d.MD5[:]); got != wantMD5 {
			return nil, fmt.Errorf("the delta base has MD5 %s, the dump expects %s", got, wantMD5)
		}
	}
	return im.rp.OpenFile(im.ctx, start.Target)
}

// reform rewrites a file whose svn:special property changed without a new
// text: between "link <target>" and the bare target.
func (im *importer) reform(start *object.Entry, special bool) (hash.Hash, error) {
	r, err := im.rp.OpenFile(im.ctx, start.Target)
	if err != nil {
		return hash.Zero, err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxSymlink+1))
	if err != nil {
		return hash.Zero, err
	}
	if special {
		return im.symlink(data)
	}
	return im.ingest(io.MultiReader(strings.NewReader("link "), bytes.NewReader(data)), int64(len(data))+5)
}

func (im *importer) symlink(text []byte) (hash.Hash, error) {
	target, ok := bytes.CutPrefix(text, []byte("link "))
	if !ok || len(text) > maxSymlink {
		return hash.Zero, errors.New(`an svn:special file that is not "link <target>"`)
	}
	return im.ingest(bytes.NewReader(target), int64(len(target)))
}

func (im *importer) ingest(r io.Reader, size int64) (hash.Hash, error) {
	res, err := im.rp.Ingest(im.ctx, r, size)
	if err != nil {
		return hash.Zero, err
	}
	im.rep.Bytes += int64(res.Size)
	im.sinceBytes += int64(res.Size)
	return res.File, nil
}

// copySource returns the entry a node copies.
func (im *importer) copySource(n *svndump.Node) (object.Entry, error) {
	c, err := im.lay.classify(n.CopyFromPath)
	if err != nil {
		return object.Entry{}, fmt.Errorf("copy source: %w", err)
	}
	if c.kind == container {
		return object.Entry{}, fmt.Errorf("copying %s, a directory of the layout, is not supported", n.CopyFromPath)
	}
	commit := im.at(c.ref, n.CopyFromRev)
	if commit.IsZero() {
		return object.Entry{}, fmt.Errorf("the copy source %s@%d is not in an imported branch", n.CopyFromPath, n.CopyFromRev)
	}
	root, err := im.tree(commit)
	if err != nil {
		return object.Entry{}, err
	}
	if c.kind == refRoot {
		return object.Entry{Kind: object.KindDir, Target: root}, nil
	}
	e, err := im.rp.StatTree(im.ctx, root, c.rel)
	if errors.Is(err, repo.ErrNotFound) {
		return object.Entry{}, fmt.Errorf("the copy source %s@%d does not exist", n.CopyFromPath, n.CopyFromRev)
	}
	return e, err
}

// at returns the commit of an SVN ref path as of a revision, or zero.
func (im *importer) at(refPath string, rev int) hash.Hash {
	marks := im.revs[refPath]
	i := sort.Search(len(marks), func(i int) bool { return marks[i].rev > rev })
	if i == 0 {
		return hash.Zero
	}
	return marks[i-1].commit
}

func (im *importer) tree(commit hash.Hash) (hash.Hash, error) {
	if t, ok := im.trees[commit]; ok {
		return t, nil
	}
	t, err := im.rp.CommitTree(im.ctx, commit)
	if err == nil && !commit.IsZero() {
		im.trees[commit] = t
	}
	return t, err
}

// editFor returns the edit of the ref holding c, starting one if needed.
func (im *importer) editFor(c class) (*edit, error) {
	if e := im.edits[c.ref]; e != nil {
		return e, nil
	}
	h := im.heads[c.ref]
	if h == nil {
		if !im.lay.root {
			return nil, fmt.Errorf("%s is in a branch or tag that does not exist", c.rel)
		}
		h = &head{name: "heads/" + im.defaultBranch, created: im.rev}
		im.heads[c.ref] = h
		im.names[h.name] = c.ref
	}
	root, err := im.tree(h.commit)
	if err != nil {
		return nil, err
	}
	e := &edit{parent: h.commit, root: root, files: map[string]*object.Entry{}}
	if h.tag && h.created < im.rev {
		im.rep.TagChanges = append(im.rep.TagChanges, fmt.Sprintf("%s@%d", c.ref, im.rev))
	}
	im.edits[c.ref] = e
	im.touched[c.ref] = true
	return e, nil
}

// createRef starts a new ref at an added branch or tag directory.
func (im *importer) createRef(c class, n *svndump.Node) error {
	if im.heads[c.ref] != nil {
		return errors.New("the branch or tag already exists")
	}
	name, err := im.refName(c)
	if err != nil {
		return err
	}
	e := &edit{files: map[string]*object.Entry{}}
	switch {
	case n.CopyFromRev < 0:
		e.root, err = im.tree(hash.Zero)
	default:
		src, cerr := im.lay.classify(n.CopyFromPath)
		if cerr != nil {
			return fmt.Errorf("copy source: %w", cerr)
		}
		if src.kind == refRoot {
			// A copy of a whole branch: the new ref starts at its commit.
			if e.parent = im.at(src.ref, n.CopyFromRev); e.parent.IsZero() {
				return fmt.Errorf("the copy source %s@%d is not in an imported branch", n.CopyFromPath, n.CopyFromRev)
			}
			e.root, err = im.tree(e.parent)
			break
		}
		var ent object.Entry
		if ent, err = im.copySource(n); err == nil && ent.Kind != object.KindDir {
			err = fmt.Errorf("the copy source %s@%d is not a directory", n.CopyFromPath, n.CopyFromRev)
		}
		e.root = ent.Target
	}
	if err != nil {
		return err
	}
	im.heads[c.ref] = &head{name: name, tag: c.tag, created: im.rev}
	im.names[name] = c.ref
	im.edits[c.ref] = e
	im.touched[c.ref] = true
	if c.renamed {
		im.rep.Renamed[c.ref] = name
	}
	return nil
}

// refName returns the F1 ref for a new branch or tag.
func (im *importer) refName(c class) (string, error) {
	name := c.refName(im.defaultBranch)
	if err := policy.ValidRefName(name); err != nil {
		return "", err
	}
	if other, ok := im.names[name]; ok && other != c.ref {
		return "", fmt.Errorf("%s and %s both map to ref %s", other, c.ref, name)
	}
	return name, nil
}

// deleteRef ends a live ref. Its commit stays reachable under
// svn-deleted/, named after the deleting revision.
func (im *importer) deleteRef(refPath string) {
	h := im.heads[refPath]
	if h == nil {
		return
	}
	delete(im.heads, refPath)
	delete(im.edits, refPath)
	delete(im.names, h.name)
	im.touched[refPath] = true
	if h.commit.IsZero() {
		delete(im.want, h.name) // created and deleted before any commit
		return
	}
	cat, short, _ := strings.Cut(h.name, "/")
	kept := fmt.Sprintf("%s/svn-deleted/%s-r%d", cat, short, im.rev)
	im.want[h.name] = hash.Zero
	im.want[kept] = h.commit
	im.rep.Deleted = append(im.rep.Deleted, kept)
}

// stat returns the entry at rel in an edit, with its pending files, or
// nil.
func (im *importer) stat(e *edit, rel string) (*object.Entry, error) {
	if ent, ok := e.files[rel]; ok {
		return ent, nil
	}
	for a := path.Dir(rel); a != "."; a = path.Dir(a) {
		if _, ok := e.files[a]; ok {
			return nil, nil // below a deleted file, or a file
		}
	}
	ent, err := im.rp.StatTree(im.ctx, e.root, rel)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ent, nil
}

// remove deletes a file or a directory from an edit.
func (im *importer) remove(e *edit, rel string) error {
	ent, err := im.stat(e, rel)
	if err != nil {
		return err
	}
	dir := ent != nil && ent.Kind == object.KindDir
	for p, f := range e.files {
		if f != nil && under(p, rel) && p != rel {
			dir = true
		}
	}
	if !dir {
		e.files[rel] = nil
		return nil
	}
	if err := im.flush(e); err != nil {
		return err
	}
	e.root, err = im.rp.EditTree(im.ctx, e.root, []repo.Change{{Op: repo.OpRemove, Path: rel}})
	return err
}

// flush writes an edit's pending files to its tree.
func (im *importer) flush(e *edit) error {
	if len(e.files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(e.files))
	for p := range e.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	changes := make([]repo.Change, len(paths))
	for i, p := range paths {
		if ent := e.files[p]; ent != nil {
			changes[i] = repo.Change{Op: repo.OpPut, Path: p, Entry: *ent}
		} else {
			changes[i] = repo.Change{Op: repo.OpDelete, Path: p}
		}
	}
	root, err := im.rp.EditTree(im.ctx, e.root, changes)
	if err != nil {
		return err
	}
	e.root = root
	clear(e.files)
	return nil
}

// endRevision writes the commits of the current revision and records
// where each touched ref is.
func (im *importer) endRevision() error {
	if im.rev <= 0 {
		return nil
	}
	refs := make([]string, 0, len(im.edits))
	for p := range im.edits {
		refs = append(refs, p)
	}
	sort.Strings(refs)
	for _, p := range refs {
		e, h := im.edits[p], im.heads[p]
		if err := im.flush(e); err != nil {
			return fmt.Errorf("svnimport: r%d %s: %w", im.rev, p, err)
		}
		commit := e.parent
		parentTree, err := im.tree(e.parent)
		if err != nil {
			return err
		}
		if e.parent.IsZero() || e.root != parentTree {
			if commit, err = im.commit(e); err != nil {
				return fmt.Errorf("svnimport: r%d %s: %w", im.rev, p, err)
			}
		}
		if commit != h.commit {
			h.commit = commit
			im.want[h.name] = commit
		}
	}
	touched := make([]string, 0, len(im.touched))
	for p := range im.touched {
		touched = append(touched, p)
	}
	sort.Strings(touched)
	for _, p := range touched {
		var c hash.Hash
		if h := im.heads[p]; h != nil {
			c = h.commit
		}
		im.revs[p] = append(im.revs[p], mark{rev: im.rev, commit: c})
	}
	im.rep.Revisions++
	im.rep.LastRevision = im.rev
	im.sinceRevs++
	if im.opts.Progress != nil {
		im.opts.Progress(im.rev)
	}
	if im.sinceRevs >= im.opts.CheckpointRevisions || im.sinceBytes >= im.opts.CheckpointBytes {
		return im.checkpoint()
	}
	return nil
}

// commit writes the commit of an edit for the current revision.
func (im *importer) commit(e *edit) (hash.Hash, error) {
	author := im.revProps["svn:author"]
	who, ok := im.opts.Authors[author]
	if !ok {
		if author == "" {
			author = "no-author"
		}
		im.authors[author] = true
		who = repo.Person{Name: author, Email: author + "@" + im.dr.UUID}
	}
	t := im.lastTime
	if d, ok := im.revProps["svn:date"]; ok {
		parsed, err := time.Parse(time.RFC3339Nano, d)
		if err != nil {
			return hash.Zero, fmt.Errorf("invalid svn:date %q", d)
		}
		t = parsed
	}
	im.lastTime = t
	msg := strings.TrimRight(im.revProps["svn:log"], " \t\r\n")
	if msg != "" {
		msg += "\n\n"
	}
	msg += "SVN-Revision: " + strconv.Itoa(im.rev) + "\n"
	if im.dr.UUID != "" {
		msg += "SVN-UUID: " + im.dr.UUID + "\n"
	}
	p := object.Person{Name: []byte(who.Name), Email: []byte(who.Email), Time: t.Unix(), TZ: "+0000"}
	c := &object.Commit{Tree: e.root, Author: p, Committer: p, Message: []byte(msg)}
	if !e.parent.IsZero() {
		c.Parents = []hash.Hash{e.parent}
	}
	h, err := im.rp.Put(im.ctx, c)
	if err != nil {
		return hash.Zero, err
	}
	im.trees[h] = e.root
	im.rep.Commits++
	return h, nil
}

// checkpoint moves the refs changed since the last checkpoint, in one
// atomic update.
func (im *importer) checkpoint() error {
	im.sinceRevs, im.sinceBytes = 0, 0
	names := make([]string, 0, len(im.want))
	for n := range im.want {
		names = append(names, n)
	}
	sort.Strings(names)
	var updates []repo.RefUpdate
	for _, n := range names {
		if old := im.committed[n]; old != im.want[n] {
			updates = append(updates, repo.RefUpdate{Ref: n, Old: old, New: im.want[n]})
		}
	}
	if len(updates) > 0 {
		// History is imported as it was: the lock policy applies to
		// edits made in F1.
		opts := repo.UpdateOptions{Principal: im.opts.Principal, Reason: fmt.Sprintf("svn import to r%d", im.rev), SkipLocks: true}
		if err := im.rp.UpdateRefs(im.ctx, updates, opts); err != nil {
			var ce *repo.ConflictError
			if errors.As(err, &ce) {
				return fmt.Errorf("svnimport: ref %s moved during the import", ce.Ref)
			}
			return fmt.Errorf("svnimport: r%d: %w", im.rev, err)
		}
	}
	for _, u := range updates {
		if u.New.IsZero() {
			delete(im.committed, u.Ref)
		} else {
			im.committed[u.Ref] = u.New
		}
	}
	clear(im.want)
	return nil
}

func (im *importer) dirProps(n *svndump.Node) {
	for k := range n.Props {
		switch k {
		case "svn:externals":
			im.rep.Externals = appendOnce(im.rep.Externals, n.Path)
		case "svn:ignore", "svn:global-ignores":
			im.rep.Ignores = appendOnce(im.rep.Ignores, n.Path)
		default:
			im.rep.DroppedProps[k]++
		}
	}
}

func (im *importer) finishReport() {
	im.rep.UUID = im.dr.UUID
	for p, h := range im.heads {
		im.rep.Refs[p] = h.name
	}
	for a := range im.authors {
		im.rep.UnknownAuthors = append(im.rep.UnknownAuthors, a)
	}
	sort.Strings(im.rep.UnknownAuthors)
	for g := range im.needsLock {
		im.rep.NeedsLock = append(im.rep.NeedsLock, g)
	}
	sort.Strings(im.rep.NeedsLock)
}

// lockGlob returns the glob a required-lock setting would use for a file:
// "*.ext", or the file name if it has no extension.
func lockGlob(p string) string {
	name := path.Base(p)
	if ext := path.Ext(name); ext != "" && ext != name {
		return "*" + ext
	}
	return name
}

func appendOnce(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}
