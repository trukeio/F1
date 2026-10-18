package svnimport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/svntest"
)

func TestClassify(t *testing.T) {
	std, err := newLayout(Layout{})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := newLayout(Layout{Trunk: "/proj/trunk/", Branches: []string{"proj/branches", "proj/releases"}, Tags: []string{"proj/tags"}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := newLayout(Layout{Root: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lay  layout
		path string
		want class
		err  string
	}{
		{std, "", class{kind: container}, ""},
		{std, "trunk", class{kind: refRoot, ref: "trunk", trunk: true}, ""},
		{std, "trunk/a/b.txt", class{kind: inRef, ref: "trunk", rel: "a/b.txt", trunk: true}, ""},
		{std, "branches", class{kind: container}, ""},
		{std, "branches/x", class{kind: refRoot, ref: "branches/x", name: "x"}, ""},
		{std, "branches/x/a.txt", class{kind: inRef, ref: "branches/x", rel: "a.txt", name: "x"}, ""},
		{std, "branches/my branch", class{kind: refRoot, ref: "branches/my branch", name: "my-branch", renamed: true}, ""},
		{std, "tags/v1/a", class{kind: inRef, ref: "tags/v1", rel: "a", tag: true, name: "v1"}, ""},
		{std, "README", class{}, "outside the layout"},
		{std, "trunkish", class{}, "outside the layout"},
		{custom, "proj", class{kind: container}, ""},
		{custom, "proj/trunk/x", class{kind: inRef, ref: "proj/trunk", rel: "x", trunk: true}, ""},
		{custom, "proj/releases/1.0", class{kind: refRoot, ref: "proj/releases/1.0", name: "1.0"}, ""},
		{custom, "trunk", class{}, "outside the layout"},
		{root, "", class{kind: container}, ""},
		{root, "trunk/a", class{kind: inRef, rel: "trunk/a", trunk: true}, ""},
	} {
		got, err := tc.lay.classify(tc.path)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: got %v, want an error with %q", tc.path, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %+v, %v; want %+v", tc.path, got, err, tc.want)
		}
	}
	for _, l := range []Layout{
		{Root: true, Trunk: "trunk"},
		{Trunk: "a", Branches: []string{"a/b"}},
		{Branches: []string{""}},
	} {
		if _, err := newLayout(l); err == nil {
			t.Errorf("layout %+v: no error", l)
		}
	}
}

func TestRefComponent(t *testing.T) {
	for in, want := range map[string]string{
		"feature":      "feature",
		"my branch":    "my-branch",
		"a:b?c*d[e\\f": "a-b-c-d-e-f",
		"x..y":         "x--y",
		".hidden":      "-hidden",
		"v1.lock":      "v1-lock",
		"end.":         "end-",
		"@":            "-",
		"at@{x}":       "at-{x}",
		"1.0":          "1.0",
	} {
		got := refComponent(in)
		if got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
		if err := policy.ValidRefName("heads/" + got); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	cfg, err := policy.Preset("svn")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "ns")
	if err := repo.Create(dir, cfg); err != nil {
		t.Fatal(err)
	}
	rp, _, err := repo.Open(context.Background(), dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rp.Close(context.Background()) })
	return rp
}

// incarnation is a ref that holds an SVN branch or tag for revisions
// [from, to).
type incarnation struct {
	path     string
	ref      string
	from, to int
}

// standardRefs are the refs of svntest.Standard after an import.
var standardRefs = []incarnation{
	{"trunk", "heads/main", 1, 1 << 30},
	{"branches/x", "heads/svn-deleted/x-r6", 3, 6},
	{"tags/v1", "tags/v1", 6, 1 << 30},
	{"branches/sub-only", "heads/svn-deleted/sub-only-r11", 8, 11},
	{"branches/sub-only", "heads/sub-only", 11, 1 << 30},
	{"branches/my branch", "heads/my-branch", 8, 1 << 30},
}

