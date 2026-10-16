package test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trukeio/f1/pkg/f1client"
)

var (
	f1Once sync.Once
	f1Path string
	f1Err  error
)

// f1Binary builds cmd/f1 once per test run.
func f1Binary(t *testing.T) string {
	t.Helper()
	f1Once.Do(func() {
		dir, err := os.MkdirTemp("", "f1-test-bin-")
		if err != nil {
			f1Err = err
			return
		}
		f1Path = filepath.Join(dir, "f1")
		if out, err := exec.Command("go", "build", "-o", f1Path, "github.com/trukeio/f1/cmd/f1").CombinedOutput(); err != nil {
			f1Err = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if f1Err != nil {
		t.Fatal(f1Err)
	}
	return f1Path
}

// wcUser runs the f1 command line in a working copy as one user.
type wcUser struct {
	t     *testing.T
	dir   string
	token string
}

func (u *wcUser) run(args ...string) (string, int) {
	u.t.Helper()
	cmd := exec.Command(f1Binary(u.t), args...)
	cmd.Dir = u.dir
	cmd.Env = append(os.Environ(), "F1_TOKEN="+u.token)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		u.t.Fatal(err)
	}
	return string(out), code
}

// ok runs a command that must succeed.
func (u *wcUser) ok(args ...string) string {
	u.t.Helper()
	out, code := u.run(args...)
	if code != 0 {
		u.t.Fatalf("f1 %s: exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// fails runs a command that must fail with output containing want.
func (u *wcUser) fails(want string, args ...string) string {
	u.t.Helper()
	out, code := u.run(args...)
	if code == 0 || !strings.Contains(out, want) {
		u.t.Fatalf("f1 %s: exit %d, want failure with %q:\n%s", strings.Join(args, " "), code, want, out)
	}
	return out
}

func (u *wcUser) write(path, content string) {
	u.t.Helper()
	name := filepath.Join(u.dir, path)
	os.MkdirAll(filepath.Dir(name), 0o755)
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		u.t.Fatal(err)
	}
}

func (u *wcUser) read(path string) string {
	u.t.Helper()
	b, err := os.ReadFile(filepath.Join(u.dir, path))
	if err != nil {
		u.t.Fatal(err)
	}
	return string(b)
}

func (u *wcUser) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(u.dir, path))
	return err == nil
}

func (u *wcUser) writable(path string) bool {
	u.t.Helper()
	fi, err := os.Stat(filepath.Join(u.dir, path))
	if err != nil {
		u.t.Fatal(err)
	}
	return fi.Mode().Perm()&0o200 != 0
}

func lines(n int, edit map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if s, ok := edit[i]; ok {
			b.WriteString(s + "\n")
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	return b.String()
}

func randomBytes(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return string(b)
}

// End-to-end scripts with the f1 command line: two working copies,
// concurrent edits, pull with rebase and merge, text and binary
// conflicts, required locks, sparse checkouts and branches.
func TestClientScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1")
	}
	e := start(t, envOpts{})
	e.createNS(t, "hw", "svn") // locks = required:*.sch,*.brd,*.docx
	e.setACL(t, "hw", "group:eng", "", "read,write,lock")
	e.setACL(t, "hw", "user:bob", "", "read,write,lock")
	root := t.TempDir()
	alice := &wcUser{t: t, dir: root, token: e.tokens["alice"]}
	bob := &wcUser{t: t, dir: root, token: e.tokens["bob"]}
	alice.ok("clone", e.url, "hw", "a")
	bob.ok("clone", e.url, "hw", "b")
	alice.dir, bob.dir = filepath.Join(root, "a"), filepath.Join(root, "b")

	// Alice adds files; the schematic needs a lock.
	alice.write("docs/t.txt", lines(10, nil))
	logo := randomBytes(100 << 10)
	alice.write("art/logo.bin", logo)
	alice.write("board/top.sch", "schematic v1\n")
	if out := alice.ok("status"); !strings.Contains(out, "? docs/t.txt") || !strings.Contains(out, "? board/top.sch") {
		t.Fatalf("status before add:\n%s", out)
	}
	alice.ok("add", "docs", "art", "board")
	alice.ok("commit", "-m", "initial")
	alice.fails("requires a lock", "push")
	alice.ok("lock", "board/top.sch")
	alice.ok("push")
	if out := alice.ok("status"); out != "" {
		t.Fatalf("status after push: %q", out)
	}

	// Bob pulls: the schematic is read-only for him, other files are not.
	bob.ok("pull")
	if bob.read("art/logo.bin") != logo || bob.read("docs/t.txt") != lines(10, nil) {
		t.Fatal("bob's checkout differs")
	}
	if bob.writable("board/top.sch") || !bob.writable("docs/t.txt") {
		t.Fatal("required-lock file should be read-only, others writable")
	}

	// Concurrent edits to different lines: push fails, pull rebases, push.
	bob.write("docs/t.txt", lines(10, map[int]string{9: "bob 9"}))
	bob.ok("commit", "-m", "bob edits 9")
	bob.ok("push")
	alice.write("docs/t.txt", lines(10, map[int]string{2: "alice 2"}))
	alice.ok("commit", "-m", "alice edits 2")
	alice.fails("run f1 pull", "push")
	if out := alice.ok("pull"); !strings.Contains(out, "1 local commit(s) rebased") {
		t.Fatalf("pull:\n%s", out)
	}
	want := lines(10, map[int]string{2: "alice 2", 9: "bob 9"})
	if got := alice.read("docs/t.txt"); got != want {
		t.Fatalf("merged:\n%s", got)
	}
	alice.ok("push")
	bob.ok("pull")
	if bob.read("docs/t.txt") != want {
		t.Fatal("bob does not see the merge")
	}
	log := bob.ok("log")
	for _, m := range []string{"alice edits 2", "bob edits 9", "initial"} {
		if !strings.Contains(log, m) {
			t.Fatalf("log lacks %q:\n%s", m, log)
		}
	}

	// Conflicts with uncommitted changes: text and binary.
	bob.write("docs/t.txt", lines(10, map[int]string{2: "alice 2", 5: "bob 5", 9: "bob 9"}))
	bobLogo := randomBytes(50 << 10)
	bob.write("art/logo.bin", bobLogo)
	bob.ok("commit", "-m", "bob 5 and logo")
	bob.ok("push")
	alice.write("docs/t.txt", lines(10, map[int]string{2: "alice 2", 5: "alice 5", 9: "bob 9"}))
	aliceLogo := randomBytes(60 << 10)
	alice.write("art/logo.bin", aliceLogo)
	out := alice.fails("C docs/t.txt", "pull")
	if !strings.Contains(out, "C art/logo.bin") {
		t.Fatalf("pull:\n%s", out)
	}
	wantConflict := lines(4, map[int]string{2: "alice 2"}) + "<<<<<<< mine\nalice 5\n=======\nbob 5\n>>>>>>> theirs\n" +
		"line 6\nline 7\nline 8\nbob 9\nline 10\n"
	if got := alice.read("docs/t.txt"); got != wantConflict {
		t.Fatalf("conflict markers:\n%s", got)
	}
	if alice.read("art/logo.bin.mine") != aliceLogo || alice.read("art/logo.bin.theirs") != bobLogo || alice.exists("art/logo.bin") {
		t.Fatal("binary conflict files")
	}
	if out := alice.ok("status"); !strings.Contains(out, "C art/logo.bin") || !strings.Contains(out, "C docs/t.txt") {
		t.Fatalf("status with conflicts:\n%s", out)
	}
	alice.fails("unresolved conflicts", "commit", "-m", "too early")
	alice.fails("unresolved conflicts", "pull")
	resolved := lines(10, map[int]string{2: "alice 2", 5: "alice and bob 5", 9: "bob 9"})
	alice.write("docs/t.txt", resolved)
	alice.write("art/logo.bin", aliceLogo)
	alice.ok("add", "docs/t.txt", "art/logo.bin")
	if alice.exists("art/logo.bin.mine") || alice.exists("art/logo.bin.theirs") {
		t.Fatal("add should remove the .mine and .theirs files")
	}
	alice.ok("commit", "-m", "resolve")
	alice.ok("push")
	bob.ok("pull")
	if bob.read("docs/t.txt") != resolved || bob.read("art/logo.bin") != aliceLogo {
		t.Fatal("bob does not see the resolution")
	}

	// A conflicting local commit is folded back into working changes, and
	// its message is kept for the next commit.
	bob.write("docs/t.txt", lines(10, map[int]string{2: "alice 2", 3: "bob 3", 5: "alice and bob 5", 9: "bob 9"}))
	bob.ok("commit", "-m", "bob 3")
	bob.ok("push")
	alice.write("docs/t.txt", lines(10, map[int]string{2: "alice 2", 3: "alice 3", 5: "alice and bob 5", 9: "bob 9"}))
	alice.ok("commit", "-m", "alice 3")
	out = alice.fails("C docs/t.txt", "pull")
	if !strings.Contains(out, "1 local commit(s) conflicted") {
		t.Fatalf("pull:\n%s", out)
	}
	alice.write("docs/t.txt", lines(10, map[int]string{2: "alice 2", 3: "alice 3 and bob 3", 5: "alice and bob 5", 9: "bob 9"}))
	alice.ok("add", "docs/t.txt")
	alice.ok("commit") // the kept message
	alice.ok("push")
	if log := alice.ok("log", "--limit", "1"); !strings.Contains(log, "alice 3") {
		t.Fatalf("kept message: %s", log)
	}

	// Uncommitted changes merge cleanly with the server's; diff shows them.
	bob.ok("pull")
	base := bob.read("docs/t.txt")
	bob.write("docs/t.txt", strings.Replace(base, "line 10", "bob 10", 1))
	alice.write("docs/t.txt", strings.Replace(base, "line 1\n", "alice 1\n", 1))
	alice.ok("commit", "-m", "alice 1")
	alice.ok("push")
	bob.ok("pull")
	if got := bob.read("docs/t.txt"); got != strings.Replace(strings.Replace(base, "line 10", "bob 10", 1), "line 1\n", "alice 1\n", 1) {
		t.Fatalf("working merge:\n%s", got)
	}
	if out := bob.ok("status"); !strings.Contains(out, "M docs/t.txt") {
		t.Fatalf("status: %s", out)
	}
	if out := bob.ok("diff"); !strings.Contains(out, "-line 10\n+bob 10\n") || !strings.Contains(out, "--- a/docs/t.txt") {
		t.Fatalf("diff:\n%s", out)
	}
	bob.ok("commit", "-m", "bob 10")
	bob.ok("push")

	// Locks: alice holds the schematic's lock; bob waits for it.
	bob.fails("locked", "lock", "board/top.sch")
	alice.ok("unlock", "board/top.sch")
	if alice.writable("board/top.sch") {
		t.Fatal("unlocked required-lock file should be read-only")
	}
	bob.ok("lock", "board/top.sch")
	if !bob.writable("board/top.sch") {
		t.Fatal("locked file should be writable")
	}
	if out := alice.ok("locks"); !strings.Contains(out, "board/top.sch user:bob") {
		t.Fatalf("locks: %s", out)
	}
	bob.write("board/top.sch", "schematic v2\n")
	bob.ok("commit", "-m", "schematic v2")
	bob.ok("push")
	bob.ok("unlock", "board/top.sch")
	if bob.writable("board/top.sch") {
		t.Fatal("file should be read-only after unlock")
	}
	alice.ok("pull")
	if alice.read("board/top.sch") != "schematic v2\n" {
		t.Fatal("alice does not see the schematic")
	}

	// Deleting a file.
	alice.ok("rm", "art/logo.bin")
	alice.ok("commit", "-m", "remove logo")
	alice.ok("push")
	bob.ok("pull")
	if bob.exists("art/logo.bin") || bob.exists("art") {
		t.Fatal("deleted file (and its empty directory) still checked out")
	}

	// Sparse checkout.
	carol := &wcUser{t: t, dir: root, token: e.tokens["alice"]}
	carol.ok("clone", "--sparse", "docs", e.url, "hw", "c")
	carol.dir = filepath.Join(root, "c")
	if !carol.exists("docs/t.txt") || carol.exists("board") {
		t.Fatal("sparse clone checked out the wrong files")
	}
	carol.ok("sparse", "add", "board")
	if carol.read("board/top.sch") != "schematic v2\n" {
		t.Fatal("sparse add")
	}
	carol.ok("sparse", "remove", "board")
	if carol.exists("board") {
		t.Fatal("sparse remove")
	}
	if out := carol.ok("sparse", "list"); out != "docs\n" {
		t.Fatalf("sparse list: %q", out)
	}
	carol.write("board/new.txt", "outside\n")
	carol.fails("outside the sparse checkout", "add", "board/new.txt")

	// Branches.
	alice.ok("branch", "feature")
	if out := alice.ok("branch"); !strings.Contains(out, "* main") || !strings.Contains(out, "  feature") {
		t.Fatalf("branches: %s", out)
	}
	alice.ok("checkout", "feature")
	alice.write("docs/feature.txt", "only on feature\n")
	alice.ok("add", "docs/feature.txt")
	alice.ok("commit", "-m", "feature work")
	alice.fails("unpushed", "checkout", "main")
	alice.ok("push")
	alice.ok("checkout", "main")
	if alice.exists("docs/feature.txt") {
		t.Fatal("feature file on main")
	}
	alice.ok("checkout", "feature")
	if alice.read("docs/feature.txt") != "only on feature\n" {
		t.Fatal("feature file missing on feature")
	}
	refs, err := e.alice.Refs(ctx, "hw")
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs = %v, %v", refs, err)
	}
}

