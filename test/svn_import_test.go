package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/svntest"
	"github.com/trukeio/f1/pkg/f1client"
)

// TestSvnImport runs "f1d repo import-svn" offline and against a running
// server, and reads the result through the native API and Git (ADR-0019).
// The revision-by-revision comparison with svn export is in
// internal/svnimport.
func TestSvnImport(t *testing.T) {
	r, youngest := svntest.Standard(t)
	bin := f1dBinary(t)
	dir := t.TempDir()
	dump := filepath.Join(dir, "repo.dump")
	if err := os.WriteFile(dump, svntest.Run(t, dir, "svnadmin", "dump", "-q", "--deltas", r.Dir), 0o600); err != nil {
		t.Fatal(err)
	}
	authors := filepath.Join(dir, "authors.toml")
	os.WriteFile(authors, []byte("alice = \"Alice Example <alice@example.com>\"\n"), 0o600)
	addr := freeAddr(t)
	cfg := filepath.Join(dir, "f1d.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\ndata_dir = \"data\"\nlisten = %q\n[server.s3]\nlisten = \"\"\n[log]\nlevel = \"warn\"\n", addr)), 0o600)
	runCLI(t, bin, "user", "add", "--config", cfg, "admin")
	runCLI(t, bin, "acl", "set", "--config", cfg, "*", "user:admin", "", "admin")
	token := strings.TrimSpace(runCLI(t, bin, "token", "create", "--config", cfg, "admin"))

	// Offline, from a file.
	out := runCLI(t, bin, "repo", "import-svn", "--config", cfg, "--create", "svn", "--authors", authors, "boards", dump)
	for _, want := range []string{
		"boards created",
		fmt.Sprintf("boards: imported to r%d: %d revisions", youngest, youngest),
		"trunk → heads/main",
		"branches/my branch → heads/my-branch",
		"heads/svn-deleted/x-r6",
		"authors not in the authors file: bob",
		`locks = "required:*.brd"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("offline import output lacks %q:\n%s", want, out)
		}
	}

	// Through the running server, from standard input, into a Git
	// namespace.
	serve := exec.Command(bin, "serve", "--config", cfg)
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		serve.Process.Signal(os.Interrupt)
		serve.Wait()
	})
	url := "http://" + addr
	waitHealthy(t, url)
	in, err := os.Open(dump)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	cmd := exec.Command(bin, "repo", "import-svn", "--config", cfg, "--create", "git", "--authors", authors, "code", "-")
	cmd.Stdin = in
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("online import: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("code: imported to r%d", youngest)) {
		t.Fatalf("online import output:\n%s", stdout.String())
	}

	ctx := context.Background()
	c := f1client.New(url, token)
	for _, ns := range []string{"boards", "code"} {
		refs, err := c.Refs(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ref := range refs {
			names = append(names, ref.Name)
		}
		slices.Sort(names)
		want := []string{"heads/main", "heads/my-branch", "heads/sub-only", "heads/svn-deleted/sub-only-r11", "heads/svn-deleted/x-r6", "tags/v1"}
		if !slices.Equal(names, want) {
			t.Errorf("%s refs %v", ns, names)
		}
		rc, _, err := c.Get(ctx, ns, "my-branch", "a.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if string(data) != "branch and trunk\n" {
			t.Errorf("%s my-branch a.txt = %q", ns, data)
		}
		log, err := c.Log(ctx, ns, "main", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		first := log[len(log)-1]
		if first.Author.Name != "Alice Example" || !strings.Contains(first.Message, "SVN-Revision: 1\n") {
			t.Errorf("%s first commit %+v", ns, first)
		}
	}

	// The Git namespace clones, with the history.
	if _, err := exec.LookPath("git"); err == nil {
		clone := filepath.Join(dir, "clone")
		git := exec.Command("git", "-c", "http.extraHeader=Authorization: Bearer "+token, "clone", "-q", url+"/git/code.git", clone)
		if out, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		out, err := exec.Command("git", "-C", clone, "log", "--format=%an %s", "main").Output()
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); lines[len(lines)-1] != "Alice Example r1: initial" {
			t.Errorf("git log:\n%s", out)
		}
	}

	// A second import into the same namespace is refused.
	cmd = exec.Command(bin, "repo", "import-svn", "--config", cfg, "code", dump)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "not empty") {
		t.Errorf("second import: %v\n%s", err, out)
	}
}

func waitHealthy(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(url + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