func TestImport(t *testing.T) {
	r, youngest := svntest.Standard(t)
	dir := filepath.Dir(r.Dir)
	uuid := strings.TrimSpace(string(svntest.Run(t, dir, "svnlook", "uuid", r.Dir)))
	for _, tc := range []struct {
		name       string
		args       []string
		checkpoint int
	}{
		{"svnadmin", []string{"svnadmin", "dump", "-q", r.Dir}, 0},
		{"svnadmin --deltas", []string{"svnadmin", "dump", "-q", "--deltas", r.Dir}, 5},
		{"svnrdump", []string{"svnrdump", "dump", "-q", r.URL}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svntest.Require(t, tc.args[0])
			dump := svntest.Run(t, dir, tc.args[0], tc.args[1:]...)
			rp := newRepo(t)
			var progress []int
			rep, err := Import(context.Background(), rp, bytes.NewReader(dump), Options{
				Authors:             map[string]repo.Person{"alice": {Name: "Alice Example", Email: "alice@example.com"}},
				CheckpointRevisions: tc.checkpoint,
				Progress:            func(rev int) { progress = append(progress, rev) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(progress) != youngest || rep.Revisions != youngest || rep.LastRevision != youngest || rep.UUID != uuid {
				t.Fatalf("progress %v, report %+v", progress, rep)
			}
			checkReport(t, rep)
			wantRefs := map[string]bool{}
			for _, inc := range standardRefs {
				wantRefs[inc.ref] = true
			}
			if got := slices.Sorted(maps.Keys(rp.Refs())); !slices.Equal(got, slices.Sorted(maps.Keys(wantRefs))) {
				t.Fatalf("refs %v", got)
			}
			for rev := 1; rev <= youngest; rev++ {
				checkRevision(t, r, rp, rev, standardRefs)
			}
			checkCommit(t, rp, uuid)
		})
	}
}

func checkReport(t *testing.T, rep *Report) {
	t.Helper()
	wantRefs := map[string]string{"trunk": "heads/main", "tags/v1": "tags/v1", "branches/sub-only": "heads/sub-only", "branches/my branch": "heads/my-branch"}
	if !maps.Equal(rep.Refs, wantRefs) {
		t.Errorf("refs %v", rep.Refs)
	}
	if !maps.Equal(rep.Renamed, map[string]string{"branches/my branch": "heads/my-branch"}) {
		t.Errorf("renamed %v", rep.Renamed)
	}
	if !slices.Equal(rep.Deleted, []string{"heads/svn-deleted/x-r6", "heads/svn-deleted/sub-only-r11"}) {
		t.Errorf("deleted %v", rep.Deleted)
	}
	if !slices.Equal(rep.TagChanges, []string{"tags/v1@10"}) {
		t.Errorf("tag changes %v", rep.TagChanges)
	}
	if !slices.Equal(rep.UnknownAuthors, []string{"bob"}) {
		t.Errorf("unknown authors %v", rep.UnknownAuthors)
	}
	if !slices.Equal(rep.NeedsLock, []string{"*.brd"}) {
		t.Errorf("needs-lock %v", rep.NeedsLock)
	}
	if rep.Commits == 0 || rep.Bytes < 300<<10 {
		t.Errorf("commits %d, bytes %d", rep.Commits, rep.Bytes)
	}
}

// checkRevision compares, for every branch and tag that exists at rev,
// svn export with F1's tree at the commit holding that revision.
func checkRevision(t *testing.T, r *svntest.Repo, rp *repo.Repo, rev int, refs []incarnation) {
	t.Helper()
	dir := filepath.Dir(r.Dir)
	var svnRefs []string
	for line := range strings.SplitSeq(string(svntest.Run(t, dir, "svnlook", "tree", "--full-paths", "-r", strconv.Itoa(rev), r.Dir)), "\n") {
		p := strings.TrimSuffix(line, "/")
		if p == "trunk" || strings.Count(p, "/") == 1 && (strings.HasPrefix(p, "branches/") || strings.HasPrefix(p, "tags/")) {
			svnRefs = append(svnRefs, p)
		}
	}
	var live []string
	for _, inc := range refs {
		if inc.from <= rev && rev < inc.to {
			live = append(live, inc.path)
		}
	}
	slices.Sort(svnRefs)
	slices.Sort(live)
	if !slices.Equal(svnRefs, live) {
		t.Fatalf("r%d: SVN has %v, the test expects %v", rev, svnRefs, live)
	}
	for _, inc := range refs {
		if inc.from > rev || rev >= inc.to {
			continue
		}
		commit := commitAt(t, rp, rp.Ref(inc.ref), rev)
		export := filepath.Join(t.TempDir(), "export")
		svntest.Run(t, dir, "svn", "export", "-q", r.URL+"/"+inc.path+"@"+strconv.Itoa(rev), export)
		want := exported(t, export)
		got := f1Files(t, rp, commit)
		if d := diffFiles(got, want); d != "" {
			t.Fatalf("r%d %s (%s), F1 against svn export:\n%s", rev, inc.path, inc.ref, d)
		}
	}
}

// diffFiles describes how two file sets differ, path by path, or returns
// "" if they are the same.
func diffFiles(got, want map[string]string) string {
	short := func(v string) string {
		kind, data, _ := strings.Cut(v, ":")
		if len(data) > 40 {
			return fmt.Sprintf("%s, %d bytes", kind, len(data))
		}
		return fmt.Sprintf("%s %q", kind, data)
	}
	var b strings.Builder
	for _, p := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for k := range got {
			yield(k, true)
		}
		for k := range want {
			yield(k, true)
		}
	}))) {
		g, gok := got[p]
		w, wok := want[p]
		switch {
		case !gok:
			fmt.Fprintf(&b, "  %s: missing (want %s)\n", p, short(w))
		case !wok:
			fmt.Fprintf(&b, "  %s: unexpected (%s)\n", p, short(g))
		case g != w:
			fmt.Fprintf(&b, "  %s: %s, want %s\n", p, short(g), short(w))
		}
	}
	return b.String()
}

