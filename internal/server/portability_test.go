package server

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/serverdb"
	"github.com/trukeio/f1/internal/testutil"
)

var ctx = context.Background()

var quiet = slog.New(slog.DiscardHandler)

// server is an in-process stand-in for a running f1d: a data directory with
// server.db and an open Manager.
type testServer struct {
	dataDir string
	db      *serverdb.DB
	mgr     *Manager
}

func startServer(t *testing.T, dataDir string) *testServer {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, ReposDir), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := serverdb.Open(ctx, filepath.Join(dataDir, ServerDB))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := OpenManager(ctx, dataDir, db, quiet, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{dataDir, db, mgr}
}

func (s *testServer) stop(t *testing.T) {
	t.Helper()
	if err := s.mgr.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
}

func (s *testServer) reposDir() string { return filepath.Join(s.dataDir, ReposDir) }

func (s *testServer) create(t *testing.T, ns string) *repo.Repo {
	t.Helper()
	if err := repo.Create(filepath.Join(s.reposDir(), ns), policy.Default()); err != nil {
		t.Fatal(err)
	}
	if err := s.mgr.Attach(ctx, ns); err != nil {
		t.Fatal(err)
	}
	r, err := s.mgr.Get(ns)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func commitFiles(t testing.TB, r *repo.Repo, ref string, files map[string][]byte) (hash.Hash, error) {
	parent := r.Ref(ref)
	var changes []repo.Change
	for p, d := range files {
		res, err := r.Ingest(ctx, strings.NewReader(string(d)), int64(len(d)))
		if err != nil {
			return hash.Zero, err
		}
		changes = append(changes, repo.Change{Op: repo.OpPut, Path: p, Entry: object.Entry{Kind: object.KindFile, Target: res.File}})
	}
	return r.Commit(ctx, repo.CommitRequest{Ref: ref, Parent: &parent, Changes: changes, Author: repo.Person{Name: "t"}})
}

// populate writes a few commits on two branches, with files of several
// sizes and a tag.
func populate(t *testing.T, r *repo.Repo) {
	t.Helper()
	for i := range 5 {
		files := map[string][]byte{}
		for j := range 4 {
			files[fmt.Sprintf("dir%d/file%d.bin", j, i)] = testutil.Bytes(fmt.Sprint(i, j), 1000+i*70000+j*13)
		}
		if _, err := commitFiles(t, r, "main", files); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := commitFiles(t, r, "feature", map[string][]byte{"f.txt": []byte("feature")}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefs(ctx, []repo.RefUpdate{{Ref: "tags/v1", New: r.Ref("main")}}, repo.UpdateOptions{Principal: "t", Reason: "tag"}); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every ref to its commit and the SHA-256 of every file in it.
func snapshot(t *testing.T, r *repo.Repo, refs map[string]hash.Hash) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, commit := range refs {
		out[name] = commit.String()
		err := r.List(ctx, commit, repo.ListOpts{}, func(it repo.ListItem) error {
			rd, _, err := r.Open(ctx, commit, it.Path)
			if err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.Copy(h, rd); err != nil {
				return err
			}
			out[name+":"+it.Path] = hex.EncodeToString(h.Sum(nil))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func sameSnapshot(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("snapshot has %d entries, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("snapshot differs at %s", k)
		}
	}
}

func fsckClean(t *testing.T, path string) {
	t.Helper()
	rep, err := gc.CheckDir(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() {
		t.Fatalf("fsck %s: %v", path, rep.Findings)
	}
}

// Offline copy: stop the server, cp -r a repository to a second data
// directory, start a second server.
func TestOfflineCopy(t *testing.T) {
	for _, withCache := range []bool{true, false} {
		t.Run(fmt.Sprint("cache=", withCache), func(t *testing.T) {
			a := startServer(t, t.TempDir())
			r := a.create(t, "ns")
			populate(t, r)
			want := snapshot(t, r, r.Refs())
			a.stop(t)

			bDir := t.TempDir()
			os.MkdirAll(filepath.Join(bDir, ReposDir), 0o700)
			cmd := exec.Command("cp", "-r", filepath.Join(a.reposDir(), "ns"), filepath.Join(bDir, ReposDir, "ns"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("cp: %v: %s", err, out)
			}
			if !withCache {
				for _, f := range []string{"cache.db", "cache.db-wal", "cache.db-shm"} {
					os.Remove(filepath.Join(bDir, ReposDir, "ns", f))
				}
			}
			b := startServer(t, bDir)
			rb, err := b.mgr.Get("ns")
			if err != nil {
				t.Fatalf("copied repository not registered at start: %v", err)
			}
			sameSnapshot(t, snapshot(t, rb, rb.Refs()), want)
			b.stop(t)
			fsckClean(t, filepath.Join(bDir, ReposDir, "ns"))
		})
	}
}

// Frozen copy: freeze, zip -r with the system zip tool, unfreeze; import
// the ZIP on a second server.
func TestFrozenCopyWithZip(t *testing.T) {
	if _, err := exec.LookPath("zip"); err != nil {
		t.Skip("zip is not installed")
	}
	a := startServer(t, t.TempDir())
	defer a.stop(t)
	r := a.create(t, "ns")
	populate(t, r)

	if err := a.mgr.Freeze(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t, r, r.Refs())
	if _, err := commitFiles(t, r, "main", map[string][]byte{"x": []byte("x")}); !errors.Is(err, repo.ErrFrozen) {
		t.Fatalf("commit while frozen: %v", err)
	}
	if _, err := r.Submit(ctx, repo.SubmitRequest{Ref: "main", Change: repo.Change{Op: repo.OpDelete, Path: "f"}}); !errors.Is(err, repo.ErrFrozen) {
		t.Fatalf("group commit while frozen: %v", err)
	}
	zipPath := filepath.Join(t.TempDir(), "ns.zip")
	cmd := exec.Command("zip", "-r", "-q", zipPath, "ns")
	cmd.Dir = a.reposDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zip: %v: %s", err, out)
	}
	if err := a.mgr.Unfreeze("ns"); err != nil {
		t.Fatal(err)
	}
	if _, err := commitFiles(t, r, "main", map[string][]byte{"after": []byte("unfrozen")}); err != nil {
		t.Fatalf("commit after unfreeze: %v", err)
	}

	b := startServer(t, t.TempDir())
	defer b.stop(t)
	res, err := ImportRepo(ctx, b.reposDir(), zipPath, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "ns" || !res.Fsck.Clean() {
		t.Fatalf("import = %+v", res)
	}
	if err := b.mgr.Attach(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	rb, _ := b.mgr.Get("ns")
	sameSnapshot(t, snapshot(t, rb, rb.Refs()), want)
}

// Online archive under load: writes continue during the archive and none
// fails; the imported archive has exactly the refs read at archive start.
func TestOnlineArchiveUnderLoad(t *testing.T) {
	a := startServer(t, t.TempDir())
	defer a.stop(t)
	r := a.create(t, "ns")
	populate(t, r)

	stop := make(chan struct{})
	var writes, failures atomic.Int64
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				d := testutil.Bytes(fmt.Sprint(w, i), 500+i%7*30000)
				res, err := r.Ingest(ctx, strings.NewReader(string(d)), int64(len(d)))
				if err == nil {
					_, err = r.Submit(ctx, repo.SubmitRequest{Ref: "main",
						Change: repo.Change{Op: repo.OpPut, Path: fmt.Sprintf("load/w%d/%d", w, i), Entry: object.Entry{Kind: object.KindFile, Target: res.File}}})
				}
				if err != nil {
					t.Error(err)
					failures.Add(1)
				}
				writes.Add(1)
			}
		})
	}
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := commitFiles(t, r, "side", map[string][]byte{fmt.Sprint("s", i): []byte(fmt.Sprint(i))}); err != nil {
				t.Error(err)
				failures.Add(1)
			}
			writes.Add(1)
		}
	})
	time.Sleep(100 * time.Millisecond)
	zipPath := filepath.Join(t.TempDir(), "ns.zip")
	info, err := ArchiveToFile(ctx, zipPath, func(w io.Writer) (repo.ArchiveInfo, error) { return a.mgr.Archive(ctx, "ns", w) })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d of %d writes failed during the archive", failures.Load(), writes.Load())
	}
	t.Logf("%d writes during the test; archive has %d packs, %d bytes", writes.Load(), info.Packs, info.Bytes)
	want := snapshot(t, r, info.Refs)

	b := startServer(t, t.TempDir())
	defer b.stop(t)
	if _, err := ImportRepo(ctx, b.reposDir(), zipPath, ImportOptions{Name: "copy"}); err != nil {
		t.Fatal(err)
	}
	fsckClean(t, filepath.Join(b.reposDir(), "copy"))
	if err := b.mgr.Attach(ctx, "copy"); err != nil {
		t.Fatal(err)
	}
	rb, _ := b.mgr.Get("copy")
	if fmt.Sprint(rb.Refs()) != fmt.Sprint(info.Refs) {
		t.Fatal("imported refs differ from the refs read at archive start")
	}
	sameSnapshot(t, snapshot(t, rb, rb.Refs()), want)
}

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, content)
	}
	zw.Close()
	f.Close()
	return p
}

