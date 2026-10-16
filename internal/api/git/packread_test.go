package git

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// memSink collects resolved objects; bases outside the pack come from a
// Git repository.
type memSink struct {
	t       *testing.T
	objects map[SHA][]byte
	types   map[SHA]Type
	repo    string // for thin pack bases
}

func (m *memSink) blob(_ context.Context, size int64, r io.Reader) (SHA, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return SHA{}, err
	}
	h := newObjectHasher(TypeBlob, size)
	h.Write(data)
	s, err := h.sum()
	m.objects[s], m.types[s] = data, TypeBlob
	return s, err
}

func (m *memSink) object(t Type, s SHA, data []byte) { m.objects[s], m.types[s] = data, t }

func (m *memSink) base(_ context.Context, s SHA) (Type, int64, io.ReaderAt, error) {
	if d, ok := m.objects[s]; ok {
		return m.types[s], int64(len(d)), bytes.NewReader(d), nil
	}
	if m.repo == "" {
		return 0, 0, nil, nil
	}
	typ, err := exec.Command("git", "-C", m.repo, "cat-file", "-t", s.String()).Output()
	if err != nil {
		return 0, 0, nil, nil
	}
	data, err := exec.Command("git", "-C", m.repo, "cat-file", strings.TrimSpace(string(typ)), s.String()).Output()
	if err != nil {
		return 0, 0, nil, err
	}
	t, _ := TypeByName(strings.TrimSpace(string(typ)))
	return t, int64(len(data)), bytes.NewReader(data), nil
}

func git(t *testing.T, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// deltaRepo builds a repository whose objects delta well: a file edited in
// several commits.
func deltaRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")
	var content []byte
	for i := range 200 {
		content = append(content, []byte(strings.Repeat("line of text number ", 3)+string(rune('a'+i%26))+"\n")...)
	}
	for i := range 5 {
		content = append(content, []byte("edit "+string(rune('0'+i))+"\n")...)
		os.WriteFile(filepath.Join(dir, "file.txt"), content, 0o644)
		os.MkdirAll(filepath.Join(dir, "d"), 0o755)
		os.WriteFile(filepath.Join(dir, "d", "copy.txt"), append([]byte("copy\n"), content...), 0o644)
		git(t, dir, nil, "add", "-A")
		git(t, dir, nil, "commit", "-q", "-m", "c"+string(rune('0'+i)))
	}
	return dir
}

func readAll(t *testing.T, pack []byte, sink *memSink) (int, error) {
	t.Helper()
	spool, n, err := readPack(context.Background(), bytes.NewReader(pack), t.TempDir(), sink)
	if err == nil {
		spool.Close()
	}
	return n, err
}

// A full pack with ofs-deltas and a thin pack with ref-deltas against
// objects outside the pack resolve to the repository's objects.
func TestReadPackDeltas(t *testing.T) {
	dir := deltaRepo(t)
	all := strings.Fields(string(git(t, dir, nil, "rev-list", "--objects", "--all")))
	var ids []string
	for _, f := range all {
		if len(f) == 40 {
			ids = append(ids, f)
		}
	}
	pack := git(t, dir, []byte(strings.Join(ids, "\n")+"\n"), "pack-objects", "--stdout", "--delta-base-offset", "--window=50")
	sink := &memSink{t: t, objects: map[SHA][]byte{}, types: map[SHA]Type{}}
	n, err := readAll(t, pack, sink)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ids) || len(sink.objects) != len(ids) {
		t.Fatalf("%d entries, %d objects, want %d", n, len(sink.objects), len(ids))
	}
	for _, id := range ids {
		s, _ := ParseSHA(id)
		want := git(t, dir, nil, "cat-file", sink.types[s].String(), id)
		if !bytes.Equal(sink.objects[s], want) {
			t.Fatalf("object %s differs", id)
		}
	}
	// Thin pack: the last commit's objects, with deltas against the
	// previous commit's objects, which are not in the pack.
	thin := git(t, dir, []byte("main\n^main~1\n"), "pack-objects", "--stdout", "--thin", "--revs", "--window=50")
	sink = &memSink{t: t, objects: map[SHA][]byte{}, types: map[SHA]Type{}, repo: dir}
	if _, err := readAll(t, thin, sink); err != nil {
		t.Fatal(err)
	}
	head := git(t, dir, nil, "rev-parse", "main:file.txt")
	s, _ := ParseSHA(strings.TrimSpace(string(head)))
	if !bytes.Equal(sink.objects[s], git(t, dir, nil, "cat-file", "blob", s.String())) {
		t.Fatal("thin pack blob differs")
	}
	// Without access to the bases, the thin pack cannot resolve.
	sink = &memSink{t: t, objects: map[SHA][]byte{}, types: map[SHA]Type{}}
	if _, err := readAll(t, thin, sink); err == nil || !strings.Contains(err.Error(), "neither in the pack nor in the repository") {
		t.Fatalf("thin pack without bases: %v", err)
	}
}

