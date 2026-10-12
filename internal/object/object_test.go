package object

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/testutil"
)

func h(s string) hash.Hash { return hash.Sum(byte(TypeChunk), []byte(s)) }

func hp(s string) *hash.Hash { x := h(s); return &x }

// sampleObjects covers every object type and every optional field.
func sampleObjects() map[string]Object {
	raw := GitRaw("100644 a\x00" + string(make([]byte, 20)))
	return map[string]Object{
		"file-empty":    &File{},
		"file-direct":   &File{Size: 300, Refs: []Ref{{Hash: h("a"), Size: 100}, {Hash: h("b"), Size: 200}}},
		"file-indexed":  &File{Size: 9000, Level: 1, Refs: []Ref{{Hash: h("x1"), Size: 4000}, {Hash: h("x2"), Size: 5000}}},
		"fileindex":     &FileIndex{Refs: []Ref{{Hash: h("c"), Size: 65536}}},
		"tree-empty":    &TreeNode{},
		"tree-leaf":     &TreeNode{Entries: sampleEntries()},
		"tree-interior": &TreeNode{Level: 1, Children: []Child{{FirstName: []byte("a"), Node: h("n1"), Count: 20}, {FirstName: []byte("m"), Node: h("n2"), Count: 30}}},
		"commit-root":   &Commit{Tree: h("t"), Author: alice(), Committer: alice(), Message: []byte("initial\n")},
		"commit-full":   &Commit{Tree: h("t"), Parents: []hash.Hash{h("p1"), h("p2")}, Author: alice(), Committer: bob(), Message: []byte("merge\n"), GitExtra: []ExtraHeader{{Key: []byte("encoding"), Value: []byte("ISO-8859-1")}, {Key: []byte("gpgsig"), Value: []byte("-----BEGIN PGP SIGNATURE-----\n...")}}, GitRawHint: []byte("tree ...\n"), GitTreeRaw: hp("raw")},
		"tag-no-tagger": &Tag{Object: h("c"), TargetType: TagTargetCommit, Name: []byte("v1.0"), Message: []byte("release\n")},
		"tag-full":      &Tag{Object: h("c"), TargetType: TagTargetCommit, Name: []byte("v2.0"), Tagger: ptr(bob()), Message: []byte("signed\n"), GitExtra: []ExtraHeader{{Key: []byte("gpgsig"), Value: []byte("sig")}}, GitRawHint: []byte("object ...")},
		"digestrecord":  &DigestRecord{File: h("f"), SHA1Git: [20]byte{1, 2, 3}, SHA256: [32]byte{4, 5, 6}, MD5: [16]byte{7, 8, 9}},
		"gitraw":        &raw,
	}
}

func ptr[T any](v T) *T { return &v }

func alice() Person {
	return Person{Name: []byte("Alice"), Email: []byte("alice@example.com"), Time: 1700000000, TZ: "+0200"}
}

func bob() Person {
	return Person{Name: []byte("B\xf6b"), Email: []byte("bob@example.com"), Time: 1700000100, TZ: "-0030"}
}

func sampleEntries() []Entry {
	return []Entry{
		{Name: []byte("README"), Kind: KindFile, Target: h("readme"), Attrs: &Attrs{ContentType: "text/plain", ETag: `"abc"`, MTime: 1700000000123456789, Meta: map[string]string{"owner": "x", "a": "b"}, Checksums: map[string][]byte{"crc32": {1, 2, 3, 4}}}},
		{Name: []byte("big.psd"), Kind: KindLFS, Target: h("psd"), Attrs: &Attrs{Exec: true, LFSPointer: []byte("version https://git-lfs.github.com/spec/v1\n")}},
		{Name: []byte("bin"), Kind: KindDir, Target: h("bin"), Attrs: &Attrs{GitRaw: hp("rawtree")}},
		{Name: []byte("link"), Kind: KindSymlink, Target: h("link")},
		{Name: []byte("run.sh"), Kind: KindExec, Target: h("run")},
		{Name: []byte("sub"), Kind: KindGitlink, Attrs: &Attrs{GitSHA1: bytes.Repeat([]byte{0xab}, 20)}},
	}
}

