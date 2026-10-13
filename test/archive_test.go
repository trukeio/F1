package test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

// Online archive under load with native API writes: writes continue during
// GET /archive and none fails; the imported archive passes fsck and has
// exactly the refs recorded in the archive.
func TestArchiveUnderNativeLoad(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "bucket", "s3")
	e.setACL(t, "bucket", "user:alice", "", "read,write")
	e.setACL(t, "bucket", "user:admin", "", "admin")
	for i := range 20 {
		d := testutil.Bytes(fmt.Sprint("seed", i), 2000+i*9000)
		if _, err := e.alice.Put(ctx, "bucket", "main", fmt.Sprintf("seed/%d", i), bytes.NewReader(d), int64(len(d)), nil); err != nil {
			t.Fatal(err)
		}
	}

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
				d := testutil.Bytes(fmt.Sprint(w, i), 1000+i%5*40000)
				if _, err := e.alice.Put(ctx, "bucket", "main", fmt.Sprintf("load/%d/%d", w, i), bytes.NewReader(d), int64(len(d)), nil); err != nil {
					t.Error(err)
					failures.Add(1)
				}
				writes.Add(1)
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	var archive bytes.Buffer
	if err := e.admin.Archive(ctx, "bucket", &archive); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d of %d writes failed during the archive", failures.Load(), writes.Load())
	}
	t.Logf("%d writes during the test; archive %d bytes", writes.Load(), archive.Len())

	if err := e.alice.Archive(ctx, "bucket", io.Discard); f1client.Code(err) != f1client.CodeForbidden {
		t.Fatalf("archive by a non-admin: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var archivedRefs string
	for _, f := range zr.File {
		if f.Name == "bucket/refs" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			archivedRefs = string(b)
		}
	}
	if archivedRefs == "" {
		t.Fatal("archive has no refs")
	}
	zipPath := filepath.Join(t.TempDir(), "bucket.zip")
	if err := os.WriteFile(zipPath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	reposDir := filepath.Join(t.TempDir(), "repos")
	res, err := server.ImportRepo(ctx, reposDir, zipPath, server.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fsck.Clean() {
		t.Fatalf("fsck: %v", res.Fsck.Findings)
	}
	r, _, err := repo.Open(ctx, filepath.Join(reposDir, "bucket"), repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	var got strings.Builder
	for name, h := range r.Refs() {
		fmt.Fprintf(&got, "%s %s\n", h, name)
	}
	if got.String() != archivedRefs {
		t.Fatalf("imported refs %q differ from the archive's %q", got.String(), archivedRefs)
	}
}
