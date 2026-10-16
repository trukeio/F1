package test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/server"
)

// f1d backup through a running server is incremental and consistent: a
// second run copies only new packs and removes packs that GC deleted; the
// backup passes fsck, and a server started on a copy of it serves the
// latest data.
func TestBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	bin := f1dBinary(t)
	e := start(t, envOpts{})
	e.createNS(t, "docs", "")
	e.createBucket(t, "objects") // retention = none
	r := put(t, e.admin, "docs", "a.txt", "first", "new")
	for i := range 5 {
		s3put(t, e.s3client("admin"), "objects", "k", fmt.Sprint("version ", i))
	}
	cfg := filepath.Join(t.TempDir(), "f1d.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\ndata_dir = %q\n", e.dataDir)), 0o600)
	dst := filepath.Join(t.TempDir(), "backup")
	out := runCLI(t, bin, "backup", "--config", cfg, dst)
	if !strings.Contains(out, "backup of 2 repositories") {
		t.Fatalf("first backup: %s", out)
	}
	put(t, e.admin, "docs", "b.txt", "second", r.Commit)
	s3put(t, e.s3client("admin"), "objects", "k", "latest")
	runCLI(t, bin, "gc", "--config", cfg, "--grace", "0", "objects")
	out = runCLI(t, bin, "backup", "--config", cfg, dst)
	docs := regexp.MustCompile(`docs: (\d+) packs, (\d+) copied`).FindStringSubmatch(out)
	objs := regexp.MustCompile(`objects: \d+ packs, \d+ copied \(\d+ bytes\), (\d+) removed`).FindStringSubmatch(out)
	if docs == nil || docs[2] == docs[1] || objs == nil || objs[1] == "0" {
		t.Fatalf("second backup is not incremental:\n%s", out)
	}
	for _, ns := range []string{"docs", "objects"} {
		if _, err := os.Stat(filepath.Join(dst, server.ReposDir, ns, "cache.db")); err == nil {
			t.Fatal("cache.db backed up")
		}
		if out := runCLI(t, bin, "fsck", "--dir", filepath.Join(dst, server.ReposDir, ns)); !strings.Contains(out, "fsck: ok") {
			t.Fatalf("fsck of backup %s: %s", ns, out)
		}
	}
	e.stop()

	// Restore: the backup is a data directory without cache.db.
	restored := filepath.Join(t.TempDir(), "data")
	if out, err := exec.Command("cp", "-a", dst, restored).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	e2 := start(t, envOpts{dataDir: restored})
	if got := get(t, e2.admin, "docs", "main", "b.txt"); got != "second" {
		t.Fatalf("restored b.txt = %q", got)
	}
	if got := s3get(t, e2.s3client("admin"), "objects", "k"); got != "latest" {
		t.Fatalf("restored object = %q", got)
	}
}