func TestImportRefusals(t *testing.T) {
	reposDir := filepath.Join(t.TempDir(), "repos")
	cfg := "format = 1\nuuid = \"u\"\n"
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{"newer format", map[string]string{"ns/f1repo.toml": "format = 2\nuuid = \"u\"\n", "ns/refs": ""}, "format 2"},
		{"zip slip", map[string]string{"../evil": "x"}, "escapes"},
		{"two roots", map[string]string{"a/f1repo.toml": cfg, "b/refs": ""}, "more than one root"},
		{"not a repository", map[string]string{"ns/readme": "x"}, "not an F1 repository"},
		{"no refs", map[string]string{"ns/f1repo.toml": cfg}, "no refs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ImportRepo(ctx, reposDir, writeZip(t, tt.files), ImportOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if entries, _ := os.ReadDir(reposDir); len(entries) != 0 {
				t.Fatalf("import left files behind: %v", entries)
			}
		})
	}
}

func TestImportNameAndUUID(t *testing.T) {
	a := startServer(t, t.TempDir())
	r := a.create(t, "ns")
	populate(t, r)
	uuid := r.Meta().UUID
	a.stop(t)
	src := filepath.Join(a.reposDir(), "ns")
	reposDir := filepath.Join(t.TempDir(), "repos")

	res, err := ImportRepo(ctx, reposDir, src, ImportOptions{})
	if err != nil || res.Name != "ns" || res.UUID != uuid {
		t.Fatalf("import = %+v, %v", res, err)
	}
	if _, err := ImportRepo(ctx, reposDir, src, ImportOptions{}); !errors.Is(err, ErrRepoExists) {
		t.Fatalf("second import under the same name: %v", err)
	}
	res, err = ImportRepo(ctx, reposDir, src, ImportOptions{Name: "ns2", NewUUID: true})
	if err != nil || res.UUID == uuid {
		t.Fatalf("import --new-uuid = %+v, %v", res, err)
	}
	if meta, _ := repodir.ReadMeta(filepath.Join(reposDir, "ns2")); meta.UUID != res.UUID {
		t.Fatal("new uuid not written")
	}
	if _, err := ImportRepo(ctx, reposDir, src, ImportOptions{Name: "Bad Name"}); err == nil {
		t.Fatal("invalid namespace accepted")
	}
}