type goldenObject struct {
	Type     string    `json:"type"`
	Encoding string    `json:"encoding"`
	Hash     hash.Hash `json:"hash"`
}

func TestGoldenObjects(t *testing.T) {
	got := map[string]goldenObject{}
	for name, o := range sampleObjects() {
		hh, b, err := Hash(o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[name] = goldenObject{o.Type().String(), hex.EncodeToString(b), hh}
	}
	testutil.Golden(t, "objects", got)
}

func TestRoundTrip(t *testing.T) {
	for name, o := range sampleObjects() {
		b, err := Encode(o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := Decode(o.Type(), b)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		b2, err := Encode(d)
		if err != nil {
			t.Fatalf("%s: re-encode: %v", name, err)
		}
		if !bytes.Equal(b, b2) {
			t.Errorf("%s: encode → decode → encode differs", name)
		}
	}
}

func TestDecodeAs(t *testing.T) {
	f := &File{Size: 1, Refs: []Ref{{Hash: h("a"), Size: 1}}}
	b, err := Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAs[*File](b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Refs, f.Refs) || got.Size != 1 {
		t.Fatalf("got %+v", got)
	}
	if _, err := DecodeAs[*Commit](b); err == nil {
		t.Fatal("decoding a File as Commit succeeded")
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestDecodeRejects(t *testing.T) {
	ha := h("a")
	hh := hex.EncodeToString(ha[:])
	entry := "a3" + "004161" + "0101" + "025820" + hh // {0: h'61', 1: 1, 2: hash}
	// Positive control: the building blocks decode when used canonically.
	if _, err := Decode(TypeTree, mustHex("a2"+"0000"+"0181"+entry)); err != nil {
		t.Fatalf("control leaf: %v", err)
	}
	if _, err := Decode(TypeFileIndex, mustHex("8200"+"81"+"825820"+hh+"01")); err != nil {
		t.Fatalf("control fileindex: %v", err)
	}
	if _, err := Decode(TypeTag, mustHex("a4"+"005820"+hh+"0166636f6d6d6974"+"024176"+"0440")); err != nil {
		t.Fatalf("control tag: %v", err)
	}
	tests := []struct {
		name string
		typ  Type
		data string
	}{
		{"non-minimal uint", TypeFile, "83" + "1800" + "00" + "80"},                          // size 0 encoded as 1800
		{"indefinite array", TypeFile, "9f000080ff"},                                         // [_ 0, 0, []]
		{"trailing bytes", TypeFile, "8300008000"},                                           // valid file plus one byte
		{"wrong array length", TypeFile, "820000"},                                           // [0, 0]
		{"hash too short", TypeFileIndex, "8200" + "81" + "82" + "41aa" + "01"},              // ref hash of 1 byte
		{"non-minimal ref size", TypeFileIndex, "8200" + "81" + "82" + "5820" + hh + "1801"}, // size 1 as 1801
		{"unsorted map keys", TypeTree, "a2" + "0181" + entry + "0000"},
		{"empty attrs", TypeTree, "a2" + "0000" + "0181" + "a4" + "004161" + "0101" + "025820" + hh + "03a0"},
		{"unknown map key", TypeTree, "a2" + "0000" + "09" + "00"},
		{"unknown attr", TypeTree, "a2" + "0000" + "0181" + "a4" + "004161" + "0101" + "025820" + hh + "03a1" + "63787878" + "01"},
		{"tag", TypeFile, "c0" + "83000080"},
		{"invalid utf8", TypeTag, "a4" + "005820" + hh + "0162ffff" + "024176" + "0440"},
		{"empty file with refs size mismatch", TypeFile, "83050081825820" + hh + "04"},
		{"gitraw empty", TypeGitRaw, "40"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.typ, mustHex(tt.data)); err == nil {
				t.Fatalf("decode of %s succeeded", tt.data)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	big := make([]Ref, MaxRefs+1)
	for i := range big {
		big[i] = Ref{Hash: h(fmt.Sprint(i)), Size: 1}
	}
	tests := map[string]Object{
		"file size mismatch":      &File{Size: 5, Refs: []Ref{{Hash: h("a"), Size: 4}}},
		"file too many refs":      &File{Size: MaxRefs + 1, Refs: big},
		"file zero-size ref":      &File{Size: 1, Refs: []Ref{{Hash: h("a"), Size: 0}, {Hash: h("b"), Size: 1}}},
		"file chunk too big":      &File{Size: 1 << 20, Refs: []Ref{{Hash: h("a"), Size: 1 << 20}}},
		"file indexed single ref": &File{Size: 5, Level: 1, Refs: []Ref{{Hash: h("a"), Size: 5}}},
		"fileindex empty":         &FileIndex{},
		"tree unsorted":           &TreeNode{Entries: []Entry{{Name: []byte("b"), Kind: KindFile, Target: h("1")}, {Name: []byte("a"), Kind: KindFile, Target: h("2")}}},
		"tree duplicate":          &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindFile, Target: h("1")}, {Name: []byte("a"), Kind: KindFile, Target: h("2")}}},
		"tree empty name":         &TreeNode{Entries: []Entry{{Kind: KindFile, Target: h("1")}}},
		"tree bad kind":           &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: 9, Target: h("1")}}},
		"tree empty attrs":        &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindFile, Target: h("1"), Attrs: &Attrs{}}}},
		"tree gitlink no sha":     &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindGitlink}}},
		"tree lfs no pointer":     &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindLFS, Target: h("1")}}},
		"tree exec on file":       &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindFile, Target: h("1"), Attrs: &Attrs{Exec: true}}}},
		"tree git_raw on file":    &TreeNode{Entries: []Entry{{Name: []byte("a"), Kind: KindFile, Target: h("1"), Attrs: &Attrs{GitRaw: hp("r")}}}},
		"tree mixed":              &TreeNode{Level: 1, Entries: sampleEntries()},
		"tree interior empty":     &TreeNode{Level: 1},
		"commit zero tree":        &Commit{},
		"commit dup parents":      &Commit{Tree: h("t"), Parents: []hash.Hash{h("p"), h("p")}},
		"tag bad type":            &Tag{Object: h("o"), TargetType: "blob2", Name: []byte("v")},
		"digest zero file":        &DigestRecord{},
	}
	for name, o := range tests {
		if _, err := Encode(o); err == nil {
			t.Errorf("%s: Encode succeeded", name)
		}
	}
}

