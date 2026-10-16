package test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/trukeio/f1/internal/client"
	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

// countingProxy forwards to the server, counts the response body bytes,
// and can claim that no object is missing (as if garbage collection
// removed objects between objects/missing and the commit).
type countingProxy struct {
	url        string
	bytes      atomic.Int64
	lieMissing atomic.Int32 // objects/missing answers to falsify
	lied       atomic.Int32
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func newCountingProxy(t *testing.T, target string) *countingProxy {
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Body = countingBody{resp.Body, &p.bytes}
		return nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objects/missing") && p.lieMissing.Load() > 0 {
			p.lieMissing.Add(-1)
			p.lied.Add(1)
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"missing":[]}`)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

// A sparse clone of one subdirectory downloads only that subtree's data
// (doc/plan.md, phase 7): 5 GB repository, 50 GB with F1_TEST_LARGE.
func TestClientSparseTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large repository")
	}
	total := int64(5 << 30)
	switch {
	case os.Getenv("F1_TEST_LARGE") != "":
		total = 50 << 30
	case raceEnabled:
		total = 200 << 20
	}
	const dirs, perDir = 10, 10
	fileSize := total / (dirs * perDir)
	e := start(t, envOpts{})
	e.createNS(t, "big", "")
	parent := "new"
	seed := func(d, f int) string { return fmt.Sprintf("sparse %d/%d", d, f) }
	for d := range dirs {
		for f := range perDir {
			res, err := e.admin.Put(ctx, "big", "main", fmt.Sprintf("d%d/f%d.bin", d, f),
				io.LimitReader(testutil.Reader(seed(d, f)), fileSize), fileSize, &f1client.PutOptions{Parent: parent})
			if err != nil {
				t.Fatal(err)
			}
			parent = res.Commit
		}
	}
	put(t, e.admin, "big", "top.txt", "top level file\n", parent)

	p := newCountingProxy(t, e.url)
	dir := filepath.Join(t.TempDir(), "wc")
	wc, err := client.Clone(ctx, p.url, "big", dir, "", []string{"d3"}, client.Options{Token: e.tokens["admin"]})
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	subtree := perDir * fileSize
	got := p.bytes.Load()
	t.Logf("repository %d MiB, subtree %d MiB, transferred %d MiB (%d bytes of metadata)", (dirs*subtree)>>20, subtree>>20, got>>20, got-subtree)
	if got < subtree || got > subtree+1<<20 {
		t.Fatalf("transferred %d bytes for a subtree of %d", got, subtree)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if strings.Join(names, ",") != ".f1,d3" {
		t.Fatalf("working copy has %v", names)
	}
	files, _ := os.ReadDir(filepath.Join(dir, "d3"))
	if len(files) != perDir {
		t.Fatalf("%d files in d3", len(files))
	}
	h := sha256.New()
	f, err := os.Open(filepath.Join(dir, "d3", "f7.bin"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(h, f)
	f.Close()
	w := sha256.New()
	io.Copy(w, io.LimitReader(testutil.Reader(seed(3, 7)), fileSize))
	if n != fileSize || !bytes.Equal(h.Sum(nil), w.Sum(nil)) {
		t.Fatal("d3/f7.bin content differs")
	}
	if st, err := wc.Status(ctx); err != nil || len(st) != 0 {
		t.Fatalf("status = %v, %v", st, err)
	}

	// Widening the checkout downloads the new subtree only.
	before := p.bytes.Load()
	if err := wc.SparseAdd(ctx, "d8"); err != nil {
		t.Fatal(err)
	}
	if got := p.bytes.Load() - before; got < subtree || got > subtree+1<<20 {
		t.Fatalf("sparse add transferred %d bytes for a subtree of %d", got, subtree)
	}
}

// Push re-uploads objects that the server reports missing at commit time
// (collected by GC after objects/missing said they were present).
func TestClientPushRetry(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "r", "")
	p := newCountingProxy(t, e.url)
	dir := filepath.Join(t.TempDir(), "wc")
	wc, err := client.Clone(ctx, p.url, "r", dir, "", nil, client.Options{Token: e.tokens["admin"]})
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	data := testutil.Bytes("retry", 3<<20)
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wc.Add(ctx, []string{name}); err != nil {
			t.Fatal(err)
		}
		if _, err := wc.Commit(ctx, "add "+name); err != nil {
			t.Fatal(err)
		}
	}
	commit("a.bin")
	p.lieMissing.Store(1)
	if r, err := wc.Push(ctx, true); err != nil || r.Pushed != 1 || p.lied.Load() != 1 {
		t.Fatalf("push = %+v, %v (lied %d times)", r, err, p.lied.Load())
	}
	if get(t, e.admin, "r", "main", "a.bin") != string(data) {
		t.Fatal("pushed content differs")
	}
	// The retries are bounded.
	data = testutil.Bytes("retry 2", 3<<20)
	commit("b.bin")
	p.lieMissing.Store(10)
	if _, err := wc.Push(ctx, true); f1client.Code(err) != f1client.CodeMissingObjects {
		t.Fatalf("push with objects always missing: %v", err)
	}
	p.lieMissing.Store(0)
	if r, err := wc.Push(ctx, true); err != nil || r.Pushed != 1 {
		t.Fatalf("push = %+v, %v", r, err)
	}
	if get(t, e.admin, "r", "main", "b.bin") != string(data) {
		t.Fatal("pushed content differs")
	}
}
