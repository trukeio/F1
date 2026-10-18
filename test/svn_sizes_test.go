package test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/testutil"
)

// Size transparency (D19) of the SVN importer (ADR-0019): a generated
// dump adds a file of every test size in r1 and changes each in r2 with
// an svndiff delta that inserts bytes in its middle. f1d checks every
// result against the dump's MD5, with bounded memory.
func TestSizeTransparencySvnImport(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	bin := f1dBinary(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "f1d.toml")
	os.WriteFile(cfg, []byte("[server]\ndata_dir = \"data\"\nlisten = \"127.0.0.1:0\"\n[server.s3]\nlisten = \"\"\n[log]\nlevel = \"warn\"\n"), 0o600)

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeSizesDump(pw, testSizes())) }()
	cmd := exec.Command(bin, "repo", "import-svn", "--config", cfg, "--create", "svn", "sizes", "-")
	cmd.Stdin = pr
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The child's rusage would include this process's peak from before
	// exec, so sample its own high-water mark while it runs.
	var rss int64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if v, ok := peakRSS(cmd.Process.Pid); ok {
				rss = max(rss, v)
			}
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	<-sampled
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "imported r1–r2") {
		t.Fatalf("import output:\n%s", out.String())
	}
	if rss > 0 {
		t.Logf("f1d peak RSS %d MiB", rss>>20)
		if rss > memoryBound {
			t.Errorf("f1d peak RSS %d MiB exceeds %d MiB", rss>>20, memoryBound>>20)
		}
	}

	ctx := context.Background()
	rp, _, err := repo.Open(ctx, filepath.Join(dir, "data", "repos", "sizes"), repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close(ctx)
	head := rp.Ref("heads/main")
	for _, n := range testSizes() {
		e, err := rp.Stat(ctx, head, fmt.Sprintf("%d.bin", n))
		if err != nil {
			t.Fatal(err)
		}
		if size, err := rp.FileSize(ctx, e.Target); err != nil || size != uint64(n)+uint64(len(inserted)) {
			t.Errorf("%d.bin: %d bytes, %v", n, size, err)
		}
	}
}

// inserted is what r2 inserts in the middle of every file.
const inserted = "inserted by revision 2"

// deltaWindow is the target size of the generated svndiff windows, as
// Subversion writes them.
const deltaWindow = 100 << 10

func writeSizesDump(w io.Writer, sizes []int64) error {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	revision := func(n int) {
		props := "K 7\nsvn:log\nV 5\nsizes\nK 8\nsvn:date\nV 27\n2026-10-18T10:00:00.000000Z\nPROPS-END\n"
		p("Revision-number: %d\nProp-content-length: %d\nContent-length: %d\n\n%s\n", n, len(props), len(props), props)
	}
	p("SVN-fs-dump-format-version: 3\n\nUUID: 00000000-0000-0000-0000-00000000d19d\n\n")
	revision(1)
	p("Node-path: trunk\nNode-kind: dir\nNode-action: add\nProp-content-length: 10\nContent-length: 10\n\nPROPS-END\n\n\n")
	for _, n := range sizes {
		seed := fmt.Sprint("svn size ", n)
		sum, err := md5Of(io.LimitReader(testutil.Reader(seed), n))
		if err != nil {
			return err
		}
		p("Node-path: trunk/%d.bin\nNode-kind: file\nNode-action: add\nText-content-length: %d\nText-content-md5: %s\nContent-length: %d\n\n", n, n, sum, n)
		if _, err := io.Copy(w, io.LimitReader(testutil.Reader(seed), n)); err != nil {
			return err
		}
		p("\n\n")
	}
	revision(2)
	for _, n := range sizes {
		seed := fmt.Sprint("svn size ", n)
		mid := n / 2
		sum, err := md5Of(io.MultiReader(io.LimitReader(testutil.Reader(seed), mid), strings.NewReader(inserted),
			io.LimitReader(readerFrom(seed, mid), n-mid)))
		if err != nil {
			return err
		}
		var count countWriter
		insertDelta(&count, n, mid)
		p("Node-path: trunk/%d.bin\nNode-kind: file\nNode-action: change\nText-delta: true\nText-content-length: %d\nText-content-md5: %s\nContent-length: %d\n\n", n, count, sum, count)
		if err := insertDelta(w, n, mid); err != nil {
			return err
		}
		p("\n\n")
	}
	return nil
}

// insertDelta writes an svndiff0 delta that copies a base of n bytes in
// windows and inserts the inserted text at mid.
func insertDelta(w io.Writer, n, mid int64) error {
	var b []byte
	b = append(b, "SVN\x00"...)
	copies := func(from, to int64) {
		for off := from; off < to; off += deltaWindow {
			l := min(deltaWindow, to-off)
			ins := append([]byte{0}, svnVarint(nil, uint64(l))...) // copy from the source view, length as a varint
			ins = svnVarint(ins, 0)
			b = svnWindow(b, uint64(off), uint64(l), uint64(l), ins, nil)
		}
	}
	copies(0, mid)
	b = svnWindow(b, 0, 0, uint64(len(inserted)), append([]byte{2 << 6}, svnVarint(nil, uint64(len(inserted)))...), []byte(inserted))
	if _, err := w.Write(b); err != nil {
		return err
	}
	b = b[:0]
	for off := mid; off < n; off += 64 * deltaWindow {
		copies(off, min(off+64*deltaWindow, n))
		if _, err := w.Write(b); err != nil {
			return err
		}
		b = b[:0]
	}
	return nil
}

func svnWindow(b []byte, sOff, sLen, tLen uint64, ins, nd []byte) []byte {
	for _, v := range []uint64{sOff, sLen, tLen, uint64(len(ins)), uint64(len(nd))} {
		b = svnVarint(b, v)
	}
	return append(append(b, ins...), nd...)
}

// svnVarint appends v as svndiff writes integers: seven bits a byte,
// most significant first, the high bit set on all but the last.
func svnVarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(v & 0x7f)
	for v >>= 7; v > 0; v >>= 7 {
		i--
		tmp[i] = byte(v&0x7f) | 0x80
	}
	return append(b, tmp[i:]...)
}

func md5Of(r io.Reader) (string, error) {
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readerFrom returns the Reader(seed) stream from offset off.
func readerFrom(seed string, off int64) io.Reader {
	r := testutil.Reader(seed)
	io.CopyN(io.Discard, r, off)
	return r
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}
