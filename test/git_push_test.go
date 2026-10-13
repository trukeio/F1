package test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/trukeio/f1/pkg/f1client"
)

func (e *env) gitNamespace(t *testing.T, name string) {
	t.Helper()
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: name, Preset: "git"}); err != nil {
		t.Fatal(err)
	}
}

// Every fixture repository pushed to an empty namespace clones back with
// identical ids.
func TestGitPushFixture(t *testing.T) {
	requireGit(t)
	src := makeFixture(t)
	e := start(t, envOpts{})
	e.gitNamespace(t, "pushed")
	gitCmd(t, src, nil, "push", "-q", "--mirror", e.gitURL("admin", "pushed"))
	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", nil, "clone", "-q", "--mirror", e.gitURL("admin", "pushed"), dst)
	want := gitRefs(t, src, "refs/heads") + gitRefs(t, src, "refs/tags")
	if got := gitRefs(t, dst, "refs/heads") + gitRefs(t, dst, "refs/tags"); got != want {
		t.Fatalf("refs differ:\n%s\nwant:\n%s", got, want)
	}
	if out, err := gitRun(dst, nil, "fsck", "--full", "--no-dangling"); err != nil {
		t.Fatalf("git fsck: %v\n%s", err, out)
	}
	// Incremental pushes, a branch deletion and a tag deletion.
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "pushed"), work)
	os.WriteFile(filepath.Join(work, "new.txt"), []byte("new\n"), 0o644)
	gitCmd(t, work, nil, "add", "new.txt")
	gitCmd(t, work, nil, "commit", "-q", "-m", "new")
	gitCmd(t, work, nil, "push", "-q", "origin", "main")
	gitCmd(t, work, nil, "push", "-q", "origin", ":refs/heads/feature", ":refs/tags/light")
	out := gitCmd(t, "", nil, "ls-remote", e.gitURL("admin", "pushed"))
	if strings.Contains(out, "refs/heads/feature") || strings.Contains(out, "refs/tags/light") ||
		!strings.Contains(out, strings.TrimSpace(gitCmd(t, work, nil, "rev-parse", "HEAD"))+"\trefs/heads/main") {
		t.Fatalf("after pushes:\n%s", out)
	}
	// A stale push is rejected (non-fast-forward).
	stale := filepath.Join(t.TempDir(), "stale")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "pushed"), stale)
	os.WriteFile(filepath.Join(work, "more.txt"), []byte("more\n"), 0o644)
	gitCmd(t, work, nil, "add", "more.txt")
	gitCmd(t, work, nil, "commit", "-q", "-m", "more")
	gitCmd(t, work, nil, "push", "-q", "origin", "main")
	os.WriteFile(filepath.Join(stale, "other.txt"), []byte("other\n"), 0o644)
	gitCmd(t, stale, nil, "add", "other.txt")
	gitCmd(t, stale, nil, "commit", "-q", "-m", "other")
	if out, err := gitRun(stale, nil, "push", "origin", "main"); err == nil || !strings.Contains(out, "rejected") {
		t.Fatalf("stale push: %v\n%s", err, out)
	}
	// Atomic pushes apply all refs or none.
	gitCmd(t, stale, nil, "branch", "side")
	if out, err := gitRun(stale, nil, "push", "--atomic", "origin", "main", "side"); err == nil {
		t.Fatalf("atomic push with a stale ref succeeded:\n%s", out)
	}
	if strings.Contains(gitCmd(t, "", nil, "ls-remote", e.gitURL("admin", "pushed")), "refs/heads/side") {
		t.Fatal("atomic push applied part of the refs")
	}
}