// batchFile is the reference definition of the File layout from the
// FileBuilder documentation.
func batchFile(t *testing.T, chunks []Ref) (*File, []*FileIndex) {
	t.Helper()
	var indexes []*FileIndex
	refs := chunks
	var level uint64
	var size uint64
	for _, r := range chunks {
		size += r.Size
	}
	for len(refs) > MaxRefs {
		var next, group []Ref
		var groupLen uint64
		for _, r := range refs {
			group = append(group, r)
			groupLen += r.Size
			if (len(group) >= MinGroup && groupBoundary(r)) || len(group) == MaxRefs {
				x := &FileIndex{Level: level, Refs: group}
				hh, _, err := Hash(x)
				if err != nil {
					t.Fatal(err)
				}
				indexes = append(indexes, x)
				next = append(next, Ref{Hash: hh, Size: groupLen})
				group, groupLen = nil, 0
			}
		}
		if len(group) > 0 {
			x := &FileIndex{Level: level, Refs: group}
			hh, _, err := Hash(x)
			if err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, x)
			next = append(next, Ref{Hash: hh, Size: groupLen})
		}
		refs = next
		level++
	}
	return &File{Size: size, Level: level, Refs: refs}, indexes
}

func syntheticRefs(n int) []Ref {
	refs := make([]Ref, n)
	for i := range refs {
		refs[i] = Ref{Hash: h(fmt.Sprintf("chunk %d", i)), Size: uint64(16<<10 + i%1000)}
	}
	return refs
}

