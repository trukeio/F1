// Package svntest builds Subversion repositories with the svn client for
// the tests of the SVN importer (ADR-0019).
package svntest

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Require skips the test if a Subversion tool is missing, or fails it if
// F1_REQUIRE_SVN is set (CI installs Subversion).
func Require(t testing.TB, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("F1_REQUIRE_SVN") != "" {
				t.Fatal(tool + " is not installed")
			}
			t.Skip(tool + " is not installed")
		}
	}
}

// Run runs a command in dir and returns its standard output.
func Run(t testing.TB, dir, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, stderr.Bytes())
	}
	return out
}

// Repo is a repository with a working copy.
type Repo struct {
	t      testing.TB
	Dir    string // the repository
	URL    string // file://Dir
	wc     string
	config string
}

// New creates an empty repository and a working copy of it.
func New(t testing.TB) *Repo {
	t.Helper()
	Require(t, "svn", "svnadmin", "svnlook")
	dir := t.TempDir()
	r := &Repo{t: t, Dir: filepath.Join(dir, "repo"), wc: filepath.Join(dir, "wc"), config: filepath.Join(dir, "config")}
	r.URL = "file://" + r.Dir
	Run(t, dir, "svnadmin", "create", r.Dir)
	Run(t, dir, "svn", "--config-dir", r.config, "-q", "checkout", r.URL, r.wc)
	return r
}

// Svn runs an svn command in the working copy.
func (r *Repo) Svn(args ...string) []byte {
	r.t.Helper()
	return Run(r.t, r.wc, "svn", append([]string{"--config-dir", r.config, "--non-interactive"}, args...)...)
}

// Write writes a file of the working copy, creating its directories.
func (r *Repo) Write(p string, data []byte) {
	r.t.Helper()
	full := filepath.Join(r.wc, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Symlink creates a symbolic link in the working copy.
func (r *Repo) Symlink(target, p string) {
	r.t.Helper()
	if err := os.Symlink(target, filepath.Join(r.wc, p)); err != nil {
		r.t.Fatal(err)
	}
}

// Commit commits the working copy as user and updates it.
func (r *Repo) Commit(user, msg string) {
	r.t.Helper()
	r.Svn("-q", "commit", "--username", user, "-m", msg)
	r.Svn("-q", "update")
}

// Big returns n bytes of pseudo-random data.
func Big(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// Standard builds a repository in the standard layout, one feature per
// revision, and returns it with its youngest revision:
//
//	r1  texts, a binary larger than a delta window and than a chunk, an
//	    executable, a needs-lock file with a MIME type, a symlink
//	r2  edits, the binary in its middle
//	r3  a branch made with a change in the same revision
//	r4  a property deleted, a file deleted
//	r5  a file replaced
//	r6  a tag, a deleted branch, a new directory
//	r7  a directory moved
//	r8  a branch from a subdirectory, a branch whose name needs changing
//	r9  one revision changing trunk and a branch
//	r10 a change to a tag after its creation
//	r11 a branch replaced by a copy of an older trunk
//	r12 a file that becomes a symlink, a symlink that becomes a file
func Standard(t testing.TB) (*Repo, int) {
	t.Helper()
	r := New(t)
	big := Big(300 << 10)

	r.Svn("-q", "mkdir", "trunk", "branches", "tags")
	r.Write("trunk/a.txt", []byte("hello\n"))
	r.Write("trunk/big.bin", big)
	r.Write("trunk/run.sh", []byte("#!/bin/sh\necho hi\n"))
	r.Write("trunk/board.brd", []byte("board v1"))
	r.Symlink("a.txt", "trunk/link")
	r.Svn("-q", "add", "trunk/a.txt", "trunk/big.bin", "trunk/run.sh", "trunk/board.brd", "trunk/link")
	r.Svn("-q", "propset", "svn:executable", "*", "trunk/run.sh")
	r.Svn("-q", "propset", "svn:needs-lock", "*", "trunk/board.brd")
	r.Svn("-q", "propset", "svn:mime-type", "application/octet-stream", "trunk/board.brd")
	r.Commit("alice", "r1: initial")

	r.Write("trunk/a.txt", []byte("hello\nworld\n"))
	copy(big[150<<10:], "an edit in the middle of the big file")
	r.Write("trunk/big.bin", big)
	r.Commit("alice", "r2: edits")

	r.Svn("-q", "copy", "trunk", "branches/x")
	r.Write("branches/x/a.txt", []byte("on the branch\n"))
	r.Commit("bob", "r3: branch with a change")

	r.Svn("-q", "propdel", "svn:needs-lock", "trunk/board.brd")
	r.Svn("-q", "delete", "trunk/run.sh")
	r.Commit("alice", "r4: property deleted, file deleted")

	r.Svn("-q", "delete", "trunk/a.txt")
	r.Write("trunk/a.txt", []byte("replaced\n"))
	r.Svn("-q", "add", "trunk/a.txt")
	r.Commit("alice", "r5: replace")

	r.Svn("-q", "copy", "trunk", "tags/v1")
	r.Svn("-q", "delete", "branches/x")
	r.Write("trunk/sub/deep.txt", []byte("deep\n"))
	r.Svn("-q", "add", "trunk/sub")
	r.Commit("bob", "r6: tag, delete branch, new directory")

	r.Svn("-q", "move", "trunk/sub", "trunk/sub2")
	r.Commit("alice", "r7: move")

	r.Svn("-q", "copy", "trunk/sub2", "branches/sub-only")
	r.Svn("-q", "copy", "trunk", "branches/my branch")
	r.Commit("bob", "r8: branch from a subdirectory, branch with a space")

	r.Write("trunk/a.txt", []byte("trunk and branch\n"))
	r.Write("branches/my branch/a.txt", []byte("branch and trunk\n"))
	r.Commit("alice", "r9: two branches")

	r.Write("tags/v1/a.txt", []byte("changed in a tag\n"))
	r.Commit("bob", "r10: tag change")

	r.Svn("-q", "delete", "branches/sub-only")
	r.Svn("-q", "copy", r.URL+"/trunk@2", "branches/sub-only")
	r.Commit("alice", "r11: branch replaced")

	r.Svn("-q", "delete", "trunk/a.txt", "trunk/link")
	r.Symlink("board.brd", "trunk/a.txt")
	r.Write("trunk/link", []byte("now a file\n"))
	r.Svn("-q", "add", "trunk/a.txt", "trunk/link")
	r.Commit("alice", "r12: file to symlink, symlink to file")
	return r, 12
}
