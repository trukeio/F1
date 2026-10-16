package test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

// BenchmarkSite measures serving a web site's small files (TODO.md): plain
// HTTP GETs of 10 kB, 100 kB and 500 kB files from F1's native API and from
// Go's http.FileServer on the same file system, so that the difference is
// F1's own cost (authentication, ref and path lookup, record reads and
// decompression). It is not in make bench; set F1_BENCH_SITE=1 to run it
// (doc/benchmarks.md, "Web site files").
//
// Each size has a working set of siteFiles files, read in turn. The
// sources are:
//   - fileserver: http.FileServer over a directory;
//   - f1-branch: /api/v1/ns/site/files/main/<path>, revalidated on every use
//     (Cache-Control: no-cache, ADR-0016);
//   - f1-commit: the same files by commit hash, cacheable for a year;
//   - fileserver-304, f1-304: conditional GETs that a browser or proxy
//     sends to revalidate a cached copy (If-Modified-Since, If-None-Match).
//
// Each source runs serially (one client: latency) and with 24 concurrent
// clients (-parallel: throughput). "cold" reads every file once from a
// server that was just restarted, so F1's read cache is empty.
func BenchmarkSite(b *testing.B) {
	if os.Getenv("F1_BENCH_SITE") == "" {
		b.Skip("set F1_BENCH_SITE=1")
	}
	const siteFiles = 100
	sizes := []struct {
		name string
		n    int
	}{{"10kB", 10_000}, {"100kB", 100_000}, {"500kB", 500_000}}

	e := start(b, envOpts{})
	e.createNS(b, "site", "")
	dir := b.TempDir()
	parent := "new"
	for _, sz := range sizes {
		for i := range siteFiles {
			p := fmt.Sprintf("%s/f%03d.bin", sz.name, i)
			d := testutil.Bytes(p, sz.n)
			res, err := e.admin.Put(ctx, "site", "main", p, bytes.NewReader(d), int64(len(d)), &f1client.PutOptions{Parent: parent})
			if err != nil {
				b.Fatal(err)
			}
			parent = res.Commit
			os.MkdirAll(filepath.Join(dir, sz.name), 0o755)
			if err := os.WriteFile(filepath.Join(dir, p), d, 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	commit := parent
	fs := httptest.NewServer(http.FileServer(http.Dir(dir)))
	b.Cleanup(fs.Close)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	b.Cleanup(client.CloseIdleConnections)

	// Validators of each file, as a client keeps them with its cached copy.
	validators := map[string]string{} // URL → ETag or Last-Modified
	fetch := func(b *testing.B, url, token, cond string, want int, record bool) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cond != "" {
			req.Header.Set(cond, validators[url])
		}
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			b.Fatalf("GET %s: %s, %d bytes, %v", url, resp.Status, n, err)
		}
		if record {
			if v := resp.Header.Get("ETag"); v != "" && token != "" {
				validators[url] = v
			} else if v := resp.Header.Get("Last-Modified"); v != "" {
				validators[url] = v
			}
		}
	}

	type source struct {
		name, base, token, cond string
		want                    int
	}
	for _, sz := range sizes {
		f1branch := e.url + "/api/v1/ns/site/files/main/" + sz.name + "/"
		sources := []source{
			{"fileserver", fs.URL + "/" + sz.name + "/", "", "", http.StatusOK},
			{"f1-branch", f1branch, e.tokens["admin"], "", http.StatusOK},
			{"f1-commit", e.url + "/api/v1/ns/site/files/" + commit + "/" + sz.name + "/", e.tokens["admin"], "", http.StatusOK},
			{"fileserver-304", fs.URL + "/" + sz.name + "/", "", "If-Modified-Since", http.StatusNotModified},
			{"f1-304", f1branch, e.tokens["admin"], "If-None-Match", http.StatusNotModified},
		}
		urls := func(base string) []string {
			u := make([]string, siteFiles)
			for i := range u {
				u[i] = fmt.Sprintf("%sf%03d.bin", base, i)
			}
			return u
		}
		for _, src := range sources {
			u := urls(src.base)
			for _, url := range u { // warm up and record validators
				fetch(b, url, src.token, "", http.StatusOK, true)
			}
			bytesPerOp := int64(sz.n)
			if src.want == http.StatusNotModified {
				bytesPerOp = 0
			}
			b.Run(sz.name+"/"+src.name+"/serial", func(b *testing.B) {
				b.SetBytes(bytesPerOp)
				for i := range b.N {
					fetch(b, u[i%len(u)], src.token, src.cond, src.want, false)
				}
			})
			b.Run(sz.name+"/"+src.name+"/parallel", func(b *testing.B) {
				b.SetBytes(bytesPerOp)
				b.SetParallelism(4) // 24 clients on 6 cores
				var n atomic.Int64
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						fetch(b, u[int(n.Add(1))%len(u)], src.token, src.cond, src.want, false)
					}
				})
				b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
			})
		}
	}

	// Cold: a restarted server reads each file once. The file server has
	// no cache of its own; its files stay in the page cache (tmpfs).
	for _, sz := range sizes {
		b.Run(sz.name+"/f1-branch/cold", func(b *testing.B) {
			b.SetBytes(int64(sz.n))
			b.StopTimer()
			for i := 0; i < b.N; i += siteFiles {
				e.stop()
				e = start(b, envOpts{dataDir: e.dataDir})
				client.CloseIdleConnections()
				base := e.url + "/api/v1/ns/site/files/main/" + sz.name + "/"
				b.StartTimer()
				for j := 0; j < siteFiles && i+j < b.N; j++ {
					fetch(b, fmt.Sprintf("%sf%03d.bin", base, j), e.tokens["admin"], "", http.StatusOK, false)
				}
				b.StopTimer()
			}
		})
	}
}