func buildFile(t *testing.T, refs []Ref) (*File, []*FileIndex) {
	t.Helper()
	var emitted []*FileIndex
	b := NewFileBuilder(func(x *FileIndex) (hash.Hash, error) {
		emitted = append(emitted, x)
		hh, _, err := Hash(x)
		return hh, err
	})
	for _, r := range refs {
		if err := b.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	f, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return f, emitted
}

func TestFileBuilderMatchesBatch(t *testing.T) {
	for _, n := range []int{0, 1, 2, MaxRefs - 1, MaxRefs, MaxRefs + 1, 3 * MaxRefs, 20000} {
		refs := syntheticRefs(n)
		got, gotIdx := buildFile(t, refs)
		want, wantIdx := batchFile(t, refs)
		gh, _, err := Hash(got)
		if err != nil {
			t.Fatal(err)
		}
		wh, _, err := Hash(want)
		if err != nil {
			t.Fatal(err)
		}
		if gh != wh {
			t.Errorf("n=%d: builder File %s != batch File %s", n, gh.Short(), wh.Short())
		}
		if len(gotIdx) != len(wantIdx) {
			t.Errorf("n=%d: %d FileIndex objects, want %d", n, len(gotIdx), len(wantIdx))
		}
		if n > MaxRefs && got.Level == 0 {
			t.Errorf("n=%d: expected an indexed file", n)
		}
		if n <= MaxRefs && got.Level != 0 {
			t.Errorf("n=%d: expected a direct file", n)
		}
	}
}

// An edit in the middle of a large file changes only the FileIndex objects
// around it, because groups are content-defined.
func TestFileIndexLocality(t *testing.T) {
	refs := syntheticRefs(20000)
	_, before := buildFile(t, refs)
	edited := append([]Ref{}, refs...)
	edited = append(edited[:10000], append([]Ref{{Hash: h("inserted"), Size: 1000}}, edited[10000:]...)...)
	_, after := buildFile(t, edited)
	old := map[hash.Hash]bool{}
	for _, x := range before {
		hh, _, _ := Hash(x)
		old[hh] = true
	}
	changed := 0
	for _, x := range after {
		hh, _, _ := Hash(x)
		if !old[hh] {
			changed++
		}
	}
	if changed > 2 {
		t.Errorf("%d FileIndex objects changed, want at most 2", changed)
	}
}

func TestGoldenFileLayout(t *testing.T) {
	got := map[string]hash.Hash{}
	for _, n := range []int{1, MaxRefs, MaxRefs + 1, 20000} {
		f, _ := buildFile(t, syntheticRefs(n))
		hh, _, err := Hash(f)
		if err != nil {
			t.Fatal(err)
		}
		got[fmt.Sprint(n)] = hh
	}
	testutil.Golden(t, "file-layout", got)
}

func syntheticEntries(n int) []Entry {
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{Name: fmt.Appendf(nil, "file-%08d.dat", i), Kind: KindFile, Target: h(fmt.Sprint(i))}
	}
	return entries
}

type memNodes map[hash.Hash]*TreeNode

func (m memNodes) put(n *TreeNode) (hash.Hash, error) {
	hh, _, err := Hash(n)
	if err != nil {
		return hash.Zero, err
	}
	m[hh] = n
	return hh, nil
}

func TestGoldenTree(t *testing.T) {
	type boundary struct {
		Name     string `json:"name"`
		Leaf     bool   `json:"leaf"`
		Interior bool   `json:"interior_level1"`
	}
	var boundaries []boundary
	for i := range 200 {
		name := fmt.Sprintf("file-%08d.dat", i)
		boundaries = append(boundaries, boundary{name, LeafBoundary([]byte(name)), InteriorBoundary(1, []byte(name))})
	}
	roots := map[string]hash.Hash{}
	for _, n := range []int{0, 1, 15, 100, 5000, 100000} {
		nodes := memNodes{}
		root, err := BuildTree(syntheticEntries(n), nodes.put)
		if err != nil {
			t.Fatal(err)
		}
		roots[fmt.Sprint(n)] = root
	}
	testutil.Golden(t, "tree", map[string]any{"boundaries": boundaries, "roots": roots})
}