// Non-canonical trees round-trip; trees with duplicate entries are
// rejected with a readable message.
func TestGitPushOddTrees(t *testing.T) {
	requireGit(t)
	e := start(t, envOpts{})
	e.gitNamespace(t, "odd")
	src := t.TempDir()
	g := func(args ...string) string { return strings.TrimSpace(gitCmd(t, src, nil, args...)) }
	g("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "f"), []byte("f\n"), 0o644)
	g("add", "f")
	g("commit", "-q", "-m", "base")
	blob, _ := hex.DecodeString(g("rev-parse", "HEAD:f"))
	sub := hashObject(t, src, "tree", append([]byte("100644 x\x00"), blob...))
	subID, _ := hex.DecodeString(sub)
	entry := func(mode, name string, id []byte) []byte { return append([]byte(mode+" "+name+"\x00"), id...) }
	cases := map[string][]byte{
		"mode-664":    entry("100664", "a", blob),
		"zero-padded": entry("040000", "d", subID),
		"unsorted":    append(entry("100644", "b", blob), entry("100644", "a", blob)...),
	}
	for name, tree := range cases {
		id := hashObject(t, src, "tree", tree)
		g("update-ref", "refs/heads/"+name, g("commit-tree", id, "-m", name))
	}
	gitCmd(t, src, nil, "push", "-q", e.gitURL("admin", "odd"), "refs/heads/*:refs/heads/*")
	want := gitRefs(t, src, "refs/heads")
	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", nil, "clone", "-q", "--mirror", e.gitURL("admin", "odd"), dst)
	if got := gitRefs(t, dst, "refs/heads"); got != want {
		t.Fatalf("refs differ:\n%s\nwant:\n%s", got, want)
	}
	for name := range cases {
		ref := "refs/heads/" + name
		if a, b := gitCmd(t, src, nil, "rev-parse", ref+"^{tree}"), gitCmd(t, dst, nil, "rev-parse", ref+"^{tree}"); a != b {
			t.Fatalf("%s: tree %s, want %s", name, b, a)
		}
	}
	// Duplicate names are refused.
	dup := hashObject(t, src, "tree", append(entry("100644", "a", blob), entry("100644", "a", blob)...))
	g("update-ref", "refs/heads/dup", g("commit-tree", dup, "-m", "dup"))
	out, err := gitRun(src, nil, "push", e.gitURL("admin", "odd"), "refs/heads/dup")
	if err == nil || !strings.Contains(out, "duplicate name") {
		t.Fatalf("duplicate entries: %v\n%s", err, out)
	}
}

// A push keeps the S3 attributes of files it does not change, and is
// refused when it changes a path locked by someone else.
func TestGitPushS3AttrsAndLocks(t *testing.T) {
	requireGit(t)
	e := start(t, envOpts{})
	e.gitNamespace(t, "mixed")
	c := e.s3client("admin")
	for _, k := range []string{"keep.txt", "change.txt", "locked.txt"} {
		s3put(t, c, "mixed", k, k+"\n", func(in *s3.PutObjectInput) {
			in.ContentType = aws.String("text/x-" + k)
			in.Metadata = map[string]string{"origin": "s3"}
		})
	}
	before, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("mixed"), Key: aws.String("keep.txt")})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // LastModified has second resolution
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "mixed"), work)
	os.WriteFile(filepath.Join(work, "change.txt"), []byte("changed by git\n"), 0o644)
	gitCmd(t, work, nil, "commit", "-q", "-am", "change one file")
	gitCmd(t, work, nil, "push", "-q", "origin", "main")
	after, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("mixed"), Key: aws.String("keep.txt")})
	if err != nil || aws.ToString(after.ContentType) != "text/x-keep.txt" || after.Metadata["origin"] != "s3" ||
		aws.ToString(after.ETag) != aws.ToString(before.ETag) || !aws.ToTime(after.LastModified).Equal(aws.ToTime(before.LastModified)) {
		t.Fatalf("unchanged file lost its S3 attributes: %+v, %v", after, err)
	}
	changed, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("mixed"), Key: aws.String("change.txt")})
	if err != nil || changed.Metadata["origin"] != "" || s3get(t, c, "mixed", "change.txt") != "changed by git\n" {
		t.Fatalf("changed file: %+v, %v", changed, err)
	}
	// Locks: alice locks a path; admin's push that changes it is refused.
	e.setACL(t, "mixed", "user:alice", "", "read,write,lock")
	if _, err := e.alice.Lock(ctx, "mixed", "main", "locked.txt", 0); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "locked.txt"), []byte("changed\n"), 0o644)
	gitCmd(t, work, nil, "commit", "-q", "-am", "touch locked file")
	out, err := gitRun(work, nil, "push", "origin", "main")
	if err == nil || !strings.Contains(out, "locked.txt is locked by user:alice") {
		t.Fatalf("push of a locked path: %v\n%s", err, out)
	}
}

