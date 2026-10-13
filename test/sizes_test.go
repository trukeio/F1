package test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	os.Exit(code)
}

// subprocessServer bootstraps a data directory with the f1d CLI and runs
// "f1d serve" in a subprocess, so that its peak memory can be measured.
type subprocessServer struct {
	cmd   *exec.Cmd
	url   string
	token string
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg := filepath.Join(dir, "f1d.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\ndata_dir = \"data\"\nlisten = %q\n[log]\nlevel = \"warn\"\n", addr)), 0o600)
	runCLI(t, bin, "user", "add", "--config", cfg, "admin")
	token := strings.TrimSpace(runCLI(t, bin, "token", "create", "--config", cfg, "admin"))
	runCLI(t, bin, "acl", "set", "--config", cfg, "*", "user:admin", "", "admin")
	runCLI(t, bin, "ns", "create", "--config", cfg, "sizes")

	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &subprocessServer{cmd: cmd, url: "http://" + addr, token: token}
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
	sizes := []int64{0, 1, 16<<10 - 1, 16 << 10, 256<<10 + 1, 64<<20 + 1}
	if os.Getenv("F1_TEST_LARGE") != "" {
		sizes = append(sizes, 1<<30, 10<<30)
	}
	s := startSubprocess(t)
	c := f1client.New(s.url, s.token)

	// Sample the client's heap during the transfers.
	var clientPeak atomic.Uint64
	done := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > clientPeak.Load() {
				clientPeak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	defer close(done)

	parent := "new"
	for _, n := range sizes {
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