func TestBuildTreeShape(t *testing.T) {
	nodes := memNodes{}
	entries := syntheticEntries(100000)
	root, err := BuildTree(entries, nodes.put)
	if err != nil {
		t.Fatal(err)
	}
	// Walk the tree: every leaf entry appears once, in order, and interior
	// counts are correct.
	var walk func(hash.Hash) uint64
	var seen []Entry
	var leaves, natural int
	walk = func(hh hash.Hash) uint64 {
		n := nodes[hh]
		if n.Level == 0 {
			leaves++
			if len(n.Entries) < NodeMaxEntries {
				natural++
			}
			seen = append(seen, n.Entries...)
			return uint64(len(n.Entries))
		}
		var total uint64
		for _, c := range n.Children {
			got := walk(c.Node)
			if got != c.Count {
				t.Fatalf("child count %d, subtree has %d", c.Count, got)
			}
			total += got
		}
		return total
	}
	if total := walk(root); total != uint64(len(entries)) {
		t.Fatalf("tree holds %d entries, want %d", total, len(entries))
	}
	if !reflect.DeepEqual(seen, entries) {
		t.Fatal("entries differ after walking the tree")
	}
	avg := len(entries) / leaves
	t.Logf("%d leaves, average %d entries, %d nodes total", leaves, avg, len(nodes))
	if avg < 40 || avg > 120 {
		t.Errorf("average leaf size %d is far from ~80", avg)
	}
	if natural != leaves && float64(leaves-natural)/float64(leaves) > 0.01 {
		t.Errorf("%d of %d leaves were force-split", leaves-natural, leaves)
	}
}

func TestTreeSizeCap(t *testing.T) {
	entries := syntheticEntries(100)
	for i := range entries {
		entries[i].Attrs = &Attrs{Meta: map[string]string{"blob": string(bytes.Repeat([]byte{'x'}, 10000))}}
	}
	nodes := memNodes{}
	if _, err := BuildTree(entries, nodes.put); err != nil {
		t.Fatal(err)
	}
	leaves := 0
	for _, n := range nodes {
		if n.Level != 0 {
			continue
		}
		leaves++
		// The node ends as soon as its size reaches the cap, so everything
		// before its last entry is below the cap.
		size := 0
		for i := range n.Entries[:len(n.Entries)-1] {
			b, _ := encMode.Marshal(&n.Entries[i])
			size += len(b)
		}
		if size >= NodeMaxBytes {
			t.Errorf("leaf with %d bytes before its last entry exceeds the cap", size)
		}
	}
	if leaves < 100*10000/NodeMaxBytes {
		t.Errorf("only %d leaves; the size cap did not split", leaves)
	}
}

func TestDecodeUnknownType(t *testing.T) {
	if _, err := Decode(0x7f, []byte{0x80}); err == nil {
		t.Fatal("unknown type accepted")
	}
	if _, err := Decode(TypeChunk, []byte{0x80}); err == nil {
		t.Fatal("chunk accepted as CBOR")
	}
}

func FuzzDecode(f *testing.F) {
	for _, o := range sampleObjects() {
		b, err := Encode(o)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(byte(o.Type()), b)
	}
	f.Fuzz(func(t *testing.T, typ byte, data []byte) {
		o, err := Decode(Type(typ), data)
		if err != nil {
			if o != nil {
				t.Fatal("object returned with error")
			}
			return
		}
		b, err := Encode(o)
		if err != nil {
			t.Fatalf("decoded object does not encode: %v", err)
		}
		if !bytes.Equal(b, data) {
			t.Fatal("accepted a non-canonical encoding")
		}
	})
}
