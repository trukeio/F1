package git

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/object"
)

func TestIdentRoundTrip(t *testing.T) {
	for _, s := range []string{
		"A U Thor <a@example.com> 1700000000 +0200",
		" <> 0 +0000",
		"Name With <Angle> <x@y> 12 -0030",
		"Zé \xff <bad\xfe@x> 99 +1345",
	} {
		p, err := ParseIdent([]byte(s))
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := string(FormatIdent(&p)); got != s {
			t.Errorf("%q round-trips to %q", s, got)
		}
	}
	for _, bad := range []string{"no email 1 +0000", "a <b> notime +0000", "a <b>"} {
		if _, err := ParseIdent([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCommitRoundTrip(t *testing.T) {
	raw := "tree " + strings.Repeat("1", 40) + "\nparent " + strings.Repeat("2", 40) + "\nparent " + strings.Repeat("3", 40) +
		"\nauthor A <a@x> 1 +0100\ncommitter C <c@x> 2 -0030\nencoding ISO-8859-1\ngpgsig -----BEGIN-----\n \n line\n -----END-----\n\nmsg\xe9\n"
	pc, err := ParseCommit([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(pc.Parents) != 2 || len(pc.Commit.GitExtra) != 2 || string(pc.Commit.GitExtra[1].Value) != "-----BEGIN-----\n\nline\n-----END-----" {
		t.Fatalf("parsed %+v", pc)
	}
	if got := string(EncodeCommit(&pc.Commit, pc.Tree, pc.Parents)); got != raw {
		t.Fatalf("re-encoded:\n%q\nwant\n%q", got, raw)
	}
}

func TestTreeOrder(t *testing.T) {
	var s SHA
	entries := []TreeEntry{{ModeFile, []byte("a.txt"), s}, {ModeDir, []byte("a"), s}, {ModeFile, []byte("a-b"), s}, {ModeFile, []byte("b"), s}}
	SortTree(entries)
	var names []string
	for _, e := range entries {
		names = append(names, string(e.Name))
	}
	// Directories compare as "a/": "a-b" (0x2D) < "a.txt" (0x2E) < "a/" (0x2F).
	if strings.Join(names, " ") != "a-b a.txt a b" {
		t.Fatalf("order %v", names)
	}
	parsed, err := ParseTree(EncodeTree(entries))
	if err != nil || len(parsed) != 4 {
		t.Fatalf("parse: %v %v", parsed, err)
	}
	for _, tt := range []struct {
		mode  string
		kind  object.Kind
		canon bool
	}{
		{"100644", object.KindFile, true}, {"100664", object.KindFile, false}, {"100755", object.KindExec, true},
		{"40000", object.KindDir, true}, {"040000", object.KindDir, false}, {"120000", object.KindSymlink, true},
		{"160000", object.KindGitlink, true},
	} {
		k, canon, err := KindForMode(tt.mode)
		if err != nil || k != tt.kind || canon != tt.canon {
			t.Errorf("KindForMode(%s) = %v %v %v", tt.mode, k, canon, err)
		}
	}
}

func TestObjectHeader(t *testing.T) {
	for _, tt := range []struct {
		size int64
		want []byte
	}{
		{0, []byte{0x30}}, {15, []byte{0x3f}}, {16, []byte{0xb0, 0x01}}, {1 << 20, []byte{0xb0, 0x80, 0x80, 0x04}},
	} {
		if got := objectHeader(TypeBlob, tt.size); !bytes.Equal(got, tt.want) {
			t.Errorf("objectHeader(blob, %d) = %x, want %x", tt.size, got, tt.want)
		}
	}
}

// FuzzObjects checks that the Git object parsers never panic, and that
// what they accept re-encodes to the same bytes when canonical.
func FuzzObjects(f *testing.F) {
	f.Add([]byte("tree " + strings.Repeat("a", 40) + "\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\n\nm\n"))
	f.Add([]byte("object " + strings.Repeat("b", 40) + "\ntype commit\ntag v\ntagger T <t> 2 +0100\n\nmsg"))
	f.Add(append([]byte("100644 f\x00"), make([]byte, 20)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if es, err := ParseTree(data); err == nil {
			if got := EncodeTree(es); !bytes.Equal(got, data) {
				t.Fatalf("tree re-encodes differently")
			}
		}
		if pc, err := ParseCommit(data); err == nil {
			_ = EncodeCommit(&pc.Commit, pc.Tree, pc.Parents)
		}
		if pt, err := ParseTag(data); err == nil {
			_ = EncodeTag(&pt.Tag, pt.Object)
		}
	})
}

// FuzzPktLine checks that the pkt-line reader never panics or loops.
func FuzzPktLine(f *testing.F) {
	f.Add([]byte("0009want\n0000"))
	f.Add([]byte("00010002ffff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		pr := &pktReader{r: bufio.NewReader(bytes.NewReader(data))}
		for i := 0; i < len(data)+2; i++ {
			kind, _, err := pr.next()
			if err != nil || kind == pktEOF {
				return
			}
		}
		t.Fatal("reader did not finish")
	})
}
