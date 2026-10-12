package repodir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
)

func newRepo(t *testing.T) *Dir {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ns")
	if err := Create(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func h(s string) hash.Hash { return hash.Sum(5, []byte(s)) }

func TestCreateOpen(t *testing.T) {
	d := newRepo(t)
	m := d.Meta()
	if m.Format != FormatVersion || len(m.UUID) != 36 || m.Layout != "tree" {
		t.Fatalf("meta = %+v", m)
	}
	for _, name := range []string{ConfigFile, RefsFile, LogFile, LocksFile, LockFile, PacksDir, TmpDir} {
		if _, err := os.Stat(d.Join(name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Create(d.Path(), policy.Default()); err == nil {
		t.Error("Create over an existing repository succeeded")
	}
}

func TestOpenIsExclusive(t *testing.T) {
	d := newRepo(t)
	if _, err := Open(d.Path()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: %v, want ErrLocked", err)
	}
	d.Close()
	d2, err := Open(d.Path())
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	d2.Close()
}

func TestOpenCleansTmp(t *testing.T) {
	d := newRepo(t)
	if err := os.WriteFile(d.Join(TmpDir, "x.pack.open"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d2, err := Open(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if entries, _ := os.ReadDir(d2.Join(TmpDir)); len(entries) != 0 {
		t.Fatalf("tmp/ not cleaned: %v", entries)
	}
}

func TestNewerFormatRefused(t *testing.T) {
	d := newRepo(t)
	d.Close()
	cfg := d.Join(ConfigFile)
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(data), "format = 1", "format = 7", 1)), 0o600)
	_, err := Open(d.Path())
	if err == nil || !strings.Contains(err.Error(), "format 7") || !strings.Contains(err.Error(), "supports format 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidConfigRefused(t *testing.T) {
	d := newRepo(t)
	d.Close()
	cfg := d.Join(ConfigFile)
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(data, "git = true\nretention = \"none\"\n"...), 0o600)
	if _, err := Open(d.Path()); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func TestRefs(t *testing.T) {
	d := newRepo(t)
	want := map[string]hash.Hash{"heads/main": h("a"), "tags/v1": h("b"), "heads/feature/x": h("c")}
	if err := d.WriteRefs(want); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadRefs()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v", got)
	}
	data, _ := os.ReadFile(d.Join(RefsFile))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], " heads/feature/x") {
		t.Fatalf("refs file not sorted:\n%s", data)
	}
}

func group(tx string, n int, olds, news []hash.Hash) []LogRecord {
	var recs []LogRecord
	for i := range n {
		recs = append(recs, LogRecord{Time: time.Unix(1700000000, int64(i)), Tx: tx, Principal: "local:alice smith",
			Ref: fmt.Sprintf("heads/b%d", i), Old: olds[i], New: news[i], Reason: "push"})
	}
	return recs
}

func TestLogRoundTrip(t *testing.T) {
	d := newRepo(t)
	g := group("tx1", 2, []hash.Hash{hash.Zero, h("o")}, []hash.Hash{h("a"), h("b")})
	g[0].Reason = ""
	g[1].Reason = "-"
	if err := d.AppendLog(g); err != nil {
		t.Fatal(err)
	}
	var got []LogRecord
	if err := d.ReadLog(func(r LogRecord) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Principal != "local:alice smith" || got[0].Reason != "" || got[1].Reason != "-" || got[1].Old != h("o") {
		t.Fatalf("got %+v", got)
	}
}

// applyGroup simulates a complete ref update: log, then refs.
func applyGroup(t *testing.T, d *Dir, g []LogRecord) {
	t.Helper()
	if err := d.AppendLog(g); err != nil {
		t.Fatal(err)
	}
	refs, _ := d.ReadRefs()
	for _, r := range g {
		if r.New.IsZero() {
			delete(refs, r.Ref)
		} else {
			refs[r.Ref] = r.New
		}
	}
	if err := d.WriteRefs(refs); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverNothingToDo(t *testing.T) {
	d := newRepo(t)
	applyGroup(t, d, group("t1", 1, []hash.Hash{hash.Zero}, []hash.Hash{h("a")}))
	rec, err := d.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if rec != (Recovery{}) {
		t.Fatalf("recovery = %+v", rec)
	}
}

func TestRecoverRollForward(t *testing.T) {
	d := newRepo(t)
	applyGroup(t, d, group("t1", 2, []hash.Hash{hash.Zero, hash.Zero}, []hash.Hash{h("a"), h("b")}))
	// Crash after the log append, before refs is replaced.
	if err := d.AppendLog(group("t2", 2, []hash.Hash{h("a"), h("b")}, []hash.Hash{h("c"), hash.Zero})); err != nil {
		t.Fatal(err)
	}
	rec, err := d.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if rec.RolledForward != 2 {
		t.Fatalf("recovery = %+v", rec)
	}
	refs, _ := d.ReadRefs()
	if refs["heads/b0"] != h("c") || len(refs) != 1 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestRecoverTornWrites(t *testing.T) {
	for _, cut := range []string{"partial line", "partial group"} {
		t.Run(cut, func(t *testing.T) {
			d := newRepo(t)
			applyGroup(t, d, group("t1", 1, []hash.Hash{hash.Zero}, []hash.Hash{h("a")}))
			good, _ := os.ReadFile(d.Join(LogFile))
			torn := formatGroup(group("t2", 3, []hash.Hash{h("a"), hash.Zero, hash.Zero}, []hash.Hash{h("x"), h("y"), h("z")}))
			lines := strings.SplitAfter(string(torn), "\n")
			var tail string
			if cut == "partial line" {
				tail = lines[0] + lines[1] + lines[2][:20]
			} else {
				tail = lines[0] + lines[1]
			}
			os.WriteFile(d.Join(LogFile), append(good, tail...), 0o600)
			rec, err := d.Recover()
			if err != nil {
				t.Fatal(err)
			}
			if rec.TruncatedBytes != int64(len(tail)) || rec.RolledForward != 0 {
				t.Fatalf("recovery = %+v, want %d bytes truncated", rec, len(tail))
			}
			after, _ := os.ReadFile(d.Join(LogFile))
			if string(after) != string(good) {
				t.Fatal("refs.log not restored to the last complete group")
			}
			refs, _ := d.ReadRefs()
			if refs["heads/b0"] != h("a") {
				t.Fatalf("refs changed: %v", refs)
			}
		})
	}
}

// The last group is found even when the log is much larger than the first
// read window and the group itself spans several windows.
func TestRecoverLargeLog(t *testing.T) {
	d := newRepo(t)
	for i := range 2000 {
		applyGroup(t, d, group(fmt.Sprint("small", i), 1, []hash.Hash{h(fmt.Sprint(i - 1))}, []hash.Hash{h(fmt.Sprint(i))}))
	}
	n := 1500 // a group of ~300 KB
	olds := make([]hash.Hash, n)
	news := make([]hash.Hash, n)
	for i := range n {
		news[i] = h(fmt.Sprint("big", i))
	}
	olds[0] = h("1999")
	big := group("big", n, olds, news)
	if err := d.AppendLog(big); err != nil {
		t.Fatal(err)
	}
	rec, err := d.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if rec.RolledForward != n {
		t.Fatalf("recovery = %+v", rec)
	}
}

func TestLocks(t *testing.T) {
	d := newRepo(t)
	now := time.Unix(1700000000, 123).UTC()
	want := []Lock{
		{ID: "l2", Ref: "heads/main", Path: "dir with space/ü%.brd", Owner: "local:bob", Created: now, Expires: now.Add(time.Hour)},
		{ID: "l1", Ref: "heads/main", Path: "a.sch", Owner: "g1:alice", Created: now},
	}
	if err := d.WriteLocks(want); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadLocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[1] || got[1] != want[0] {
		t.Fatalf("got %+v", got)
	}
}

func TestEscape(t *testing.T) {
	for _, s := range []string{"", "-", "--", "a b", "100%", "line\nbreak", "\x00\xff", "plain"} {
		e := escape(s)
		if strings.ContainsAny(e, " \n") || e == "" {
			t.Errorf("escape(%q) = %q", s, e)
		}
		back, err := unescape(e)
		if err != nil || back != s {
			t.Errorf("unescape(escape(%q)) = %q, %v", s, back, err)
		}
	}
	for _, bad := range []string{"%", "%4", "%zz", "a b"} {
		if _, err := unescape(bad); err == nil {
			t.Errorf("unescape(%q) succeeded", bad)
		}
	}
}