func TestReadPackRejects(t *testing.T) {
	dir := deltaRepo(t)
	pack := git(t, dir, []byte("main\n"), "pack-objects", "--stdout", "--revs")
	sink := func() *memSink { return &memSink{t: t, objects: map[SHA][]byte{}, types: map[SHA]Type{}} }
	bad := bytes.Clone(pack)
	bad[len(bad)/2] ^= 0xff
	if _, err := readAll(t, bad, sink()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt pack: %v", err)
	}
	if _, err := readAll(t, pack[:10], sink()); err == nil {
		t.Fatal("truncated pack accepted")
	}
}

// The SHA-1 collision attack of SHAttered is detected. Its two colliding
// blocks are aligned to the start of shattered-1.pdf, so a Git object (with
// its "blob <n>" header) cannot carry the collision; the test checks the
// detector every pack object goes through on the raw blocks.
func TestCollisionDetected(t *testing.T) {
	prefix, err := os.ReadFile("testdata/shattered-1-prefix.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collisionSum(bytes.NewReader(prefix)); !errors.Is(err, ErrCollision) {
		t.Fatalf("SHAttered blocks: %v", err)
	}
	if _, err := collisionSum(bytes.NewReader(prefix[:192])); err != nil {
		t.Fatalf("prefix without the colliding blocks: %v", err)
	}
	h := newObjectHasher(TypeBlob, 3)
	h.Write([]byte("abc"))
	if s, err := h.sum(); err != nil || s != Hash(TypeBlob, []byte("abc")) {
		t.Fatalf("object id %s, %v", s, err)
	}
}

// FuzzReadPack checks that malformed packs never panic.
func FuzzReadPack(f *testing.F) {
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00"))
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01\x30\x78\x9c\x03\x00\x00\x00\x00\x01"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Append the trailer so that parsing gets past the checksum.
		sum := sha1.Sum(data)
		h := sum[:]
		sink := &memSink{t: t, objects: map[SHA][]byte{}, types: map[SHA]Type{}}
		if spool, _, err := readPack(context.Background(), bytes.NewReader(append(bytes.Clone(data), h...)), t.TempDir(), sink); err == nil {
			spool.Close()
		}
	})
}

// A pack of many commits and trees is held in the object spool, not in
// memory: a push of a long history has hundreds of thousands of trees.
func TestReadPackMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an 80 MiB pack")
	}
	const n, size = 20000, 4096
	var buf bytes.Buffer
	buf.WriteString("PACK\x00\x00\x00\x02")
	binary.Write(&buf, binary.BigEndian, uint32(n))
	data := make([]byte, size)
	for i := range n {
		for j := range data {
			data[j] = byte(i*31 + j*7 + j/3)
		}
		binary.BigEndian.PutUint32(data, uint32(i))
		// Entry header: type tree, size in 4 + 7-bit groups.
		c := byte(TypeTree)<<4 | byte(size&0x0f)
		rest := size >> 4
		for rest > 0 {
			buf.WriteByte(c | 0x80)
			c, rest = byte(rest&0x7f), rest>>7
		}
		buf.WriteByte(c)
		zw := zlib.NewWriter(&buf)
		zw.Write(data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	pack := buf.Bytes()

	sink := &nullSink{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	spool, got, err := readPack(context.Background(), bytes.NewReader(pack), t.TempDir(), sink)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(pack) // live in both measurements
	if got != n || len(spool.index) != n || sink.objects != n {
		t.Fatalf("%d entries, %d spooled, %d seen", got, len(spool.index), sink.objects)
	}
	t.Logf("heap grew by %.1f MiB for %d MiB of trees", float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), n*size>>20)
	// The content is 80 MiB; what stays in memory is the index.
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 16<<20 {
		t.Fatalf("heap grew by %d MiB", grown>>20)
	}
	var any SHA
	for s := range spool.index {
		any = s
		break
	}
	typ, back, err := spool.get(any)
	if err != nil || typ != TypeTree || len(back) != size {
		t.Fatalf("spooled object: %v, %d bytes, %v", typ, len(back), err)
	}
}

// nullSink counts objects and keeps none.
type nullSink struct{ objects int }

func (s *nullSink) blob(_ context.Context, size int64, r io.Reader) (SHA, error) {
	return SHA{}, errors.New("unexpected blob")
}
func (s *nullSink) object(Type, SHA, []byte) { s.objects++ }
func (s *nullSink) base(context.Context, SHA) (Type, int64, io.ReaderAt, error) {
	return 0, 0, nil, nil
}
