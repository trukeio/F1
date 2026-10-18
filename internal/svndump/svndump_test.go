package svndump

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/svntest"
)

func appendVarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(v & 0x7f)
	for v >>= 7; v > 0; v >>= 7 {
		i--
		tmp[i] = byte(v&0x7f) | 0x80
	}
	return append(b, tmp[i:]...)
}

// op encodes one instruction; off is ignored for new-data copies.
func op(code byte, n, off int) []byte {
	var b []byte
	if n < 64 {
		b = []byte{code<<6 | byte(n)}
	} else {
		b = appendVarint([]byte{code << 6}, uint64(n))
	}
	if code != 2 {
		b = appendVarint(b, uint64(off))
	}
	return b
}

// window encodes one window. In version 1, compress chooses zlib for both
// sections.
func window(version byte, compress bool, sOff, sLen, tLen int, ins, nd []byte) []byte {
	section := func(s []byte) []byte {
		if version == 0 {
			return s
		}
		out := appendVarint(nil, uint64(len(s)))
		if !compress {
			return append(out, s...)
		}
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(s)
		zw.Close()
		return append(out, z.Bytes()...)
	}
	is, ns := section(ins), section(nd)
	b := appendVarint(nil, uint64(sOff))
	for _, v := range []int{sLen, tLen, len(is), len(ns)} {
		b = appendVarint(b, uint64(v))
	}
	return append(append(b, is...), ns...)
}

func delta(version byte, windows ...[]byte) []byte {
	return slices.Concat(append([][]byte{{'S', 'V', 'N', version}}, windows...)...)
}

func apply(t *testing.T, d []byte, base []byte) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	var b io.ReaderAt
	if base != nil {
		b = bytes.NewReader(base)
	}
	n, err := Apply(&out, bytes.NewReader(d), b)
	if err == nil && n != int64(out.Len()) {
		t.Fatalf("Apply returned %d, wrote %d", n, out.Len())
	}
	return out.Bytes(), err
}

