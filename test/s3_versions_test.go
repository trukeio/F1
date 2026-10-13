package test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/trukeio/f1/internal/testutil"
	"github.com/trukeio/f1/pkg/f1client"
)

// Versioning maps to history: a namespace that keeps history is a
// versioned bucket, version ids are commits, deletions are delete markers.
func TestS3Versioning(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "hist", Config: &f1client.NamespaceConfig{
		Layout: "flat", Retention: "all", Locks: "off", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	ver, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("hist")})
	if err != nil || ver.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("GetBucketVersioning = %+v, %v", ver, err)
	}
	var ids []string
	for i := range 3 {
		out := s3put(t, c, "hist", "doc", fmt.Sprint("version ", i))
		if out.VersionId == nil {
			t.Fatal("no version id")
		}
		ids = append(ids, aws.ToString(out.VersionId))
	}
	s3put(t, c, "hist", "other", "x")
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc")})
	if err != nil || !aws.ToBool(del.DeleteMarker) || del.VersionId == nil {
		t.Fatalf("DeleteObject = %+v, %v", del, err)
	}
	marker := aws.ToString(del.VersionId)

	// Old versions are readable by id; the current state has no "doc".
	for i, id := range ids {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: aws.String(id)})
		if err != nil {
			t.Fatalf("version %d: %v", i, err)
		}
		b, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if string(b) != fmt.Sprint("version ", i) || aws.ToString(out.VersionId) != id {
			t.Fatalf("version %d = %q", i, b)
		}
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc")})
	wantS3Err(t, err, "NoSuchKey")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: aws.String(marker)})
	if s3status(err) != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a delete marker: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: aws.String(strings.Repeat("A", 52))})
	wantS3Err(t, err, "NoSuchVersion")

	// ListObjectVersions: newest first per key, one delete marker.
	lv, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("hist")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range lv.Versions {
		got = append(got, fmt.Sprintf("%s@%s latest=%v", aws.ToString(v.Key), aws.ToString(v.VersionId), aws.ToBool(v.IsLatest)))
	}
	want := []string{
		"doc@" + ids[2] + " latest=false", "doc@" + ids[1] + " latest=false", "doc@" + ids[0] + " latest=false",
		"other@" + aws.ToString(lv.Versions[3].VersionId) + " latest=true",
	}
	if !slices.Equal(got, want) || len(lv.DeleteMarkers) != 1 || aws.ToString(lv.DeleteMarkers[0].VersionId) != marker || !aws.ToBool(lv.DeleteMarkers[0].IsLatest) {
		t.Fatalf("versions %q, markers %+v", got, lv.DeleteMarkers)
	}
	// Paging through versions with max-keys 1 sees all five entries.
	n := 0
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String("hist"), MaxKeys: aws.Int32(1)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n += len(page.Versions) + len(page.DeleteMarkers)
	}
	if n != 5 {
		t.Fatalf("paged versions: %d entries", n)
	}

	// Deleting the delete marker restores the object; deleting the
	// current version restores the previous one; older versions stay.
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: aws.String(marker)}); err != nil {
		t.Fatal(err)
	}
	if got := s3get(t, c, "hist", "doc"); got != "version 2" {
		t.Fatalf("after removing the delete marker: %q", got)
	}
	cur := s3put(t, c, "hist", "doc", "version 3")
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: cur.VersionId}); err != nil {
		t.Fatal(err)
	}
	if got := s3get(t, c, "hist", "doc"); got != "version 2" {
		t.Fatalf("after deleting the current version: %q", got)
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("hist"), Key: aws.String("doc"), VersionId: aws.String(ids[0])})
	wantS3Err(t, err, "NotImplemented")

	// Unversioned buckets: version id "null".
	e.createBucket(t, "plain")
	s3put(t, c, "plain", "k", "v")
	lv, err = c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("plain")})
	if err != nil || len(lv.Versions) != 1 || aws.ToString(lv.Versions[0].VersionId) != "null" || !aws.ToBool(lv.Versions[0].IsLatest) {
		t.Fatalf("unversioned ListObjectVersions = %+v, %v", lv, err)
	}
	if got := func() string {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("plain"), Key: aws.String("k"), VersionId: aws.String("null")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(out.Body)
		out.Body.Close()
		return string(b)
	}(); got != "v" {
		t.Fatalf("GET versionId=null: %q", got)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("plain"), Key: aws.String("k"), VersionId: aws.String("null")}); err != nil {
		t.Fatal(err)
	}
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("plain"), Key: aws.String("k")})
	if s3status(err) != http.StatusNotFound {
		t.Fatalf("after DeleteObject versionId=null: %v", err)
	}
}

// Conditional writes through the group committer: of concurrent creates of
// one key with If-None-Match: *, exactly one succeeds.
func TestS3ConditionalCreateRace(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "race")
	for round := range 5 {
		key := fmt.Sprint("k", round)
		var ok, failed atomic.Int32
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("race"), Key: aws.String(key),
					Body: strings.NewReader(fmt.Sprint(i)), IfNoneMatch: aws.String("*")})
				switch s3code(err) {
				case "":
					ok.Add(1)
				case "PreconditionFailed":
					failed.Add(1)
				default:
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if ok.Load() != 1 || failed.Load() != 15 {
			t.Fatalf("round %d: %d succeeded, %d failed", round, ok.Load(), failed.Load())
		}
	}
}

// A bucket alias exposes a branch of a namespace.
func TestS3BucketAlias(t *testing.T) {
	e := start(t, envOpts{aliases: map[string][2]string{"proj-dev": {"proj", "heads/dev"}}})
	c := e.s3client("admin")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "proj", Config: &f1client.NamespaceConfig{
		Layout: "tree", Retention: "all", Branches: true, Locks: "off", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	s3put(t, c, "proj", "a", "main")
	s3put(t, c, "proj-dev", "a", "dev")
	if s3get(t, c, "proj", "a") != "main" || s3get(t, c, "proj-dev", "a") != "dev" {
		t.Fatal("alias reads the wrong ref")
	}
	refs, err := e.admin.Refs(ctx, "proj")
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
	lb, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 2 {
		t.Fatalf("ListBuckets = %+v, %v", lb, err)
	}
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("proj-dev")})
	if s3status(err) != http.StatusMethodNotAllowed {
		t.Fatalf("DeleteBucket on an alias: %v", err)
	}
}

// Online archive under load with S3 writes (phase 2b procedure).
func TestArchiveUnderS3Load(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "load")
	for i := range 10 {
		s3put(t, c, "load", fmt.Sprint("seed/", i), fmt.Sprint(i))
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
				if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("load"), Key: aws.String(fmt.Sprintf("w%d/%d", w, i)),
					Body: bytes.NewReader(d)}); err != nil {
					t.Error(err)
					failures.Add(1)
				}
				writes.Add(1)
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	var archive bytes.Buffer
	if err := e.admin.Archive(ctx, "load", &archive); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d of %d S3 writes failed during the archive", failures.Load(), writes.Load())
	}
	checkArchive(t, "load", archive.Bytes())
}
