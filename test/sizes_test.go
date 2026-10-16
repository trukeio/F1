package test

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/client"
	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

var (
	binOnce sync.Once
	binDir  string
	binPath string
	binErr  error
)

// f1dBinary builds cmd/f1d once per test run.
func f1dBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		if binDir, binErr = os.MkdirTemp("", "f1d-test-bin-"); binErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "f1d")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/trukeio/f1/cmd/f1d").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	if f1Path != "" {
		os.RemoveAll(filepath.Dir(f1Path))
	}
	os.Exit(code)
}

// subprocessServer bootstraps a data directory with the f1d CLI and runs
// "f1d serve" in a subprocess, so that its peak memory can be measured.
type subprocessServer struct {
	cmd      *exec.Cmd
	url      string
	token    string
	s3url    string
	s3key    string
	s3secret string
}

func runCLI(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("f1d %v: %v: %s", args, err, stderr)
	}
	return string(out)
}

func startSubprocess(t *testing.T) *subprocessServer {
	t.Helper()
	bin := f1dBinary(t)
	dir := t.TempDir()
	addr, s3addr := freeAddr(t), freeAddr(t)
	cfg := filepath.Join(dir, "f1d.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\ndata_dir = \"data\"\nlisten = %q\n[server.s3]\nlisten = %q\n[log]\nlevel = \"warn\"\n", addr, s3addr)), 0o600)
	runCLI(t, bin, "user", "add", "--config", cfg, "admin")
	token := strings.TrimSpace(runCLI(t, bin, "token", "create", "--config", cfg, "admin"))
	var key, secret string
	for _, line := range strings.Split(runCLI(t, bin, "s3key", "create", "--config", cfg, "admin"), "\n") {
		if v, ok := strings.CutPrefix(line, "access_key = "); ok {
			key = v
		}
		if v, ok := strings.CutPrefix(line, "secret_key = "); ok {
			secret = v
		}
	}
	runCLI(t, bin, "acl", "set", "--config", cfg, "*", "user:admin", "", "admin")
	runCLI(t, bin, "ns", "create", "--config", cfg, "sizes")
	runCLI(t, bin, "ns", "create", "--config", cfg, "--preset", "git", "sizes-git")

	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &subprocessServer{cmd: cmd, url: "http://" + addr, token: token, s3url: "http://" + s3addr, s3key: key, s3secret: secret}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(s.url + "/healthz"); err == nil {
			resp.Body.Close()
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freeAddr returns a free local TCP address.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// peakRSS returns the peak resident set size of a process (Linux).
func peakRSS(pid int) (int64, bool) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmHWM:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb << 10, err == nil
		}
	}
	return 0, false
}

// memoryBound is the peak memory allowed to the server process and to the
// client (D19), independent of file size.
const memoryBound = 512 << 20

// Size transparency (D19): the same operations work the same way for
// every file size, and memory stays bounded. Native Put, Get, Range and
// Stat.
func TestSizeTransparencyNative(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	s := startSubprocess(t)
	c := f1client.New(s.url, s.token)
	clientPeak := sampleHeap(t)

	parent := "new"
	for _, n := range testSizes() {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			seed := fmt.Sprint("size ", n)
			path := fmt.Sprintf("sizes/%d.bin", n)
			h := sha256.New()
			res, err := c.Put(ctx, "sizes", "main", path, io.TeeReader(io.LimitReader(testutil.Reader(seed), n), h), n, &f1client.PutOptions{Parent: parent})
			if err != nil {
				t.Fatal(err)
			}
			parent = res.Commit
			want := hex.EncodeToString(h.Sum(nil))
			if res.Size != n || res.SHA256 != want {
				t.Fatalf("put: size %d sha256 %s, want %d %s", res.Size, res.SHA256, n, want)
			}
			st, err := c.Stat(ctx, "sizes", "main", path)
			if err != nil || st.Size != n {
				t.Fatalf("stat = %+v, %v", st, err)
			}
			rc, _, err := c.Get(ctx, "sizes", "main", path, nil)
			if err != nil {
				t.Fatal(err)
			}
			g := sha256.New()
			got, err := io.Copy(g, rc)
			rc.Close()
			if err != nil || got != n || hex.EncodeToString(g.Sum(nil)) != want {
				t.Fatalf("get: %d bytes, %v", got, err)
			}
			if n == 0 {
				return
			}
			for _, off := range []int64{0, n / 2, max(n-1000, 0)} {
				l := min(int64(1000), n-off)
				rc, _, err := c.Get(ctx, "sizes", "main", path, &f1client.GetOptions{Range: fmt.Sprintf("bytes=%d-%d", off, off+l-1)})
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || !bytes.Equal(b, testutil.ReadAt(seed, off, int(l))) {
					t.Fatalf("range at %d: %d bytes, %v", off, len(b), err)
				}
			}
		})
	}
	checkMemory(t, s, clientPeak)
}

