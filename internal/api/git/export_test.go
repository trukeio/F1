package git_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trukeio/f1/internal/api/git"
	"github.com/trukeio/f1/internal/backend"
	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/gittest"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/store"
)

var ctx = context.Background()

// countingBackend records every ranged read.
type countingBackend struct {
	backend.Backend
	mu    sync.Mutex
	reads []read
}

type read struct {
	key    string
	off, n int64
}

func (c *countingBackend) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	c.mu.Lock()
	c.reads = append(c.reads, read{key, off, n})
	c.mu.Unlock()
	return c.Backend.Get(ctx, key, off, n)
}

func (c *countingBackend) reset() {
	c.mu.Lock()
	c.reads = nil
	c.mu.Unlock()
}

func openRepo(t *testing.T, dir string, be backend.Backend) *repo.Repo {
	t.Helper()
	rp, _, err := repo.Open(ctx, dir, repo.Options{Store: store.Options{Backend: be}})
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

func gitDo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// After cache.db is deleted, exporting every ref again gives the same Git
// ids and reads no chunk payload: blob ids come from DigestRecords
// (doc/plan.md, phase 5 acceptance).
func TestRebuildReadsNoChunks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := t.TempDir()
	gitDo(t, src, "init", "-q", "-b", "main")
	for i, f := range []string{"a.txt", "dir/b.txt", "dir/sub/c.bin"} {
		p := filepath.Join(src, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(strings.Repeat(f, 20000*(i+1))), 0o644)
		gitDo(t, src, "add", f)
		gitDo(t, src, "commit", "-q", "-m", "add "+f)
	}
	gitDo(t, src, "tag", "-a", "v1", "-m", "v1")

	dir := filepath.Join(t.TempDir(), "ns")
	cfg, _ := policy.Preset("git")
	if err := repo.Create(dir, cfg); err != nil {
		t.Fatal(err)
	}
	fsb, err := backend.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	be := &countingBackend{Backend: fsb}
	rp := openRepo(t, dir, be)
	if err := gittest.Import(ctx, rp, src); err != nil {
		t.Fatal(err)
	}
	ids := func(rp *repo.Repo) map[string]git.SHA {
		x := git.NewExporter(rp)
		out := map[string]git.SHA{}
		for name, h := range rp.Refs() {
			s, _, err := x.RefTarget(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = s
		}
		return out
	}
	before := ids(rp)
	if err := rp.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cache.Remove(filepath.Join(dir, "cache.db")); err != nil {
		t.Fatal(err)
	}

	rp = openRepo(t, dir, be)
	defer rp.Close(ctx)
	be.reset()
	after := ids(rp)
	for name, s := range before {
		if after[name] != s {
			t.Fatalf("ref %s: %s after the rebuild, %s before", name, after[name], s)
		}
		if want := gitDo(t, src, "rev-parse", "refs/"+name); s.String() != want {
			t.Fatalf("ref %s: %s, Git has %s", name, s, want)
		}
	}
	// No read touches a chunk record.
	packs, err := rp.Store().LivePacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	for _, p := range packs {
		entries, err := rp.Store().Cache().ObjectsInPack(ctx, p.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Type != object.TypeChunk {
				continue
			}
			chunks++
			start, end := int64(e.Offset), int64(e.Offset)+int64(e.Stored)+42
			for _, r := range be.reads {
				if r.key == p.Key && r.off < end && r.off+r.n > start {
					t.Fatalf("export read chunk %s (read %d+%d of %s)", e.Hash.Short(), r.off, r.n, p.Key)
				}
			}
		}
	}
	if chunks == 0 || len(be.reads) == 0 {
		t.Fatalf("test checks nothing: %d chunks, %d reads", chunks, len(be.reads))
	}
}