func TestApply(t *testing.T) {
	long := bytes.Repeat([]byte("0123456789"), 20) // 200 bytes: varint lengths
	base := []byte("hello, world")
	for _, tc := range []struct {
		name  string
		delta []byte
		base  []byte
		want  string
	}{
		{"empty", delta(0), nil, ""},
		{"new data", delta(0, window(0, false, 0, 0, 5, op(2, 5, 0), []byte("abcde"))), nil, "abcde"},
		{"source copy", delta(0, window(0, false, 7, 5, 5, op(0, 5, 0), nil)), base, "world"},
		{"target copy repeats", delta(0, window(0, false, 0, 0, 7, slices.Concat(op(2, 2, 0), op(1, 5, 0)), []byte("ab"))), nil, "abababa"},
		{"mixed", delta(0, window(0, false, 0, 12, 13, slices.Concat(op(0, 7, 0), op(2, 1, 0), op(0, 5, 7)), []byte("W"))), base, "hello, Wworld"},
		{"long instruction", delta(0, window(0, false, 0, 0, 200, op(2, 200, 0), long)), nil, string(long)},
		{"two windows", delta(0,
			window(0, false, 0, 5, 5, op(0, 5, 0), nil),
			window(0, false, 5, 7, 3, op(2, 3, 0), []byte("!!!"))), base, "hello!!!"},
		{"version 1 raw", delta(1, window(1, false, 0, 0, 200, op(2, 200, 0), long)), nil, string(long)},
		{"version 1 zlib", delta(1, window(1, true, 0, 0, 200, op(2, 200, 0), long)), nil, string(long)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := apply(t, tc.delta, tc.base)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	base := []byte("hello")
	for _, tc := range []struct {
		name, want  string
		delta, base []byte
	}{
		{"not svndiff", "not an svndiff", []byte("GIT\x00"), nil},
		{"short header", "header", []byte("SV"), nil},
		{"version 3", "unknown version", []byte("SVN\x03"), nil},
		{"past target", "past the target", delta(0, window(0, false, 0, 0, 2, op(2, 3, 0), []byte("abc"))), nil},
		{"short target", "fill 2 of 3", delta(0, window(0, false, 0, 0, 3, op(2, 2, 0), []byte("ab"))), nil},
		{"past source", "past the source", delta(0, window(0, false, 0, 5, 5, op(0, 5, 1), nil)), base},
		{"base too short", "the base ends", delta(0, window(0, false, 2, 5, 5, op(0, 5, 0), nil)), base},
		{"no base", "needs a base", delta(0, window(0, false, 0, 5, 5, op(0, 5, 0), nil)), nil},
		{"target copy ahead", "past the target written", delta(0, window(0, false, 0, 0, 2, op(1, 2, 0), nil)), nil},
		{"past new data", "past the new data", delta(0, window(0, false, 0, 0, 3, op(2, 3, 0), []byte("ab"))), nil},
		{"unused new data", "use 2 of 3", delta(0, window(0, false, 0, 0, 2, op(2, 2, 0), []byte("abc"))), nil},
		{"invalid opcode", "invalid instruction", delta(0, window(0, false, 0, 0, 1, []byte{3<<6 | 1}, nil)), nil},
		{"truncated window", "unexpected EOF", delta(0, window(0, false, 0, 0, 3, op(2, 3, 0), []byte("abc")))[:9], nil},
		{"huge window", "too large", delta(0, appendVarint(appendVarint([]byte{0, 0}, 1<<30), 0)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apply(t, tc.delta, tc.base)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	if _, err := apply(t, []byte("SVN\x02"), nil); !errors.Is(err, ErrLZ4) {
		t.Fatalf("version 2: %v", err)
	}
}

const handDump = "SVN-fs-dump-format-version: 3\n\n" +
	"UUID: 6e928293-9d1f-4e2a-b325-054988abf0b9\n\n" +
	"Revision-number: 0\nProp-content-length: 56\nContent-length: 56\n\n" +
	"K 8\nsvn:date\nV 27\n2026-10-18T10:00:00.000000Z\nPROPS-END\n\n" +
	"Revision-number: 1\nProp-content-length: 38\nContent-length: 38\n\n" +
	"K 7\nsvn:log\nV 10\ntwo\nlines\n\nPROPS-END\n\n" +
	"Node-path: trunk\nNode-kind: dir\nNode-action: add\nProp-content-length: 10\nContent-length: 10\n\nPROPS-END\n\n\n" +
	"Node-path: trunk/a.txt\nNode-kind: file\nNode-action: add\nProp-content-length: 38\nText-content-length: 6\n" +
	"Text-content-md5: b1946ac92492d2347c6235b4d2611184\nContent-length: 44\n\n" +
	"K 13\nsvn:mime-type\nV 4\ntext\nPROPS-END\nhello\n\n\n" +
	"Node-path: trunk/b.txt\nNode-kind: file\nNode-action: add\nText-content-length: 5\nContent-length: 5\n\nskip!\n\n" +
	"Revision-number: 2\nProp-content-length: 10\nContent-length: 10\n\nPROPS-END\n\n" +
	"Node-path: trunk/a.txt\nNode-kind: file\nNode-action: change\nProp-delta: true\nProp-content-length: 29\nContent-length: 29\n\n" +
	"D 13\nsvn:mime-type\nPROPS-END\n\n\n" +
	"Node-path: branches/x\nNode-kind: dir\nNode-action: add\nNode-copyfrom-rev: 1\nNode-copyfrom-path: trunk\n\n\n" +
	"Node-path: trunk/b.txt\nNode-action: delete\n\n\n"

func TestReader(t *testing.T) {
	r, err := NewReader(strings.NewReader(handDump))
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 3 {
		t.Fatalf("version %d", r.Version)
	}
	var recs []Record
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n, ok := rec.(*Node); ok && n.Path == "trunk/a.txt" && n.HasText {
			text, _ := io.ReadAll(n.Text)
			c := NewChecker(n.TextContentMD5, "")
			c.Write(text)
			if string(text) != "hello\n" || c.Check() != nil {
				t.Fatalf("text %q, check %v", text, c.Check())
			}
		}
		recs = append(recs, rec) // trunk/b.txt's text is left unread
	}
	if r.UUID != "6e928293-9d1f-4e2a-b325-054988abf0b9" {
		t.Fatalf("UUID %q", r.UUID)
	}
	if len(recs) != 9 {
		t.Fatalf("%d records", len(recs))
	}
	rev1 := recs[1].(*Revision)
	if rev1.Number != 1 || rev1.Props["svn:log"] != "two\nlines\n" {
		t.Fatalf("revision 1: %+v", rev1)
	}
	trunk := recs[2].(*Node)
	if trunk.Kind != KindDir || trunk.Action != ActionAdd || trunk.Props == nil || len(trunk.Props) != 0 || trunk.HasText || trunk.CopyFromRev != -1 {
		t.Fatalf("trunk: %+v", trunk)
	}
	a := recs[3].(*Node)
	if a.Props["svn:mime-type"] != "text" || a.TextLength != 6 {
		t.Fatalf("a.txt: %+v", a)
	}
	change := recs[6].(*Node)
	if !change.PropDelta || !slices.Equal(change.DeletedProps, []string{"svn:mime-type"}) || change.Props == nil {
		t.Fatalf("change: %+v", change)
	}
	cp := recs[7].(*Node)
	if cp.CopyFromPath != "trunk" || cp.CopyFromRev != 1 || cp.Props != nil {
		t.Fatalf("copy: %+v", cp)
	}
	del := recs[8].(*Node)
	if del.Action != ActionDelete || del.Kind != "" {
		t.Fatalf("delete: %+v", del)
	}
}

func TestReaderErrors(t *testing.T) {
	for _, tc := range []struct{ name, dump, want string }{
		{"empty", "", "empty dump"},
		{"no version", "UUID: x\n\n", "not a dump"},
		{"version 1", "SVN-fs-dump-format-version: 1\n\n", "version 1 is not supported"},
		{"bad header", "SVN-fs-dump-format-version: 2\n\nNode-path x\n\n", "invalid header line"},
		{"duplicate header", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-path: y\n\n", "appears twice"},
		{"unknown action", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-kind: file\nNode-action: move\n\n", "unknown action"},
		{"no kind", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-action: add\n\n", "no kind"},
		{"no PROPS-END", "SVN-fs-dump-format-version: 2\n\nRevision-number: 1\nProp-content-length: 4\nContent-length: 4\n\nK 1\n", "invalid key"},
		{"bad value length", "SVN-fs-dump-format-version: 2\n\nRevision-number: 1\nProp-content-length: 23\nContent-length: 23\n\nK 1\na\nV 99\nb\nPROPS-END\n", "invalid value"},
		{"truncated text", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-kind: file\nNode-action: add\nText-content-length: 10\nContent-length: 10\n\nabc", "unexpected EOF"},
		{"truncated headers", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-kind: file", "unexpected EOF"},
		{"short content-length", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-kind: file\nNode-action: add\nText-content-length: 10\nContent-length: 5\n\n", "less than"},
		{"directory text", "SVN-fs-dump-format-version: 2\n\nNode-path: x\nNode-kind: dir\nNode-action: add\nText-content-length: 1\nContent-length: 1\n\nx\n", "directory has a text"},
		{"unknown record", "SVN-fs-dump-format-version: 2\n\nFoo: bar\n\n", "unknown record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(strings.NewReader(tc.dump))
			for err == nil {
				_, err = r.Next()
			}
			if err == io.EOF || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// tree is a model of one revision: file contents by path, and directories
// (with a nil content).
type tree map[string][]byte

func (tr tree) clone() tree {
	out := make(tree, len(tr))
	maps.Copy(out, tr)
	return out
}

func under(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// replay applies a dump to a model of the repository, checking every
// text's checksums and delta base, and returns the model of each revision
// and the nodes read.
func replay(t *testing.T, dump []byte) ([]tree, []*Node, *Reader) {
	t.Helper()
	r, err := NewReader(bytes.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	var trees []tree // trees[i] is revision i, once it is complete
	var nodes []*Node
	cur := tree{}
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch rec := rec.(type) {
		case *Revision:
			if rec.Number > 0 {
				if rec.Number != len(trees)+1 {
					t.Fatalf("revision %d after %d", rec.Number, len(trees)-1)
				}
				trees = append(trees, cur)
				cur = cur.clone()
			}
		case *Node:
			nodes = append(nodes, rec)
			var base []byte
			if rec.Action == ActionDelete || rec.Action == ActionReplace {
				for p := range cur {
					if under(p, rec.Path) {
						delete(cur, p)
					}
				}
			}
			if rec.Action == ActionDelete {
				continue
			}
			if rec.CopyFromRev >= 0 {
				for p, v := range trees[rec.CopyFromRev] {
					if under(p, rec.CopyFromPath) {
						cur[rec.Path+strings.TrimPrefix(p, rec.CopyFromPath)] = v
					}
				}
			}
			if rec.Kind == KindDir {
				cur[rec.Path] = nil
				continue
			}
			if rec.Action == ActionChange || rec.CopyFromRev >= 0 {
				base = cur[rec.Path]
			}
			if !rec.HasText {
				if _, ok := cur[rec.Path]; !ok {
					cur[rec.Path] = []byte{} // an empty file
				}
				continue
			}
			var text bytes.Buffer
			c := NewChecker(rec.TextContentMD5, rec.TextContentSHA1)
			w := io.MultiWriter(&text, c)
			if rec.TextDelta {
				if rec.TextDeltaBaseMD5 != "" {
					if sum := md5.Sum(base); hex.EncodeToString(sum[:]) != rec.TextDeltaBaseMD5 {
						t.Fatalf("%s: the delta base is not the model's previous text", rec.Path)
					}
				}
				if _, err := Apply(w, rec.Text, bytes.NewReader(base)); err != nil {
					t.Fatalf("%s: %v", rec.Path, err)
				}
			} else if _, err := io.Copy(w, rec.Text); err != nil {
				t.Fatal(err)
			}
			if err := c.Check(); err != nil {
				t.Fatalf("%s: %v", rec.Path, err)
			}
			cur[rec.Path] = text.Bytes()
		}
	}
	trees = append(trees, cur)
	return trees, nodes, r
}

func TestSvnDumps(t *testing.T) {
	r, youngest := svntest.Standard(t)
	repo := r.Dir
	dir := filepath.Dir(repo)
	uuid := strings.TrimSpace(string(svntest.Run(t, dir, "svnlook", "uuid", repo)))
	for _, tc := range []struct {
		name    string
		version int
		dump    func() []byte
	}{
		{"svnadmin", 2, func() []byte { return svntest.Run(t, dir, "svnadmin", "dump", "-q", repo) }},
		{"svnadmin --deltas", 3, func() []byte { return svntest.Run(t, dir, "svnadmin", "dump", "-q", "--deltas", repo) }},
		{"svnrdump", 3, func() []byte { return svntest.Run(t, dir, "svnrdump", "dump", "-q", "file://"+repo) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "svnrdump" {
				svntest.Require(t, "svnrdump")
			}
			trees, nodes, r := replay(t, tc.dump())
			if r.Version != tc.version || r.UUID != uuid {
				t.Fatalf("version %d, UUID %q", r.Version, r.UUID)
			}
			if len(trees) != youngest+1 {
				t.Fatalf("%d revisions", len(trees)-1)
			}
			for rev := 1; rev <= youngest; rev++ {
				checkRevision(t, repo, rev, trees[rev])
			}
			checkProps(t, nodes, tc.version)
		})
	}
}

// checkRevision compares the model with svnlook's view of a revision.
func checkRevision(t *testing.T, repo string, rev int, model tree) {
	t.Helper()
	dir := filepath.Dir(repo)
	r := strconv.Itoa(rev)
	var paths []string
	for line := range strings.SplitSeq(string(svntest.Run(t, dir, "svnlook", "tree", "--full-paths", "-r", r, repo)), "\n") {
		if line != "" && line != "/" {
			paths = append(paths, strings.TrimSuffix(line, "/"))
		}
	}
	var want []string
	for p := range model {
		want = append(want, p)
	}
	slices.Sort(paths)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("r%d: paths %v, svnlook has %v", rev, want, paths)
	}
	for p, content := range model {
		if content == nil {
			continue
		}
		if got := svntest.Run(t, dir, "svnlook", "cat", "-r", r, repo, p); !bytes.Equal(got, content) {
			t.Fatalf("r%d %s: %d bytes, svnlook has %d", rev, p, len(content), len(got))
		}
	}
}

func checkProps(t *testing.T, nodes []*Node, version int) {
	t.Helper()
	find := func(path, action string, nth int) *Node {
		for _, n := range nodes {
			if n.Path == path && n.Action == action {
				if nth == 0 {
					return n
				}
				nth--
			}
		}
		t.Fatalf("no node %s %s", action, path)
		return nil
	}
	if n := find("trunk/run.sh", ActionAdd, 0); n.Props["svn:executable"] != "*" {
		t.Fatalf("run.sh props %v", n.Props)
	}
	if n := find("trunk/link", ActionAdd, 0); n.Props["svn:special"] != "*" {
		t.Fatalf("link props %v", n.Props)
	}
	if n := find("trunk/board.brd", ActionAdd, 0); n.Props["svn:needs-lock"] != "*" || n.Props["svn:mime-type"] != "application/octet-stream" {
		t.Fatalf("board.brd props %v", n.Props)
	}
	n := find("trunk/board.brd", ActionChange, 0)
	if version == 3 {
		if !n.PropDelta || !slices.Equal(n.DeletedProps, []string{"svn:needs-lock"}) {
			t.Fatalf("board.brd prop delta %+v", n)
		}
	} else if _, ok := n.Props["svn:needs-lock"]; ok || n.Props["svn:mime-type"] == "" {
		t.Fatalf("board.brd props after propdel %v", n.Props)
	}
	if n := find("branches/x", ActionAdd, 0); n.CopyFromPath != "trunk" || n.CopyFromRev != 2 {
		t.Fatalf("branch copy %+v", n)
	}
	if n := find("trunk/a.txt", ActionReplace, 0); n.CopyFromRev != -1 {
		t.Fatalf("replace %+v", n)
	}
}

func FuzzApply(f *testing.F) {
	f.Add(delta(0, window(0, false, 0, 5, 7, slices.Concat(op(0, 5, 0), op(1, 2, 0)), nil)), []byte("hello"))
	f.Add(delta(1, window(1, true, 0, 0, 10, op(2, 10, 0), []byte("0123456789"))), []byte(nil))
	f.Fuzz(func(t *testing.T, d, base []byte) {
		Apply(io.Discard, bytes.NewReader(d), bytes.NewReader(base))
	})
}

func FuzzDump(f *testing.F) {
	f.Add([]byte(handDump))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := NewReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		for {
			rec, err := r.Next()
			if err != nil {
				return
			}
			if n, ok := rec.(*Node); ok && n.HasText && len(data)%2 == 0 {
				io.Copy(io.Discard, n.Text) // read some texts, skip others
			}
		}
	})
}