// testSizes are the file sizes of the size transparency suite (D19).
// F1_TEST_SIZES (comma-separated byte counts) replaces them, for
// investigating one size.
func testSizes() []int64 {
	if v := os.Getenv("F1_TEST_SIZES"); v != "" {
		var sizes []int64
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
			if err == nil {
				sizes = append(sizes, n)
			}
		}
		return sizes
	}
	sizes := []int64{0, 1, 16<<10 - 1, 16 << 10, 256<<10 + 1, 64<<20 + 1}
	if os.Getenv("F1_TEST_LARGE") != "" {
		sizes = append(sizes, 1<<30, 10<<30)
	}
	return sizes
}

// sampleHeap records the test process's peak heap until the test ends.
// With F1_HEAP_PROFILE set to a file name, a heap profile is written there
// at every new peak above 64 MiB.
func sampleHeap(t *testing.T) *atomic.Uint64 {
	var peak atomic.Uint64
	done := make(chan struct{})
	profile := os.Getenv("F1_HEAP_PROFILE")
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				if profile != "" && ms.HeapInuse > 64<<20 && ms.HeapInuse > peak.Load()*11/10 {
					if f, err := os.Create(profile); err == nil {
						pprof.WriteHeapProfile(f)
						f.Close()
					}
				}
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { close(done) })
	return &peak
}

// checkMemory checks the server's peak RSS and the client's peak heap
// against the bound.
func checkMemory(t *testing.T, s *subprocessServer, clientPeak *atomic.Uint64) {
	t.Helper()
	if rss, ok := peakRSS(s.cmd.Process.Pid); ok {
		t.Logf("server peak RSS %d MiB, client peak heap %d MiB", rss>>20, clientPeak.Load()>>20)
		if rss > memoryBound {
			t.Errorf("server peak RSS %d MiB exceeds %d MiB", rss>>20, memoryBound>>20)
		}
	}
	if clientPeak.Load() > memoryBound {
		t.Errorf("client peak heap %d MiB exceeds %d MiB", clientPeak.Load()>>20, memoryBound>>20)
	}
}

