package test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

// The benchmarks of doc/benchmarks.md (phase 9): ingest, small Put rate,
// S3 GET latency, large reads and git clone, each against an in-process
// server on a temporary directory. make bench runs them; scripts/benchcmp
// compares a run with the recorded baseline.

// Native Put of 64 MiB files: ingest throughput (chunking, hashing,
// compression, digests, pack writes and the commit).
func BenchmarkIngest(b *testing.B) {
	e := start(b, envOpts{})
	e.createNS(b, "bench", "")
	const size = 64 << 20
	data := testutil.Bytes("ingest", size)
	b.SetBytes(size)
	parent := "new"
	b.ResetTimer()
	for i := range b.N {
		// Vary the content so that nothing deduplicates.
		copy(data, fmt.Sprintf("version %d", i))
		res, err := e.admin.Put(ctx, "bench", "main", "big.bin", bytes.NewReader(data), size, &f1client.PutOptions{Parent: parent})
		if err != nil {
			b.Fatal(err)
		}
		parent = res.Commit
	}
}

// Concurrent native Puts of 1 KiB objects into an autocommit namespace
// (group commit): the small write rate. The writes overwrite 1,000 keys, so
// that the namespace's size, and the cost of a commit, does not grow with
// the number of iterations.
func BenchmarkSmallPut(b *testing.B) {
	e := start(b, envOpts{})
	e.createNS(b, "small", "s3")
	var n atomic.Int64
	b.SetParallelism(4)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			d := testutil.Bytes(fmt.Sprint("small ", i), 1024)
			if _, err := e.admin.Put(ctx, "small", "main", fmt.Sprintf("k/%d", i%1000), bytes.NewReader(d), int64(len(d)), nil); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "puts/s")
}

// S3 GetObject of a 4 KiB object: request latency.
func BenchmarkS3GetLatency(b *testing.B) {
	e := start(b, envOpts{})
	e.createBucket(b, "lat")
	c := e.s3client("admin")
	d := testutil.Bytes("latency", 4096)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("lat"), Key: aws.String("k"), Body: bytes.NewReader(d)}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("lat"), Key: aws.String("k")})
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, out.Body)
		out.Body.Close()
	}
}

// Native Get of a 64 MiB file (about a thousand chunks): sequential read
// throughput.
func BenchmarkGetLarge(b *testing.B) {
	e := start(b, envOpts{})
	e.createNS(b, "read", "")
	const size = 64 << 20
	if _, err := e.admin.Put(ctx, "read", "main", "big.bin", io.LimitReader(testutil.Reader("read"), size), size, &f1client.PutOptions{Parent: "new"}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		rc, _, err := e.admin.Get(ctx, "read", "main", "big.bin", nil)
		if err != nil {
			b.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, rc); err != nil || n != size {
			b.Fatalf("read %d bytes: %v", n, err)
		}
		rc.Close()
	}
}

// git clone of a fixture with 300 commits over 400 source files (about
// 2 MiB per checkout, 25 MiB of file versions).
func BenchmarkGitClone(b *testing.B) {
	requireGit(b)
	src := b.TempDir()
	gitCmd(b, src, nil, "init", "-q", "-b", "main")
	files := make([]string, 400)
	for i := range files {
		var s strings.Builder
		for l := range 100 {
			fmt.Fprintf(&s, "func f%d_%d() int { return %d } // a line of source code\n", i, l, i*l)
		}
		files[i] = s.String()
	}
	for c := range 300 {
		for k := range 3 { // each commit edits three files
			i := (c*7 + k*131) % len(files)
			files[i] = strings.Replace(files[i], fmt.Sprintf("return %d }", (c+k)%100*i), fmt.Sprintf("return %d } // edited in %d", (c+k)%100*i, c), 1)
			p := filepath.Join(src, fmt.Sprintf("pkg%d", i%20), fmt.Sprintf("file%d.go", i))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(files[i]), 0o644)
		}
		if c == 0 {
			for i, f := range files {
				p := filepath.Join(src, fmt.Sprintf("pkg%d", i%20), fmt.Sprintf("file%d.go", i))
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte(f), 0o644)
			}
		}
		gitCmd(b, src, nil, "add", "-A")
		gitCmd(b, src, nil, "commit", "-q", "-m", fmt.Sprint("commit ", c))
	}
	e := start(b, envOpts{})
	e.gitNamespace(b, "clone")
	gitCmd(b, src, nil, "push", "-q", e.gitURL("admin", "clone"), "main")
	var packBytes int64
	b.ResetTimer()
	for range b.N {
		dst := filepath.Join(b.TempDir(), "c")
		gitCmd(b, "", nil, "clone", "-q", "--bare", e.gitURL("admin", "clone"), dst)
		b.StopTimer()
		packs, _ := filepath.Glob(filepath.Join(dst, "objects", "pack", "*.pack"))
		for _, p := range packs {
			if st, err := os.Stat(p); err == nil {
				packBytes = st.Size()
			}
		}
		os.RemoveAll(dst)
		b.StartTimer()
	}
	b.ReportMetric(float64(packBytes)/(1<<20), "pack-MiB")
}