// Concurrent pushes and S3 writes to one namespace lose no update; the
// losers get a clear rejection and retry.
func TestGitPushConcurrentS3(t *testing.T) {
	requireGit(t)
	e := start(t, envOpts{})
	e.gitNamespace(t, "busy")
	c := e.s3client("admin")
	s3put(t, c, "busy", "seed", "seed")
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", nil, "clone", "-q", e.gitURL("admin", "busy"), work)

	stop := make(chan struct{})
	var s3writes atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		// A steady writer (one commit per write) while Git users push.
		for i := 0; i < 400; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("busy"), Key: aws.String(fmt.Sprintf("s3/%d", i)),
				Body: strings.NewReader("x")}); err != nil {
				t.Error(err)
				return
			}
			s3writes.Add(1)
		}
	})
	rejected := 0
	const pushes = 6
	for i := range pushes {
		name := fmt.Sprintf("git/%d.txt", i)
		os.MkdirAll(filepath.Join(work, "git"), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(name), 0o644)
		gitCmd(t, work, nil, "add", name)
		gitCmd(t, work, nil, "commit", "-q", "-m", name)
		for attempt := 0; ; attempt++ {
			out, err := gitRun(work, nil, "push", "origin", "main")
			if err == nil {
				break
			}
			if !strings.Contains(out, "rejected") || attempt > 500 {
				t.Fatalf("push: %v\n%s", err, out)
			}
			rejected++
			gitCmd(t, work, nil, "pull", "-q", "--rebase")
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d S3 writes, %d rejected pushes", s3writes.Load(), rejected)
	gitCmd(t, work, nil, "pull", "-q", "--rebase")
	for i := range pushes {
		if _, err := os.Stat(filepath.Join(work, fmt.Sprintf("git/%d.txt", i))); err != nil {
			t.Fatalf("git file %d lost", i)
		}
	}
	for i := range int(s3writes.Load()) {
		if _, err := os.Stat(filepath.Join(work, fmt.Sprintf("s3/%d", i))); err != nil {
			t.Fatalf("S3 object %d lost", i)
		}
	}
}

// A large file written through S3 is a normal blob in Git; a partial
// clone with a blob size limit skips it until checkout.
func TestGitLargeS3File(t *testing.T) {
	requireGit(t)
	if testing.Short() {
		t.Skip("large transfer")
	}
	size := int64(512 << 20)
	switch {
	case os.Getenv("F1_TEST_LARGE") != "":
		size = 5 << 30
	case raceEnabled:
		size = 64 << 20
	}
	e := start(t, envOpts{})
	e.gitNamespace(t, "big")
	c := e.s3client("admin")
	g := newGen("large s3 file", size)
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", size)
	io.Copy(h, io.NewSectionReader(g, 0, size))
	wantBlob := hex.EncodeToString(h.Sum(nil))
	// Multipart, as S3 tools upload large files.
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("data/big.bin")})
	if err != nil {
		t.Fatal(err)
	}
	done := uploadParts(t, c, "big", "data/big.bin", aws.ToString(cu.UploadId), sectionReaders(g, size, 64<<20), "")
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("data/big.bin"),
		UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatal(err)
	}
	s3put(t, c, "big", "small.txt", "small\n")

	dst := filepath.Join(t.TempDir(), "partial")
	gitCmd(t, "", nil, "clone", "-q", "--filter=blob:limit=1m", "--no-checkout", e.gitURL("admin", "big"), dst)
	if got := strings.TrimSpace(gitCmd(t, dst, nil, "rev-parse", "HEAD:data/big.bin")); got != wantBlob {
		t.Fatalf("blob id %s, want %s", got, wantBlob)
	}
	if out := gitCmd(t, dst, nil, "rev-list", "--objects", "--missing=print", "HEAD"); !strings.Contains(out, "?"+wantBlob) {
		t.Fatal("the large blob was sent despite the size filter")
	}
	gitCmd(t, dst, nil, "checkout", "-q", "main")
	f, err := os.Open(filepath.Join(dst, "data", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h2 := sha1.New()
	fmt.Fprintf(h2, "blob %d\x00", size)
	if n, err := io.Copy(h2, f); err != nil || n != size || hex.EncodeToString(h2.Sum(nil)) != wantBlob {
		t.Fatalf("checked-out file: %d bytes, %v", n, err)
	}
}

// sectionReaders splits g into readers of partSize bytes.
func sectionReaders(g *genReader, size, partSize int64) []*genReader {
	var out []*genReader
	for off := int64(0); off < size; off += partSize {
		n := min(partSize, size-off)
		out = append(out, &genReader{seed: g.seed, size: n, base: off})
	}
	return out
}

// Git LFS: push uploads objects through the batch API, they read back
// through Git LFS, the native API and S3 as real content, an S3 overwrite
// is seen by git lfs pull, and git lfs locks work.
func TestGitLFS(t *testing.T) {
	requireGit(t)
	if _, err := exec.LookPath("git-lfs"); err != nil {
		if os.Getenv("F1_REQUIRE_S3TOOLS") != "" {
			t.Fatal("git-lfs is not installed")
		}
		t.Skip("git-lfs is not installed")
	}
	e := start(t, envOpts{})
	e.gitNamespace(t, "lfs")
	u := e.gitURL("admin", "lfs")
	work := filepath.Join(t.TempDir(), "work")
	g := func(dir string, args ...string) string { return strings.TrimSpace(gitCmd(t, dir, nil, args...)) }
	g("", "init", "-q", "-b", "main", work)
	g(work, "lfs", "install", "--local")
	g(work, "config", "lfs.locksverify", "true")
	g(work, "lfs", "track", "*.bin")
	content := bytes.Repeat([]byte("large binary "), 100000)
	os.WriteFile(filepath.Join(work, "asset.bin"), content, 0o644)
	os.WriteFile(filepath.Join(work, "README"), []byte("readme\n"), 0o644)
	g(work, "add", ".gitattributes", "asset.bin", "README")
	g(work, "commit", "-q", "-m", "lfs file")
	g(work, "remote", "add", "origin", u)
	g(work, "push", "-q", "-u", "origin", "main")

	// Real content through the native API and S3.
	if got := get(t, e.admin, "lfs", "main", "asset.bin"); got != string(content) {
		t.Fatalf("native read of the LFS file: %d bytes", len(got))
	}
	c := e.s3client("admin")
	if got := s3get(t, c, "lfs", "asset.bin"); got != string(content) {
		t.Fatalf("S3 read of the LFS file: %d bytes", len(got))
	}
	// A clone gets the pointer through Git and the content through LFS.
	clone := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, "", []string{"GIT_LFS_SKIP_SMUDGE=1"}, "clone", "-q", u, clone)
	g(clone, "lfs", "install", "--local")
	g(clone, "lfs", "pull")
	if b, err := os.ReadFile(filepath.Join(clone, "asset.bin")); err != nil || !bytes.Equal(b, content) {
		t.Fatalf("git lfs pull: %d bytes, %v", len(b), err)
	}
	// An S3 overwrite keeps the file in LFS with the new content.
	s3put(t, c, "lfs", "asset.bin", "new content from S3\n")
	g(clone, "pull", "-q")
	g(clone, "lfs", "pull")
	if b, err := os.ReadFile(filepath.Join(clone, "asset.bin")); err != nil || string(b) != "new content from S3\n" {
		t.Fatalf("after the S3 overwrite: %q, %v", b, err)
	}
	if ptr := g(clone, "cat-file", "-p", "HEAD:asset.bin"); !strings.HasPrefix(ptr, "version https://git-lfs.github.com/spec/v1") {
		t.Fatalf("the file is no longer an LFS pointer in Git: %q", ptr)
	}
	// A push referencing an LFS object that was never uploaded is refused.
	g(work, "pull", "-q", "--rebase")
	os.WriteFile(filepath.Join(work, "asset.bin"), []byte("never uploaded\n"), 0o644)
	g(work, "commit", "-q", "-am", "missing lfs object")
	if out, err := gitRun(work, []string{"GIT_LFS_SKIP_PUSH=1"}, "push", "origin", "main"); err == nil || !strings.Contains(out, "git lfs push") {
		t.Fatalf("push without the LFS object: %v\n%s", err, out)
	}
	g(work, "push", "-q", "origin", "main") // the pre-push hook uploads it

	// Locks.
	g(clone, "pull", "-q")
	out := g(clone, "lfs", "lock", "asset.bin")
	if !strings.Contains(out, "Locked asset.bin") {
		t.Fatalf("git lfs lock: %s", out)
	}
	if out := g(clone, "lfs", "locks"); !strings.Contains(out, "asset.bin") {
		t.Fatalf("git lfs locks: %s", out)
	}
	locks, err := e.admin.Locks(ctx, "lfs", "main", "")
	if err != nil || len(locks) != 1 || locks[0].Path != "asset.bin" {
		t.Fatalf("F1 locks after git lfs lock: %+v, %v", locks, err)
	}
	g(clone, "lfs", "unlock", "asset.bin")
	if out := g(clone, "lfs", "locks"); strings.Contains(out, "asset.bin") {
		t.Fatalf("lock remains after unlock: %s", out)
	}
}
