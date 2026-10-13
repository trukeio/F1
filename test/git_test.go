package test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/gittest"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/pkg/f1client"
)

// gitCmd runs git in dir with a clean environment and returns its output.
func gitCmd(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, env, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func gitRun(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	home := filepath.Join(os.TempDir(), "f1-git-home")
	os.MkdirAll(home, 0o700)
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// hashObject writes a raw object with git hash-object --literally.
func hashObject(t *testing.T, dir, typ string, data []byte) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--literally", "-t", typ, "--stdin")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func catFile(t *testing.T, dir, sha string) []byte {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "cat-file", "-p", sha).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// makeFixture builds a Git repository with the cases the export must
// reproduce exactly (doc/plan.md, phase 5 acceptance).
func makeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	g := func(args ...string) string { return strings.TrimSpace(gitCmd(t, dir, nil, args...)) }
	gEnv := func(env []string, args ...string) string { return strings.TrimSpace(gitCmd(t, dir, env, args...)) }
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	g("init", "-q", "-b", "main")
	g("config", "commit.gpgsign", "false")
	g("config", "core.quotepath", "false")
	// An empty tree.
	g("commit", "-q", "--allow-empty", "-m", "empty root")
	// Files: executable, symlink, nested, non-UTF-8 name.
	write("README", "hello\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho run\n", 0o755)
	write("src/a/b/deep.txt", "deep\n", 0o644)
	write("src/\xff\xfename.bin", "latin1 name\n", 0o644)
	if err := os.Symlink("README", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-q", "-m", "files")
	// A submodule (gitlink).
	g("update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",vendor/sub")
	g("commit", "-q", "-m", "submodule")
	// Unusual time zone.
	write("tz.txt", "tz\n", 0o644)
	g("add", "tz.txt")
	gEnv([]string{"GIT_AUTHOR_DATE=1700000000 -0030", "GIT_COMMITTER_DATE=1700000100 +1345"}, "commit", "-q", "-m", "odd zone")
	// Non-UTF-8 message with an encoding header.
	os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o644)
	msgFile := filepath.Join(t.TempDir(), "msg")
	os.WriteFile(msgFile, []byte("caf\xe9 latin-1\n"), 0o644)
	write("enc.txt", "enc\n", 0o644)
	g("add", "enc.txt")
	g("-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "-F", msgFile)
	// A merge.
	g("checkout", "-q", "-b", "feature")
	write("feature.txt", "feature\n", 0o644)
	g("add", "feature.txt")
	g("commit", "-q", "-m", "feature")
	g("checkout", "-q", "main")
	write("main.txt", "main\n", 0o644)
	g("add", "main.txt")
	g("commit", "-q", "-m", "main work")
	g("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	// An octopus merge.
	for _, b := range []string{"o1", "o2", "o3"} {
		g("checkout", "-q", "-b", b, "main")
		write(b+".txt", b+"\n", 0o644)
		g("add", b+".txt")
		g("commit", "-q", "-m", b)
	}
	g("checkout", "-q", "main")
	g("merge", "-q", "-m", "octopus", "o1", "o2", "o3")
	// A signed commit (the signature is not checked; the header must
	// survive byte for byte).
	headRaw := catFile(t, dir, "HEAD")
	head := g("rev-parse", "HEAD")
	hdr, _, _ := bytes.Cut(headRaw, []byte("\n\n"))
	sig := "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n=abcd\n-----END PGP SIGNATURE-----"
	signed := fmt.Sprintf("tree %s\nparent %s\n%s\ngpgsig %s\n\nsigned commit\n",
		g("rev-parse", "HEAD^{tree}"), head, afterParents(hdr), strings.ReplaceAll(sig, "\n", "\n "))
	signedSHA := hashObject(t, dir, "commit", []byte(signed))
	g("update-ref", "refs/heads/main", signedSHA)
	// A non-canonical tree (mode 100664) on its own branch.
	blob := g("rev-parse", "HEAD:README")
	b, _ := hex.DecodeString(blob)
	oddTree := hashObject(t, dir, "tree", append([]byte("100664 odd\x00"), b...))
	oddCommit := g("commit-tree", oddTree, "-m", "odd mode")
	g("update-ref", "refs/heads/odd", oddCommit)
	// Tags: lightweight, annotated, signed, a tag of a tag.
	g("tag", "light")
	g("tag", "-a", "v1", "-m", "release 1")
	signedTag := fmt.Sprintf("object %s\ntype commit\ntag v1-signed\ntagger Test <test@example.com> 1700000000 +0200\n\nsigned tag\n%s\n", signedSHA, sig)
	g("update-ref", "refs/tags/v1-signed", hashObject(t, dir, "tag", []byte(signedTag)))
	g("tag", "-a", "v1-outer", "-m", "tag of a tag", "v1")
	return dir
}

// afterParents returns the header lines after tree and parent lines.
func afterParents(hdr []byte) string {
	var keep []string
	for _, l := range strings.Split(string(hdr), "\n") {
		if !strings.HasPrefix(l, "tree ") && !strings.HasPrefix(l, "parent ") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// gitRefs returns "sha refname" lines of a repository.
func gitRefs(t *testing.T, dir string, prefix string) string {
	t.Helper()
	return gitCmd(t, dir, nil, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
}

// importFixture creates namespace ns from a Git repository before the
// server starts (the test-only importer).
func importFixture(t *testing.T, src, ns string) func(string) {
	return func(dataDir string) {
		cfg, _ := policy.Preset("git")
		dir := filepath.Join(dataDir, server.ReposDir, ns)
		if err := repo.Create(dir, cfg); err != nil {
			t.Fatal(err)
		}
		rp, _, err := repo.Open(ctx, dir, repo.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer rp.Close(ctx)
		if err := gittest.Import(ctx, rp, src); err != nil {
			t.Fatal(err)
		}
	}
}

// gitURL is the clone URL of a namespace with the user's token.
func (e *env) gitURL(user, ns string) string {
	u, _ := url.Parse(e.url)
	u.User = url.UserPassword(user, e.tokens[user])
	u.Path = "/git/" + ns + ".git"
	return u.String()
}

// Fixtures imported into F1 clone with identical ids, over protocols v2,
// v1 and v0, and pass git fsck.
func TestGitCloneFixture(t *testing.T) {
	requireGit(t)
	src := makeFixture(t)
	e := start(t, envOpts{setup: importFixture(t, src, "fix")})
	want := gitRefs(t, src, "refs/heads") + gitRefs(t, src, "refs/tags")
	for _, v := range []string{"2", "1", "0"} {
		t.Run("protocol v"+v, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "clone")
			gitCmd(t, "", nil, "-c", "protocol.version="+v, "clone", "-q", "--mirror", e.gitURL("admin", "fix"), dst)
			got := gitRefs(t, dst, "refs/heads") + gitRefs(t, dst, "refs/tags")
			if got != want {
				t.Fatalf("refs differ:\n%s\nwant:\n%s", got, want)
			}
			if out, err := gitRun(dst, nil, "fsck", "--full", "--strict", "--no-dangling"); err != nil && !strings.Contains(out, "zeroPaddedFilemode") && !strings.Contains(out, "badFilemode") {
				t.Fatalf("git fsck: %v\n%s", err, out)
			}
			if gitCmd(t, dst, nil, "rev-parse", "HEAD") != gitCmd(t, src, nil, "rev-parse", "HEAD") {
				t.Fatal("HEAD differs")
			}
		})
	}
	// A working-tree clone checks out the files, the non-UTF-8 name and
	// the symlink.
	dst := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "fix"), dst)
	if b, err := os.ReadFile(filepath.Join(dst, "src", "\xff\xfename.bin")); err != nil || string(b) != "latin1 name\n" {
		t.Fatalf("non-UTF-8 file: %q %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "README" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "bin", "run.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("executable: %v %v", fi, err)
	}
	// Incremental fetch after new history on the server side is covered by
	// TestGitFetchNativeWrites; ls-remote works.
	out := gitCmd(t, "", nil, "ls-remote", e.gitURL("admin", "fix"))
	if !strings.Contains(out, "refs/tags/v1-outer^{}") {
		t.Fatalf("ls-remote lacks peeled tags:\n%s", out)
	}
}

// Partial clone and shallow clone.
func TestGitPartialAndShallow(t *testing.T) {
	requireGit(t)
	src := makeFixture(t)
	e := start(t, envOpts{setup: importFixture(t, src, "fix")})
	u := e.gitURL("admin", "fix")

	dst := filepath.Join(t.TempDir(), "partial")
	gitCmd(t, "", nil, "clone", "-q", "--filter=blob:none", "--no-checkout", u, dst)
	if out := gitCmd(t, dst, nil, "rev-list", "--objects", "--missing=print", "HEAD"); !strings.Contains(out, "?") {
		t.Fatal("blob:none clone has every blob")
	}
	gitCmd(t, dst, nil, "checkout", "-q", "main") // fetches blobs on demand
	if b, err := os.ReadFile(filepath.Join(dst, "README")); err != nil || string(b) != "hello\n" {
		t.Fatalf("lazily fetched README: %q %v", b, err)
	}
	gitCmd(t, "", nil, "clone", "-q", "--filter=blob:limit=8", "--no-checkout", u, filepath.Join(t.TempDir(), "limit"))
	gitCmd(t, "", nil, "clone", "-q", "--filter=tree:0", "--no-checkout", u, filepath.Join(t.TempDir(), "tree0"))

	for _, v := range []string{"2", "0"} {
		sh := filepath.Join(t.TempDir(), "shallow")
		gitCmd(t, "", nil, "-c", "protocol.version="+v, "clone", "-q", "--depth=1", "--no-single-branch", u, sh)
		if n := strings.TrimSpace(gitCmd(t, sh, nil, "rev-list", "--count", "HEAD")); n != "1" {
			t.Fatalf("v%s depth 1: %s commits", v, n)
		}
		gitCmd(t, sh, nil, "-c", "protocol.version="+v, "fetch", "-q", "--deepen=2")
		if n := strings.TrimSpace(gitCmd(t, sh, nil, "rev-list", "--count", "HEAD")); n == "1" {
			t.Fatalf("v%s deepen did not add commits", v)
		}
		gitCmd(t, sh, nil, "-c", "protocol.version="+v, "fetch", "-q", "--unshallow")
		if gitCmd(t, sh, nil, "rev-list", "--count", "HEAD") != gitCmd(t, src, nil, "rev-list", "--count", "HEAD") {
			t.Fatalf("v%s unshallow: history differs", v)
		}
		gitCmd(t, sh, nil, "fsck", "--no-dangling")
	}
}

// Files written through the native API appear in Git with deterministic
// ids, and an incremental fetch gets only the new history.
func TestGitFetchNativeWrites(t *testing.T) {
	requireGit(t)
	e := start(t, envOpts{})
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "code", Preset: "git"}); err != nil {
		t.Fatal(err)
	}
	r1 := put(t, e.admin, "code", "a.txt", "one\n", "new")
	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "code"), dst)
	blob := strings.TrimSpace(gitCmd(t, dst, nil, "rev-parse", "HEAD:a.txt"))
	if sum := sha1.Sum([]byte("blob 4\x00one\n")); blob != hex.EncodeToString(sum[:]) {
		t.Fatalf("blob id %s", blob)
	}
	v0 := filepath.Join(t.TempDir(), "v0")
	gitCmd(t, "", nil, "-c", "protocol.version=0", "clone", "-q", e.gitURL("admin", "code"), v0)
	r2 := put(t, e.admin, "code", "dir/b.txt", "two\n", r1.Commit)
	put(t, e.admin, "code", "dir/c.txt", "three\n", r2.Commit)
	gitCmd(t, dst, nil, "pull", "-q")
	if b, err := os.ReadFile(filepath.Join(dst, "dir", "b.txt")); err != nil || string(b) != "two\n" {
		t.Fatalf("after pull: %q %v", b, err)
	}
	gitCmd(t, dst, nil, "fsck", "--no-dangling")
	// Protocol v0 negotiation with haves (multi_ack_detailed).
	gitCmd(t, v0, nil, "-c", "protocol.version=0", "pull", "-q")
	if gitCmd(t, v0, nil, "rev-parse", "HEAD") != gitCmd(t, dst, nil, "rev-parse", "HEAD") {
		t.Fatal("v0 pull ended elsewhere")
	}
	gitCmd(t, v0, nil, "fsck", "--no-dangling")
	// Access control: bob has no access; alice with partial read cannot
	// use Git (D13).
	if out, err := gitRun("", nil, "ls-remote", e.gitURL("bob", "code")); err == nil {
		t.Fatalf("bob listed refs: %s", out)
	}
	e.setACL(t, "code", "user:alice", "dir/", "read")
	if out, err := gitRun("", nil, "ls-remote", e.gitURL("alice", "code")); err == nil || !strings.Contains(out, "whole namespace") {
		t.Fatalf("partial reader: %v %s", err, out)
	}
	if out, err := gitRun("", nil, "ls-remote", strings.Replace(e.gitURL("admin", "code"), e.tokens["admin"], "wrong", 1)); err == nil {
		t.Fatalf("wrong token: %s", out)
	}
}

// After cache.db is deleted, the Git ids are recomputed identically.
func TestGitCacheRebuild(t *testing.T) {
	requireGit(t)
	src := makeFixture(t)
	e := start(t, envOpts{setup: importFixture(t, src, "fix")})
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "code", Preset: "git"}); err != nil {
		t.Fatal(err)
	}
	put(t, e.admin, "code", "x", "native write\n", "new")
	before := gitCmd(t, "", nil, "ls-remote", e.gitURL("admin", "fix")) + gitCmd(t, "", nil, "ls-remote", e.gitURL("admin", "code"))
	e.stop()
	for _, ns := range []string{"fix", "code"} {
		for _, f := range []string{"cache.db", "cache.db-wal", "cache.db-shm"} {
			os.Remove(filepath.Join(e.dataDir, server.ReposDir, ns, f))
		}
	}
	e2 := start(t, envOpts{dataDir: e.dataDir})
	after := gitCmd(t, "", nil, "ls-remote", e2.gitURL("admin", "fix")) + gitCmd(t, "", nil, "ls-remote", e2.gitURL("admin", "code"))
	if before != after {
		t.Fatalf("ids changed after the cache rebuild:\n%s\nvs\n%s", before, after)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", nil, "clone", "-q", "--mirror", e2.gitURL("admin", "fix"), dst)
	if gitRefs(t, dst, "refs/heads") != gitRefs(t, src, "refs/heads") {
		t.Fatal("clone after rebuild differs")
	}
}