// Paths from the server that would escape the working copy or overwrite
// its metadata are refused.
func TestClientUnsafePaths(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "p", "")
	r := put(t, e.admin, "p", "ok.txt", "fine", "new")
	if _, err := e.admin.Put(ctx, "p", "main", ".f1/state.json", strings.NewReader("{}"), 2, &f1client.PutOptions{Parent: r.Commit}); err != nil {
		t.Fatal(err)
	}
	u := &wcUser{t: t, dir: t.TempDir(), token: e.tokens["admin"]}
	out := u.ok("clone", e.url, "p", "w")
	if !strings.Contains(out, "skipping") {
		t.Fatalf("clone did not warn:\n%s", out)
	}
	u.dir = filepath.Join(u.dir, "w")
	if u.read("ok.txt") != "fine" || !bytes.HasPrefix([]byte(u.read(".f1/state.json")), []byte("{\n")) {
		t.Fatal("metadata overwritten")
	}
}

// TestClientRebasedPush checks that f1 push lands on top of others'
// changes to other files, and updates the working copy (ADR-0014).
func TestClientRebasedPush(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1")
	}
	e := start(t, envOpts{})
	e.createNS(t, "docs", "")
	e.setACL(t, "docs", "group:eng", "", "read,write,lock")
	e.setACL(t, "docs", "user:bob", "", "read,write,lock")
	root := t.TempDir()
	alice := &wcUser{t: t, dir: root, token: e.tokens["alice"]}
	bob := &wcUser{t: t, dir: root, token: e.tokens["bob"]}
	alice.ok("clone", e.url, "docs", "a")
	alice.dir = filepath.Join(root, "a")
	alice.write("a.txt", "a1\n")
	alice.write("b.txt", "b1\n")
	alice.ok("add", "a.txt", "b.txt")
	alice.ok("commit", "-m", "initial")
	alice.ok("push")
	bob.ok("clone", e.url, "docs", "b")
	bob.dir = filepath.Join(root, "b")

	// Bob changes a.txt; Alice, not up to date, changes b.txt in two
	// commits and has an uncommitted edit of a third file.
	bob.write("a.txt", "a2 by bob\n")
	bob.ok("commit", "-m", "bob a")
	bob.ok("push")
	alice.write("b.txt", "b2\n")
	alice.ok("commit", "-m", "alice b2")
	alice.write("b.txt", "b3\n")
	alice.write("new.txt", "new\n")
	alice.ok("add", "new.txt")
	alice.ok("commit", "-m", "alice b3")
	alice.write("notes.txt", "draft\n")
	out := alice.ok("push")
	if !strings.Contains(out, "rebased the push") || !strings.Contains(out, "1 file(s) updated") {
		t.Fatalf("push:\n%s", out)
	}
	if alice.read("a.txt") != "a2 by bob\n" || alice.read("b.txt") != "b3\n" || alice.read("notes.txt") != "draft\n" {
		t.Fatal("working copy after a rebased push")
	}
	if out := alice.ok("status"); out != "? notes.txt\n" {
		t.Fatalf("status after a rebased push: %q", out)
	}
	if out := alice.ok("pull"); !strings.Contains(out, "0 file(s) updated") {
		t.Fatalf("pull after a rebased push:\n%s", out)
	}
	log := alice.ok("log")
	if i, j := strings.Index(log, "alice b3"), strings.Index(log, "bob a"); i < 0 || j < 0 || i > j {
		t.Fatalf("log:\n%s", log)
	}

	// The same file changed on both sides still needs a pull.
	bob.ok("pull")
	bob.write("b.txt", "b4 by bob\n")
	bob.ok("commit", "-m", "bob b4")
	bob.ok("push")
	alice.write("b.txt", "b4 by alice\n")
	alice.ok("commit", "-m", "alice b4")
	if out := alice.fails("run f1 pull", "push"); !strings.Contains(out, "changed there: b.txt") {
		t.Fatalf("push of a file changed on the server:\n%s", out)
	}

	// --no-rebase refuses any change on the server.
	bob.write("new.txt", "newer\n")
	bob.ok("commit", "-m", "bob new")
	refs, err := e.admin.Refs(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	put(t, e.admin, "docs", "other.txt", "other\n", refs[0].Commit)
	bob.fails("run f1 pull", "push", "--no-rebase")
	bob.ok("push")
	if bob.read("other.txt") != "other\n" {
		t.Fatal("other.txt not updated by the rebased push")
	}
}