// The admin socket drives a running server: list, freeze, archive, detach
// and attach. A second server on the same data directory is refused.
func TestAdminSocket(t *testing.T) {
	dataDir := t.TempDir()
	if err := repo.Create(filepath.Join(dataDir, ReposDir, "ns"), policy.Default()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.DataDir = dataDir
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.S3.Listen = "127.0.0.1:0"
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- Run(runCtx, cfg, quiet, func(Addrs) { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("server: %v", err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()

	if err := Run(ctx, cfg, quiet, nil); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("second server: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dataDir, AdminSocket)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("admin socket mode: %v %v", st.Mode(), err)
	}
	admin, ok := DialAdmin(dataDir)
	if !ok {
		t.Fatal("admin socket not reachable")
	}
	list := func() string {
		repos, err := admin.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(repos)
	}
	if l := list(); !strings.Contains(l, "ns") {
		t.Fatalf("repository placed in repos/ before start not registered: %s", l)
	}
	if err := admin.Do(ctx, "freeze", "ns"); err != nil {
		t.Fatal(err)
	}
	if l := list(); !strings.Contains(l, "true") {
		t.Fatalf("not frozen: %s", l)
	}
	if err := admin.Do(ctx, "unfreeze", "ns"); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "ns.zip")
	if _, err := admin.Archive(ctx, "ns", zipPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := admin.Do(ctx, "detach", "ns"); err != nil {
		t.Fatal(err)
	}
	if l := list(); strings.Contains(l, "ns") {
		t.Fatalf("detached repository still listed: %s", l)
	}
	fsckClean(t, filepath.Join(dataDir, ReposDir, "ns")) // detached: lock released
	if err := admin.Do(ctx, "attach", "ns"); err != nil {
		t.Fatal(err)
	}
	if err := admin.Do(ctx, "freeze", "missing"); err == nil || !strings.Contains(err.Error(), "no such repository") {
		t.Fatalf("freeze of a missing repository: %v", err)
	}
}

// A ZIP above 4 GiB (ZIP64) round-trips. Needs about 13 GB of disk space.
func TestZip64(t *testing.T) {
	if os.Getenv("F1_TEST_LARGE") == "" {
		t.Skip("set F1_TEST_LARGE=1 to run (writes about 13 GB)")
	}
	a := startServer(t, t.TempDir())
	defer a.stop(t)
	r := a.create(t, "big")
	const size = 4<<30 + 100<<20
	h := sha256.New()
	res, err := r.Ingest(ctx, io.TeeReader(io.LimitReader(testutil.Reader("zip64"), size), h), size)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))
	parent := hash.Zero
	if _, err := r.Commit(ctx, repo.CommitRequest{Ref: "main", Parent: &parent, Author: repo.Person{Name: "t"},
		Changes: []repo.Change{{Op: repo.OpPut, Path: "big.bin", Entry: object.Entry{Kind: object.KindFile, Target: res.File}}}}); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "big.zip")
	info, err := ArchiveToFile(ctx, zipPath, func(w io.Writer) (repo.ArchiveInfo, error) { return a.mgr.Archive(ctx, "big", w) })
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(zipPath); st.Size() <= 4<<30 {
		t.Fatalf("archive is only %d bytes", st.Size())
	}
	t.Logf("archive: %d packs, %d bytes", info.Packs, info.Bytes)
	b := startServer(t, t.TempDir())
	defer b.stop(t)
	if _, err := ImportRepo(ctx, b.reposDir(), zipPath, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.mgr.Attach(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	rb, _ := b.mgr.Get("big")
	if got := snapshot(t, rb, rb.Refs())["heads/main:big.bin"]; got != want {
		t.Fatal("content differs after the ZIP64 round trip")
	}
}