var svnRevision = regexp.MustCompile(`(?m)^SVN-Revision: (\d+)$`)

// commitAt returns the first commit on the first-parent history of start
// whose SVN revision is at most rev.
func commitAt(t *testing.T, rp *repo.Repo, start hash.Hash, rev int) hash.Hash {
	t.Helper()
	for h := start; !h.IsZero(); {
		c, err := rp.ReadCommit(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		m := svnRevision.FindSubmatch(c.Message)
		if m == nil {
			t.Fatalf("commit %s has no SVN-Revision", h.Short())
		}
		if n, _ := strconv.Atoi(string(m[1])); n <= rev {
			return h
		}
		if len(c.Parents) == 0 {
			break
		}
		h = c.Parents[0]
	}
	t.Fatalf("no commit for r%d", rev)
	return hash.Zero
}

// exported describes the files of an export: "file:<content>",
// "exec:<content>" or "link:<target>". Directories are left out, as F1
// keeps no empty ones.
func exported(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link:" + target
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			kind := "file:"
			if info.Mode()&0o111 != 0 {
				kind = "exec:"
			}
			out[rel] = kind + string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func f1Files(t *testing.T, rp *repo.Repo, commit hash.Hash) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	err := rp.List(ctx, commit, repo.ListOpts{}, func(it repo.ListItem) error {
		r, err := rp.OpenFile(ctx, it.Entry.Target)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		kind := map[object.Kind]string{object.KindFile: "file:", object.KindExec: "exec:", object.KindSymlink: "link:"}[it.Entry.Kind]
		if kind == "" {
			t.Fatalf("%s has kind %d", it.Path, it.Entry.Kind)
		}
		out[it.Path] = kind + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkCommit checks the metadata of trunk's first commit and the MIME
// type of board.brd.
func checkCommit(t *testing.T, rp *repo.Repo, uuid string) {
	t.Helper()
	ctx := context.Background()
	first := commitAt(t, rp, rp.Ref("heads/main"), 1)
	c, err := rp.ReadCommit(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Author.Name) != "Alice Example" || string(c.Author.Email) != "alice@example.com" || c.Committer.Time != c.Author.Time {
		t.Errorf("author %+v, committer %+v", c.Author, c.Committer)
	}
	if d := time.Since(time.Unix(c.Author.Time, 0)); d < 0 || d > time.Hour {
		t.Errorf("author time %v", time.Unix(c.Author.Time, 0))
	}
	if want := "r1: initial\n\nSVN-Revision: 1\nSVN-UUID: " + uuid + "\n"; string(c.Message) != want {
		t.Errorf("message %q", c.Message)
	}
	if len(c.Parents) != 0 {
		t.Errorf("parents %v", c.Parents)
	}
	e, err := rp.Stat(ctx, first, "board.brd")
	if err != nil {
		t.Fatal(err)
	}
	if e.Attrs == nil || e.Attrs.ContentType != "application/octet-stream" {
		t.Errorf("board.brd attrs %+v", e.Attrs)
	}
	branch := commitAt(t, rp, rp.Ref("heads/svn-deleted/x-r6"), 3)
	if bc, err := rp.ReadCommit(ctx, branch); err != nil || len(bc.Parents) != 1 || bc.Parents[0] != commitAt(t, rp, rp.Ref("heads/main"), 2) {
		t.Errorf("branch x's commit does not follow trunk@2: %+v, %v", bc, err)
	}
}

func TestImportRoot(t *testing.T) {
	r, youngest := svntest.Standard(t)
	dir := filepath.Dir(r.Dir)
	dump := svntest.Run(t, dir, "svnadmin", "dump", "-q", "--deltas", r.Dir)
	rp := newRepo(t)
	rep, err := Import(context.Background(), rp, bytes.NewReader(dump), Options{Layout: Layout{Root: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(rep.Refs, map[string]string{"": "heads/main"}) || rep.Commits != youngest {
		t.Fatalf("report %+v", rep)
	}
	for rev := 1; rev <= youngest; rev++ {
		export := filepath.Join(t.TempDir(), "export")
		svntest.Run(t, dir, "svn", "export", "-q", r.URL+"@"+strconv.Itoa(rev), export)
		want := exported(t, export)
		if d := diffFiles(f1Files(t, rp, commitAt(t, rp, rp.Ref("heads/main"), rev)), want); d != "" {
			t.Fatalf("r%d, F1 against svn export:\n%s", rev, d)
		}
	}
}

func TestImportErrors(t *testing.T) {
	r := svntest.New(t)
	r.Svn("-q", "mkdir", "trunk")
	r.Write("README", []byte("outside\n"))
	r.Svn("-q", "add", "README")
	r.Commit("alice", "r1")
	dump := svntest.Run(t, filepath.Dir(r.Dir), "svnadmin", "dump", "-q", r.Dir)
	_, err := Import(context.Background(), newRepo(t), bytes.NewReader(dump), Options{})
	if err == nil || !strings.Contains(err.Error(), "r1 README") || !strings.Contains(err.Error(), "--root") {
		t.Fatalf("file outside the layout: %v", err)
	}

	rp := newRepo(t)
	if _, err := Import(context.Background(), rp, bytes.NewReader(dump), Options{Layout: Layout{Root: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), rp, bytes.NewReader(dump), Options{Layout: Layout{Root: true}}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second import: %v", err)
	}
}

// dumpWriter writes a format 3 dump by hand, for cases the svn client
// does not produce.
type dumpWriter struct{ b strings.Builder }

func newDump() *dumpWriter {
	d := &dumpWriter{}
	d.b.WriteString("SVN-fs-dump-format-version: 3\n\nUUID: 00000000-0000-0000-0000-000000000001\n\n")
	return d
}

func propBlock(props map[string]string, deleted []string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(props)) {
		fmt.Fprintf(&b, "K %d\n%s\nV %d\n%s\n", len(k), k, len(props[k]), props[k])
	}
	for _, k := range deleted {
		fmt.Fprintf(&b, "D %d\n%s\n", len(k), k)
	}
	b.WriteString("PROPS-END\n")
	return b.String()
}

func (d *dumpWriter) revision(n int, log string) {
	p := propBlock(map[string]string{"svn:log": log, "svn:author": "alice", "svn:date": "2026-10-18T10:00:00.000000Z"}, nil)
	fmt.Fprintf(&d.b, "Revision-number: %d\nProp-content-length: %d\nContent-length: %d\n\n%s\n", n, len(p), len(p), p)
}

// node writes a node; props nil means no property block, text nil no
// text. A non-nil deleted list makes the block a delta.
func (d *dumpWriter) node(path, kind, action string, props map[string]string, deleted []string, text *string) {
	fmt.Fprintf(&d.b, "Node-path: %s\nNode-kind: %s\nNode-action: %s\n", path, kind, action)
	var content string
	if props != nil || deleted != nil {
		p := propBlock(props, deleted)
		if deleted != nil {
			d.b.WriteString("Prop-delta: true\n")
		}
		fmt.Fprintf(&d.b, "Prop-content-length: %d\n", len(p))
		content = p
	}
	if text != nil {
		fmt.Fprintf(&d.b, "Text-content-length: %d\n", len(*text))
		content += *text
	}
	fmt.Fprintf(&d.b, "Content-length: %d\n\n%s\n\n", len(content), content)
}

func TestImportSpecialWithoutText(t *testing.T) {
	text := "link a.txt"
	d := newDump()
	d.revision(1, "add")
	d.node("trunk", "dir", "add", map[string]string{}, nil, nil)
	d.node("trunk/l", "file", "add", map[string]string{}, nil, &text)
	d.revision(2, "make it a symlink")
	d.node("trunk/l", "file", "change", map[string]string{"svn:special": "*"}, []string{}, nil)
	d.revision(3, "make it a file again")
	d.node("trunk/l", "file", "change", map[string]string{}, []string{"svn:special"}, nil)
	rp := newRepo(t)
	if _, err := Import(context.Background(), rp, strings.NewReader(d.b.String()), Options{}); err != nil {
		t.Fatal(err)
	}
	for rev, want := range map[int]string{1: "file:link a.txt", 2: "link:a.txt", 3: "file:link a.txt"} {
		if got := f1Files(t, rp, commitAt(t, rp, rp.Ref("heads/main"), rev))["l"]; got != want {
			t.Errorf("r%d: %q, want %q", rev, got, want)
		}
	}
}
