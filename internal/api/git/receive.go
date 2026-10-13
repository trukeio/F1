package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/serverdb"
	"github.com/trukeio/f1/internal/tree"
)

// receiveCaps are the capabilities advertised to pushes.
var receiveCaps = []string{
	"report-status", "side-band-64k", "atomic", "delete-refs", "ofs-delta", "quiet", "push-options",
	"object-format=sha1", agent,
}

// advertiseReceive answers GET info/refs?service=git-receive-pack.
func (s *Server) advertiseReceive(ctx context.Context, w http.ResponseWriter, rp *repo.Repo, version int) error {
	x := NewExporter(rp)
	refs, err := advertisedRefs(ctx, x, rp)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	p := &pktWriter{w: &buf}
	p.line("# service=git-receive-pack\n")
	p.flush()
	if version == 1 {
		p.line("version 1\n")
	}
	caps := strings.Join(receiveCaps, " ")
	for i, r := range refs {
		if i == 0 {
			p.line("%s %s\x00%s\n", r.sha, r.name, caps)
		} else {
			p.line("%s %s\n", r.sha, r.name)
		}
	}
	if len(refs) == 0 {
		p.line("%s capabilities^{}\x00%s\n", SHA{}, caps)
	}
	p.flush()
	if p.err != nil {
		return p.err
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_, err = w.Write(buf.Bytes())
	return err
}

// command is a ref update requested by a push.
type command struct {
	old, new SHA
	name     string // full Git name: refs/heads/main
	ref      string // F1 name: heads/main
	err      string // set when the update is rejected
	update   repo.RefUpdate
}

// pushSink stores the objects of a pushed pack: blobs through the
// importer (streamed into Ingest), other objects kept for import in
// dependency order.
type pushSink struct {
	im      *Importer
	rp      *repo.Repo
	objects map[SHA]pushedObject
}

type pushedObject struct {
	t    Type
	data []byte
}

func (ps *pushSink) blob(ctx context.Context, size int64, r io.Reader) (SHA, error) {
	return ps.im.BlobStream(ctx, r, size)
}

func (ps *pushSink) object(t Type, s SHA, data []byte) {
	ps.objects[s] = pushedObject{t, data}
}

// base returns a delta base that is not a resolved non-blob of the pack:
// a blob of this push, or an object of the repository (thin packs).
func (ps *pushSink) base(ctx context.Context, s SHA) (Type, int64, io.ReaderAt, error) {
	if o, ok := ps.objects[s]; ok {
		return o.t, int64(len(o.data)), bytesReaderAt(o.data), nil
	}
	imp, ok, err := ps.im.Known(ctx, s)
	if err != nil || !ok {
		return 0, 0, nil, err
	}
	switch imp.Type {
	case TypeBlob:
		rd, err := ps.rp.OpenFile(ctx, imp.F1)
		if err != nil {
			return 0, 0, nil, err
		}
		return TypeBlob, rd.Size(), rd, nil
	default:
		data, err := ps.im.x.Content(ctx, Object{Type: imp.Type, SHA: s, F1: imp.F1, Raw: imp.Raw})
		if err != nil {
			return 0, 0, nil, err
		}
		return imp.Type, int64(len(data)), bytesReaderAt(data), nil
	}
}

// receive answers POST git-receive-pack: commands, push options, a pack;
// the pack is imported (doc/plan.md, "Object mapping", import) and the
// refs updated with ACL and lock checks.
func (s *Server) receive(ctx context.Context, w http.ResponseWriter, body io.Reader, rp *repo.Repo, c *auth.Caller, g *auth.Grants) error {
	br := newBufReader(body)
	pr := &pktReader{r: br}
	var cmds []*command
	caps := map[string]bool{}
	for {
		kind, data, err := pr.next()
		if err != nil {
			return badRequest("%v", err)
		}
		if kind != pktData {
			break
		}
		line := string(data)
		if len(cmds) == 0 {
			if l, cs, ok := strings.Cut(line, "\x00"); ok {
				line = l
				for _, c := range strings.Fields(cs) {
					caps[c] = true
				}
			}
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return badRequest("malformed command %q", line)
		}
		old, err1 := ParseSHA(f[0])
		nw, err2 := ParseSHA(f[1])
		if err1 != nil || err2 != nil {
			return badRequest("malformed command %q", line)
		}
		cmds = append(cmds, &command{old: old, new: nw, name: f[2]})
	}
	if caps["push-options"] {
		for {
			kind, _, err := pr.next()
			if err != nil {
				return badRequest("%v", err)
			}
			if kind != pktData {
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	unpackErr := s.importPack(ctx, br, rp, cmds)
	if unpackErr == nil {
		s.updateRefs(ctx, rp, c, g, cmds, caps["atomic"])
	} else {
		s.Log.Info("git push rejected", "namespace", rp.Meta().UUID, "principal", c.Principal, "err", unpackErr)
		for _, cmd := range cmds {
			cmd.err = "unpacker error"
		}
	}
	if !caps["report-status"] {
		return nil
	}
	var buf bytes.Buffer
	p := &pktWriter{w: &buf}
	if unpackErr != nil {
		p.line("unpack %s\n", oneLine(unpackErr.Error()))
	} else {
		p.line("unpack ok\n")
	}
	for _, cmd := range cmds {
		if cmd.err == "" {
			p.line("ok %s\n", cmd.name)
		} else {
			p.line("ng %s %s\n", cmd.name, oneLine(cmd.err))
		}
	}
	p.flush()
	out := &pktWriter{w: w}
	if caps["side-band-64k"] {
		if unpackErr != nil {
			// Band 2 shows the reason on the client's terminal.
			out.write(append([]byte{2}, []byte("error: "+unpackErr.Error()+"\n")...))
		}
		(&sideband{p: out}).Write(buf.Bytes())
		out.flush()
		return out.err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

// importPack reads the pushed pack, if any, and imports its objects.
func (s *Server) importPack(ctx context.Context, body io.Reader, rp *repo.Repo, cmds []*command) error {
	allDeletes := true
	for _, c := range cmds {
		if !c.new.IsZero() {
			allDeletes = false
		}
	}
	if allDeletes {
		return nil
	}
	im := NewImporter(rp)
	sink := &pushSink{im: im, rp: rp, objects: map[SHA]pushedObject{}}
	if _, err := readPack(ctx, body, filepath.Join(rp.Path(), repodir.TmpDir), sink); err != nil {
		return err
	}
	if err := importObjects(ctx, im, sink.objects); err != nil {
		return err
	}
	for _, c := range cmds {
		if c.new.IsZero() {
			continue
		}
		o, ok, err := im.Known(ctx, c.new)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("missing object %s for %s", c.new, c.name)
		}
		c.update.New = o.F1
		if o.Type != TypeCommit && o.Type != TypeTag {
			c.err = fmt.Sprintf("%s is a %s; refs point to commits and tags", c.new, o.Type)
		} else if o.Type != TypeCommit && strings.HasPrefix(c.name, "refs/heads/") {
			c.err = "branches must point to commits"
		}
	}
	return nil
}

// importObjects imports the non-blob objects of a push in dependency
// order: trees after their subtrees, commits after their trees and
// parents, tags after their targets. Iterative: histories are deep.
func importObjects(ctx context.Context, im *Importer, objs map[SHA]pushedObject) error {
	shas := make([]SHA, 0, len(objs))
	for sha := range objs {
		shas = append(shas, sha)
	}
	sort.Slice(shas, func(i, j int) bool { return bytes.Compare(shas[i][:], shas[j][:]) < 0 })
	deps := func(sha SHA, o pushedObject) ([]SHA, error) {
		var out []SHA
		add := func(d SHA) {
			if _, ok := objs[d]; ok {
				out = append(out, d)
			}
		}
		switch o.t {
		case TypeTree:
			entries, err := ParseTree(o.data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode != ModeGitlink {
					add(e.SHA)
				}
			}
		case TypeCommit:
			pc, err := ParseCommit(o.data)
			if err != nil {
				return nil, fmt.Errorf("commit %s: %w", sha, err)
			}
			add(pc.Tree)
			for _, p := range pc.Parents {
				add(p)
			}
		case TypeTag:
			pt, err := ParseTag(o.data)
			if err != nil {
				return nil, fmt.Errorf("tag %s: %w", sha, err)
			}
			add(pt.Object)
		}
		return out, nil
	}
	done := map[SHA]bool{}
	for _, root := range shas {
		stack := []SHA{root}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			top := stack[len(stack)-1]
			if done[top] {
				stack = stack[:len(stack)-1]
				continue
			}
			o := objs[top]
			ds, err := deps(top, o)
			if err != nil {
				return err
			}
			pending := false
			for _, d := range ds {
				if !done[d] {
					stack = append(stack, d)
					pending = true
				}
			}
			if pending {
				if len(stack) > len(objs)+1 {
					return errors.New("object graph has a cycle")
				}
				continue
			}
			switch o.t {
			case TypeTree:
				err = im.Tree(ctx, top, o.data)
			case TypeCommit:
				err = im.Commit(ctx, top, o.data)
			case TypeTag:
				err = im.Tag(ctx, top, o.data)
			}
			if err != nil {
				return err
			}
			done[top] = true
			stack = stack[:len(stack)-1]
		}
	}
	return nil
}

// updateRefs checks and applies the ref updates of a push: CAS against the
// advertised value, ACLs (write on every changed path of a branch, write
// on the whole namespace for tags and deletions) and locks (in
// UpdateRefs). With atomic, all updates are applied or none.
func (s *Server) updateRefs(ctx context.Context, rp *repo.Repo, c *auth.Caller, g *auth.Grants, cmds []*command, atomic bool) {
	x := NewExporter(rp)
	refs := rp.Refs()
	var ok []*command
	for _, cmd := range cmds {
		if cmd.err != "" {
			continue
		}
		name, isRef := strings.CutPrefix(cmd.name, "refs/")
		if !isRef || policy.ValidRefName(name) != nil || !(strings.HasPrefix(name, "heads/") || strings.HasPrefix(name, "tags/")) {
			cmd.err = "invalid ref name"
			continue
		}
		cmd.ref = name
		cur := refs[name]
		var curSHA SHA
		if !cur.IsZero() {
			var err error
			if curSHA, _, err = x.RefTarget(ctx, cur); err != nil {
				cmd.err = "cannot read the current value"
				continue
			}
		}
		if curSHA != cmd.old {
			cmd.err = "fetch first (the ref changed on the server)"
			continue
		}
		cmd.update.Ref, cmd.update.Old = name, cur
		if cmd.new.IsZero() || !strings.HasPrefix(name, "heads/") {
			if !g.Whole(serverdb.PermWrite) {
				cmd.err = "permission denied: tags and deletions need write permission on the whole namespace"
				continue
			}
		}
		if !cmd.new.IsZero() && !cur.IsZero() && strings.HasPrefix(name, "heads/") {
			// Keep S3 attributes of unchanged files.
			nw, err := carryAttrs(ctx, x, rp, cur, cmd.update.New)
			if err != nil {
				cmd.err = "internal error"
				s.Log.Error("git push: carrying S3 attributes forward", "ref", name, "err", err)
				continue
			}
			cmd.update.New = nw
		}
		ok = append(ok, cmd)
	}
	if atomic && len(ok) != len(cmds) {
		for _, cmd := range ok {
			cmd.err = "atomic push failed: another ref was rejected"
		}
		return
	}
	opts := repo.UpdateOptions{Principal: c.Principal, Reason: "git push", CheckPath: func(_ string, pc repo.PathChange) error {
		if !g.Allow(pc.Path, serverdb.PermWrite) {
			return fmt.Errorf("permission denied: no write permission on %s", pc.Path)
		}
		return nil
	}}
	apply := func(cmds []*command) {
		updates := make([]repo.RefUpdate, len(cmds))
		for i, cmd := range cmds {
			updates[i] = cmd.update
		}
		if err := rp.UpdateRefs(ctx, updates, opts); err != nil {
			msg := refError(err)
			for _, cmd := range cmds {
				cmd.err = msg
			}
		}
	}
	if atomic {
		if len(ok) > 0 {
			apply(ok)
		}
		return
	}
	for _, cmd := range ok {
		apply([]*command{cmd})
	}
}

// refError is the reason shown to the Git client for a rejected update.
func refError(err error) string {
	var le *repo.LockedError
	var ce *repo.ConflictError
	switch {
	case errors.As(err, &le):
		if le.Owner == "" {
			return fmt.Sprintf("locked: %s needs a lock held by the pusher", le.Path)
		}
		return fmt.Sprintf("locked: %s is locked by %s", le.Path, le.Owner)
	case errors.As(err, &ce):
		return "fetch first (the ref changed on the server)"
	case errors.Is(err, repo.ErrFrozen):
		return "the repository is frozen for copying; retry later"
	case strings.HasPrefix(err.Error(), "permission denied"):
		return err.Error()
	}
	return "update failed: " + err.Error()
}

// carryAttrs returns the commit to store for a pushed branch head: the
// pushed commit, or an equal one (same Git id) whose tree reuses the old
// head's entries, with their S3 attributes, wherever the Git blob id and
// mode are unchanged (doc/plan.md, "S3 attrs carry forward").
func carryAttrs(ctx context.Context, x *Exporter, rp *repo.Repo, oldHead, newCommit hash.Hash) (hash.Hash, error) {
	oldRec, err := rp.Store().Get(ctx, oldHead)
	if err != nil || oldRec.Type != object.TypeCommit {
		return newCommit, err
	}
	rec, err := rp.Store().Get(ctx, newCommit)
	if err != nil || rec.Type != object.TypeCommit {
		return newCommit, err
	}
	oldC, err := rp.ReadCommit(ctx, oldHead)
	if err != nil {
		return hash.Zero, err
	}
	nc, err := rp.ReadCommit(ctx, newCommit)
	if err != nil {
		return hash.Zero, err
	}
	merged, err := mergeAttrs(ctx, x, rp, oldC.Tree, nc.Tree)
	if err != nil || merged == nc.Tree {
		return newCommit, err
	}
	want, err := x.CommitSHA(ctx, newCommit)
	if err != nil {
		return hash.Zero, err
	}
	c := *nc
	c.Tree = merged
	h, err := rp.Put(ctx, &c)
	if err != nil {
		return hash.Zero, err
	}
	if got, err := x.CommitSHA(ctx, h); err != nil || got != want {
		return hash.Zero, fmt.Errorf("carried commit exports as %s, want %s: %v", got, want, err)
	}
	return h, nil
}

// mergeAttrs returns the new tree with entries of the old tree wherever
// their Git form is identical.
func mergeAttrs(ctx context.Context, x *Exporter, rp *repo.Repo, oldRoot, newRoot hash.Hash) (hash.Hash, error) {
	if oldRoot == newRoot {
		return newRoot, nil
	}
	oldEntries, err := tree.All(ctx, rp.Nodes(), oldRoot)
	if err != nil {
		return hash.Zero, err
	}
	byName := map[string]object.Entry{}
	for _, e := range oldEntries {
		byName[string(e.Name)] = e
	}
	entries, err := tree.All(ctx, rp.Nodes(), newRoot)
	if err != nil {
		return hash.Zero, err
	}
	changed := false
	for i := range entries {
		e := &entries[i]
		o, ok := byName[string(e.Name)]
		if !ok || o.Target == e.Target && sameAttrs(o.Attrs, e.Attrs) {
			continue
		}
		if ModeForKind(&o) != ModeForKind(e) {
			continue
		}
		os, err1 := x.entrySHA(ctx, &o)
		ns, err2 := x.entrySHA(ctx, e)
		if err1 != nil || err2 != nil {
			return hash.Zero, errors.Join(err1, err2)
		}
		if os == ns {
			*e = o // the old entry, with its attributes
			changed = true
			continue
		}
		if o.Kind == object.KindDir && e.Kind == object.KindDir {
			sub, err := mergeAttrs(ctx, x, rp, o.Target, e.Target)
			if err != nil {
				return hash.Zero, err
			}
			if sub != e.Target {
				e.Target = sub
				changed = true
			}
		}
	}
	if !changed {
		return newRoot, nil
	}
	return rp.PutTree(ctx, entries)
}

func sameAttrs(a, b *object.Attrs) bool {
	if a == nil || b == nil {
		return a == b
	}
	ea, _ := object.Encode(&object.TreeNode{Entries: []object.Entry{{Name: []byte("x"), Kind: 1, Attrs: a}}})
	eb, _ := object.Encode(&object.TreeNode{Entries: []object.Entry{{Name: []byte("x"), Kind: 1, Attrs: b}}})
	return bytes.Equal(ea, eb)
}