// Size transparency (D19) over Git: files of every size written through the
// native API clone with the right blob ids, with bounded server memory.
func TestSizeTransparencyGit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	requireGit(t)
	s := startSubprocess(t)
	c := f1client.New(s.url, s.token)
	parent := "new"
	want := map[string]string{} // path → Git blob id
	for _, n := range testSizes() {
		seed := fmt.Sprint("git size ", n)
		path := fmt.Sprintf("sizes/%d.bin", n)
		h := sha1.New()
		h.Write([]byte(fmt.Sprintf("blob %d\x00", n)))
		res, err := c.Put(ctx, "sizes-git", "main", path, io.TeeReader(io.LimitReader(testutil.Reader(seed), n), h), n, &f1client.PutOptions{Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		parent = res.Commit
		want[path] = hex.EncodeToString(h.Sum(nil))
	}
	u, _ := url.Parse(s.url)
	u.User = url.UserPassword("admin", s.token)
	u.Path = "/git/sizes-git.git"
	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", nil, "clone", "-q", "--bare", u.String(), dst)
	for path, id := range want {
		if got := strings.TrimSpace(gitCmd(t, dst, nil, "rev-parse", "HEAD:"+path)); got != id {
			t.Fatalf("%s: blob %s, want %s", path, got, id)
		}
	}
	gitCmd(t, dst, nil, "fsck", "--no-dangling")
	checkMemory(t, s, sampleHeap(t))
}

// Size transparency (D19) over Git push and Git LFS: files of every size
// pushed by Git (as blobs, and as LFS objects when git-lfs is installed)
// arrive with the right content, with bounded server memory.
func TestSizeTransparencyGitPush(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	requireGit(t)
	_, lfsErr := exec.LookPath("git-lfs")
	s := startSubprocess(t)
	u, _ := url.Parse(s.url)
	u.User = url.UserPassword("admin", s.token)
	u.Path = "/git/sizes-git.git"
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", nil, "init", "-q", "-b", "main", work)
	if lfsErr == nil {
		gitCmd(t, work, nil, "lfs", "install", "--local")
		gitCmd(t, work, nil, "lfs", "track", "lfs/*")
		gitCmd(t, work, nil, "add", ".gitattributes")
	}
	want := map[string]string{} // path → SHA-256 of the content
	for _, n := range testSizes() {
		dirs := []string{"git"}
		if lfsErr == nil {
			dirs = append(dirs, "lfs")
		}
		for _, d := range dirs {
			path := fmt.Sprintf("%s/%d.bin", d, n)
			os.MkdirAll(filepath.Join(work, d), 0o755)
			f, err := os.Create(filepath.Join(work, path))
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(testutil.Reader(fmt.Sprint("push ", d, n)), n)); err != nil {
				t.Fatal(err)
			}
			f.Close()
			want[path] = hex.EncodeToString(h.Sum(nil))
		}
	}
	gitCmd(t, work, nil, "add", "-A")
	gitCmd(t, work, nil, "commit", "-q", "-m", "sizes")
	gitCmd(t, work, nil, "push", "-q", u.String(), "main")
	c := f1client.New(s.url, s.token)
	for path, sum := range want {
		rc, _, err := c.Get(ctx, "sizes-git", "main", path, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil || hex.EncodeToString(h.Sum(nil)) != sum {
			t.Fatalf("%s: content differs (%v)", path, err)
		}
	}
	if lfsErr != nil {
		t.Log("git-lfs is not installed; LFS sizes skipped")
	}
	checkMemory(t, s, sampleHeap(t))
}

// sizeLine appends line i (64 bytes) of the client size test's text, with
// the tag of an edit if there is one, without allocating: the generator
// runs in the process whose heap the test measures.
func sizeLine(b []byte, i int64, edits map[int64]string) []byte {
	tag := "base"
	if e, ok := edits[i]; ok {
		tag = e
	}
	start := len(b)
	b = append(b, tag...)
	for len(b)-start < 7 {
		b = append(b, ' ')
	}
	var num [12]byte
	d := strconv.AppendInt(num[:0], i, 10)
	for range 12 - len(d) {
		b = append(b, '0')
	}
	b = append(b, d...)
	b = append(b, ' ')
	for range 43 {
		b = append(b, 'x')
	}
	return append(b, '\n')
}

// lineGen generates the client size test's text.
type lineGen struct {
	i     int64
	buf   []byte
	edits map[int64]string
}

func (g *lineGen) Read(p []byte) (int, error) {
	for len(g.buf) < len(p) {
		g.buf = sizeLine(g.buf, g.i, g.edits)
		g.i++
	}
	n := copy(p, g.buf)
	g.buf = append(g.buf[:0], g.buf[n:]...)
	return n, nil
}

func sizeText(n int64, edits map[int64]string) io.Reader {
	return io.LimitReader(&lineGen{edits: edits}, n)
}

func sha256Of(t *testing.T, r io.Reader) string {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256File(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return sha256Of(t, f)
}

// Size transparency of the native client: checkout, commit, push, pull,
// and a three-way text merge of concurrent edits (rebased local commit),
// for every size.
func TestSizeTransparencyClient(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	s := startSubprocess(t)
	admin := f1client.New(s.url, s.token)
	if _, err := admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "sizes-client", Preset: "svn"}); err != nil {
		t.Fatal(err)
	}
	clientPeak := sampleHeap(t)
	root := t.TempDir()
	opts := client.Options{Token: s.token}
	wc1, err := client.Clone(ctx, s.url, "sizes-client", filepath.Join(root, "1"), "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer wc1.Close()
	wc2, err := client.Clone(ctx, s.url, "sizes-client", filepath.Join(root, "2"), "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer wc2.Close()
	write := func(wc *client.WC, path string, r io.Reader) {
		t.Helper()
		f, err := os.Create(filepath.Join(wc.Root, path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, r); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	commitPush := func(wc *client.WC, msg string) {
		t.Helper()
		if _, err := wc.Commit(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if _, err := wc.Push(ctx); err != nil {
			t.Fatal(err)
		}
	}
	serverSHA := func(path string) string {
		t.Helper()
		rc, _, err := admin.Get(ctx, "sizes-client", "main", path, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		return sha256Of(t, rc)
	}
	for _, n := range testSizes() {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			path := fmt.Sprintf("%d.txt", n)
			write(wc1, path, sizeText(n, nil))
			if err := wc1.Add(ctx, []string{path}); err != nil {
				t.Fatal(err)
			}
			commitPush(wc1, "add "+path)
			want := sha256Of(t, sizeText(n, nil))
			if got := serverSHA(path); got != want {
				t.Fatal("pushed content differs")
			}
			if _, err := wc2.Pull(ctx); err != nil {
				t.Fatal(err)
			}
			if got := sha256File(t, filepath.Join(wc2.Root, path)); got != want {
				t.Fatal("checked out content differs")
			}
			lines := n / 64
			if lines < 20 {
				return
			}
			// Concurrent edits far apart: wc1 pushes first, wc2's
			// commit is rebased with a three-way merge.
			k1, k2 := lines/10, lines*9/10
			write(wc1, path, sizeText(n, map[int64]string{k1: "mine"}))
			commitPush(wc1, "edit 1")
			write(wc2, path, sizeText(n, map[int64]string{k2: "theirs"}))
			if _, err := wc2.Commit(ctx, "edit 2"); err != nil {
				t.Fatal(err)
			}
			if _, err := wc2.Push(ctx); err != client.ErrRemoteChanged {
				t.Fatalf("push before pull: %v", err)
			}
			res, err := wc2.Pull(ctx)
			if err != nil || res.Rebased != 1 || len(res.Conflicts) != 0 {
				t.Fatalf("pull = %+v, %v", res, err)
			}
			if _, err := wc2.Push(ctx); err != nil {
				t.Fatal(err)
			}
			merged := sha256Of(t, sizeText(n, map[int64]string{k1: "mine", k2: "theirs"}))
			if got := serverSHA(path); got != merged {
				t.Fatal("merged content differs")
			}
			if _, err := wc1.Pull(ctx); err != nil {
				t.Fatal(err)
			}
			if got := sha256File(t, filepath.Join(wc1.Root, path)); got != merged {
				t.Fatal("pulled merge differs")
			}
		})
	}
	checkMemory(t, s, clientPeak)
}
