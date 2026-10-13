package test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/trukeio/f1/pkg/f1client"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestS3Buckets(t *testing.T) {
	e := start(t, envOpts{})
	admin, alice := e.s3client("admin"), e.s3client("alice")

	e.createBucket(t, "photos")
	e.createBucket(t, "photos") // re-creating one's own bucket succeeds (us-east-1)
	for _, bad := range []string{"Bad_Name", "ab", "a..b", "a.-b", "a-.b", "192.168.5.123", "-ab"} {
		_, err := admin.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bad)})
		wantS3Err(t, err, "InvalidBucketName")
	}
	// Only server administrators create buckets.
	_, err := alice.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("mine")})
	wantS3Err(t, err, "AccessDenied")

	// The bucket is a namespace with the S3 preset.
	ns, err := e.admin.Namespaces(ctx)
	if err != nil || len(ns) != 1 || ns[0].Name != "photos" || ns[0].Config.Layout != "flat" || !ns[0].Config.Autocommit {
		t.Fatalf("namespaces = %+v, %v", ns, err)
	}

	// Alice sees only buckets she has access to; unknown and inaccessible
	// buckets look the same.
	e.createBucket(t, "docs")
	e.setACL(t, "docs", "user:alice", "", "read")
	lb, err := alice.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 1 || *lb.Buckets[0].Name != "docs" {
		t.Fatalf("alice ListBuckets = %+v, %v", lb, err)
	}
	lb, err = admin.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 2 {
		t.Fatalf("admin ListBuckets = %+v, %v", lb, err)
	}
	var paged []string
	for p := s3.NewListBucketsPaginator(admin, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1)}); p.HasMorePages(); {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Buckets {
			paged = append(paged, aws.ToString(b.Name))
		}
	}
	if !slices.Equal(paged, []string{"docs", "photos"}) {
		t.Fatalf("paged ListBuckets = %q", paged)
	}
	if _, err := alice.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("docs")}); err != nil {
		t.Fatal(err)
	}
	_, err = alice.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("photos")})
	if s3status(err) != http.StatusNotFound {
		t.Fatalf("HeadBucket without access: %v", err)
	}
	_, err = alice.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos")})
	wantS3Err(t, err, "NoSuchBucket")
	_, err = alice.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("nothere")})
	wantS3Err(t, err, "NoSuchBucket")

	loc, err := admin.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("photos")})
	if err != nil || loc.LocationConstraint != "" {
		t.Fatalf("GetBucketLocation = %+v, %v", loc, err)
	}
	ver, err := admin.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("photos")})
	if err != nil || ver.Status != "" {
		t.Fatalf("GetBucketVersioning = %+v, %v", ver, err)
	}

	// Unsupported operations are 501 NotImplemented.
	_, err = admin.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String("photos")})
	wantS3Err(t, err, "NotImplemented")
	_, err = admin.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	wantS3Err(t, err, "NotImplemented")

	// DeleteBucket: only when empty; the directory is moved aside.
	s3put(t, admin, "photos", "a.jpg", "jpeg")
	_, err = admin.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("photos")})
	wantS3Err(t, err, "BucketNotEmpty")
	_, err = alice.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("docs")})
	wantS3Err(t, err, "AccessDenied")
	if _, err := admin.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("a.jpg")}); err != nil {
		t.Fatal(err)
	}
	// Open multipart uploads do not keep a bucket; they are discarded.
	if _, err := admin.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("big")}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}
	_, err = admin.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("photos")})
	if s3status(err) != http.StatusNotFound {
		t.Fatalf("HeadBucket after delete: %v", err)
	}
	e.createBucket(t, "photos") // the name is free again
}

func TestS3Objects(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "bkt")

	put := s3put(t, c, "bkt", "dir/hello.txt", "hello world", func(in *s3.PutObjectInput) {
		in.ContentType = aws.String("text/plain")
		in.Metadata = map[string]string{"color": "blue"}
		in.CacheControl = aws.String("max-age=60")
		in.ContentDisposition = aws.String(`attachment; filename="h.txt"`)
	})
	if got := strings.Trim(aws.ToString(put.ETag), `"`); got != md5hex("hello world") {
		t.Fatalf("ETag = %s", got)
	}
	if put.VersionId != nil {
		t.Fatalf("unversioned bucket returned version %s", *put.VersionId)
	}
	get, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt")})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if string(body) != "hello world" || aws.ToString(get.ContentType) != "text/plain" || get.Metadata["color"] != "blue" ||
		aws.ToString(get.CacheControl) != "max-age=60" || aws.ToString(get.ContentDisposition) != `attachment; filename="h.txt"` ||
		aws.ToInt64(get.ContentLength) != 11 || aws.ToString(get.ETag) != `"`+md5hex("hello world")+`"` {
		t.Fatalf("GetObject = %q %+v", body, get)
	}
	if time.Since(aws.ToTime(get.LastModified)) > time.Minute {
		t.Fatalf("LastModified = %v", get.LastModified)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt")})
	if err != nil || aws.ToInt64(head.ContentLength) != 11 || head.Metadata["color"] != "blue" {
		t.Fatalf("HeadObject = %+v, %v", head, err)
	}

	// Ranges.
	for _, tc := range []struct{ rng, want string }{
		{"bytes=0-4", "hello"}, {"bytes=6-", "world"}, {"bytes=-5", "world"}, {"bytes=6-100", "world"},
	} {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), Range: aws.String(tc.rng)})
		if err != nil {
			t.Fatalf("%s: %v", tc.rng, err)
		}
		b, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if string(b) != tc.want || !strings.HasPrefix(aws.ToString(out.ContentRange), "bytes ") {
			t.Fatalf("%s = %q (%s)", tc.rng, b, aws.ToString(out.ContentRange))
		}
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), Range: aws.String("bytes=11-")})
	wantS3Err(t, err, "InvalidRange")

	// Conditional reads.
	etag := `"` + md5hex("hello world") + `"`
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), IfNoneMatch: aws.String(etag)})
	if s3status(err) != http.StatusNotModified {
		t.Fatalf("If-None-Match: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), IfMatch: aws.String(`"nope"`)})
	wantS3Err(t, err, "PreconditionFailed")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), IfModifiedSince: aws.Time(time.Now().Add(time.Hour))})
	if s3status(err) != http.StatusNotModified {
		t.Fatalf("If-Modified-Since: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), IfUnmodifiedSince: aws.Time(time.Now().Add(-time.Hour))})
	wantS3Err(t, err, "PreconditionFailed")
	// If-Match wins over a false If-Unmodified-Since.
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), IfMatch: aws.String(etag),
		IfUnmodifiedSince: aws.Time(time.Now().Add(-time.Hour))}); err != nil {
		t.Fatal(err)
	}

	// Conditional writes.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), Body: strings.NewReader("x"), IfNoneMatch: aws.String("*")})
	wantS3Err(t, err, "PreconditionFailed")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), Body: strings.NewReader("x"), IfMatch: aws.String(`"bad"`)})
	wantS3Err(t, err, "PreconditionFailed")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("missing"), Body: strings.NewReader("x"), IfMatch: aws.String(etag)})
	wantS3Err(t, err, "NoSuchKey")
	// If-None-Match with ETags: refused when the current ETag matches.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/hello.txt"), Body: strings.NewReader("x"), IfNoneMatch: aws.String(etag)})
	wantS3Err(t, err, "PreconditionFailed")
	s3put(t, c, "bkt", "dir/hello.txt", "hello again", func(in *s3.PutObjectInput) { in.IfMatch = aws.String(etag) })
	s3put(t, c, "bkt", "dir/hello.txt", "hello again", func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String(etag) })
	s3put(t, c, "bkt", "new", "n", func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })

	// Integrity: a wrong Content-MD5 is rejected and nothing is written.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("bad-md5"), Body: strings.NewReader("data"),
		ContentMD5: aws.String("1B2M2Y8AsgTpgAmY7PhCfg==")})
	wantS3Err(t, err, "BadDigest")
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("bad-md5")})
	if s3status(err) != http.StatusNotFound {
		t.Fatalf("object written despite BadDigest: %v", err)
	}

	// Errors.
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("nothing")})
	wantS3Err(t, err, "NoSuchKey")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("nobucket"), Key: aws.String("x")})
	wantS3Err(t, err, "NoSuchBucket")

	// Conditional deletes: checked when the key exists.
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new"), IfMatch: aws.String(`"bad"`)})
	wantS3Err(t, err, "PreconditionFailed")
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new"), IfMatchSize: aws.Int64(99)})
	wantS3Err(t, err, "PreconditionFailed")
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new"), IfMatchSize: aws.Int64(1),
		IfMatch: aws.String(`"` + md5hex("n") + `"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new"), IfMatch: aws.String(`"bad"`)}); err != nil {
		t.Fatalf("conditional delete of a missing key: %v", err)
	}
	s3put(t, c, "bkt", "new", "n")

	// Delete: missing keys succeed.
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("new")}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		s3put(t, c, "bkt", fmt.Sprintf("many/%d", i), "x")
	}
	del, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
		{Key: aws.String("many/0")}, {Key: aws.String("many/1")}, {Key: aws.String("many/9")},
	}}})
	if err != nil || len(del.Deleted) != 3 || len(del.Errors) != 0 {
		t.Fatalf("DeleteObjects = %+v, %v", del, err)
	}
	ls, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt"), Prefix: aws.String("many/")})
	if err != nil || aws.ToInt32(ls.KeyCount) != 3 {
		t.Fatalf("after DeleteObjects: %+v, %v", ls, err)
	}
}

func TestS3Copy(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "src")
	e.createBucket(t, "dst")
	data := strings.Repeat("copy me ", 50000)
	s3put(t, c, "src", "a", data, func(in *s3.PutObjectInput) {
		in.ContentType = aws.String("text/x-a")
		in.Metadata = map[string]string{"k": "v"}
	})

	// Same bucket, metadata copied.
	out, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("bkt"), CopySource: aws.String("src/a")})
	if err != nil || strings.Trim(aws.ToString(out.CopyObjectResult.ETag), `"`) != md5hex(data) {
		t.Fatalf("CopyObject = %+v, %v", out, err)
	}
	h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("src"), Key: aws.String("bkt")})
	if err != nil || aws.ToString(h.ContentType) != "text/x-a" || h.Metadata["k"] != "v" {
		t.Fatalf("copied head = %+v, %v", h, err)
	}
	// Another bucket (another repository: the content is streamed), with
	// metadata replaced; a key with special characters.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("dst"), Key: aws.String("x y+z"), CopySource: aws.String("src/a"),
		MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String("text/x-new"), Metadata: map[string]string{"n": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	h, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("dst"), Key: aws.String("x y+z")})
	if err != nil || aws.ToString(h.ContentType) != "text/x-new" || h.Metadata["n"] != "1" || h.Metadata["k"] != "" {
		t.Fatalf("replaced head = %+v, %v", h, err)
	}
	if s3get(t, c, "dst", "x y+z") != data {
		t.Fatal("copied content differs")
	}
	// Copy to itself needs REPLACE.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("a"), CopySource: aws.String("src/a")})
	wantS3Err(t, err, "InvalidRequest")
	// Copy-source conditions.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("c"), CopySource: aws.String("src/a"),
		CopySourceIfMatch: aws.String(`"other"`)})
	wantS3Err(t, err, "PreconditionFailed")
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("src"), Key: aws.String("c"), CopySource: aws.String("src/missing")})
	wantS3Err(t, err, "NoSuchKey")
}

func TestS3Listing(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	// A tree-layout namespace: listings are in byte order of the full key,
	// so "a.txt" (0x2E) comes before "a/b" (0x2F).
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "tree", Preset: "s3",
		Config: nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "svn", Preset: "svn"}); err != nil {
		t.Fatal(err)
	}
	keys := []string{"a/b", "a.txt", "a/c/d", "b", "a-x/1", "a-x/2", "z", "a/c/e", "ab"}
	for _, bucket := range []string{"tree", "svn"} {
		for _, k := range keys {
			s3put(t, c, bucket, k, k)
		}
	}
	sorted := slices.Clone(keys)
	sort.Strings(sorted)
	for _, bucket := range []string{"tree", "svn"} {
		// Every page size gives the same keys in byte order.
		for _, max := range []int32{1, 2, 3, 1000} {
			var got []string
			p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(max)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range page.Contents {
					got = append(got, aws.ToString(o.Key))
					if aws.ToInt64(o.Size) != int64(len(aws.ToString(o.Key))) || strings.Trim(aws.ToString(o.ETag), `"`) != md5hex(aws.ToString(o.Key)) {
						t.Fatalf("%s: object %+v", bucket, o)
					}
				}
			}
			if !slices.Equal(got, sorted) {
				t.Fatalf("%s max %d: keys %q, want %q", bucket, max, got, sorted)
			}
		}
		// Delimiter "/" and another delimiter, with pagination.
		for _, tc := range []struct {
			prefix, delim string
			want          []string // keys, and prefixes with a trailing "*"
		}{
			{"", "/", []string{"a-x/*", "a.txt", "a/*", "ab", "b", "z"}},
			{"a/", "/", []string{"a/b", "a/c/*"}},
			{"", "-", []string{"a-*", "a.txt", "a/b", "a/c/d", "a/c/e", "ab", "b", "z"}},
			{"a", "", []string{"a-x/1", "a-x/2", "a.txt", "a/b", "a/c/d", "a/c/e", "ab"}},
		} {
			for _, max := range []int32{1, 2, 1000} {
				var got []string
				p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(tc.prefix),
					Delimiter: aws.String(tc.delim), MaxKeys: aws.Int32(max)})
				for p.HasMorePages() {
					page, err := p.NextPage(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var items []string
					for _, o := range page.Contents {
						items = append(items, aws.ToString(o.Key))
					}
					for _, cp := range page.CommonPrefixes {
						items = append(items, aws.ToString(cp.Prefix)+"*")
					}
					sort.Slice(items, func(i, j int) bool { return strings.TrimSuffix(items[i], "*") < strings.TrimSuffix(items[j], "*") })
					got = append(got, items...)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("%s prefix %q delim %q max %d: %q, want %q", bucket, tc.prefix, tc.delim, max, got, tc.want)
				}
			}
		}
		// start-after, and ListObjects v1 with markers.
		out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), StartAfter: aws.String("a/c/d")})
		if err != nil || len(out.Contents) != 4 || aws.ToString(out.Contents[0].Key) != "a/c/e" {
			t.Fatalf("start-after: %+v, %v", out, err)
		}
		var v1 []string
		marker := ""
		for {
			out, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String(bucket), Marker: aws.String(marker), MaxKeys: aws.Int32(4)})
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range out.Contents {
				v1 = append(v1, aws.ToString(o.Key))
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			marker = aws.ToString(out.NextMarker)
		}
		if !slices.Equal(v1, sorted) {
			t.Fatalf("%s v1: %q", bucket, v1)
		}
	}
	// URL encoding of keys in listings.
	s3put(t, c, "tree", "sp ace/é", "x")
	out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("tree"), Prefix: aws.String("sp"), EncodingType: types.EncodingTypeUrl})
	if err != nil || len(out.Contents) != 1 {
		t.Fatalf("encoded listing: %+v, %v", out, err)
	}
	if k, err := url.QueryUnescape(aws.ToString(out.Contents[0].Key)); err != nil || k != "sp ace/é" {
		t.Fatalf("encoded key %q", aws.ToString(out.Contents[0].Key))
	}
}

// Keys that the tree layout cannot represent are refused with
// 400 InvalidArgument; the flat layout accepts them.
func TestS3TreeLayoutKeys(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "flat")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "tree", Config: &f1client.NamespaceConfig{
		Layout: "tree", Retention: "none", Autocommit: true, Locks: "off", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dir/", "a//b", "/lead", ".", "..", "./x", "a/../b", "a/./b"} {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("tree"), Key: aws.String(key), Body: strings.NewReader("x")})
		if s3code(err) != "InvalidArgument" || s3status(err) != 400 || !strings.Contains(err.Error(), "layout = flat") {
			t.Fatalf("tree %q: %v", key, err)
		}
		s3put(t, c, "flat", key, "x")
		if got := s3get(t, c, "flat", key); got != "x" {
			t.Fatalf("flat %q = %q", key, got)
		}
	}
	// Hierarchy conflicts are 409.
	s3put(t, c, "tree", "a", "file")
	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("tree"), Key: aws.String("a/b"), Body: strings.NewReader("x")})
	if s3status(err) != http.StatusConflict {
		t.Fatalf("hierarchy conflict: %v", err)
	}
	// In flat layout, "a" and "a/b" coexist.
	s3put(t, c, "flat", "a", "1")
	s3put(t, c, "flat", "a/b", "2")
	if s3get(t, c, "flat", "a") != "1" || s3get(t, c, "flat", "a/b") != "2" {
		t.Fatal("flat keys")
	}
	// Directories are not objects.
	s3put(t, c, "tree", "d/f", "x")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("tree"), Key: aws.String("d")})
	wantS3Err(t, err, "NoSuchKey")
}

// S3 writes follow ACLs and locks; listings are filtered.
func TestS3Permissions(t *testing.T) {
	e := start(t, envOpts{})
	admin, alice, bob := e.s3client("admin"), e.s3client("alice"), e.s3client("bob")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "proj", Preset: "svn"}); err != nil {
		t.Fatal(err)
	}
	e.setACL(t, "proj", "group:eng", "", "read")
	e.setACL(t, "proj", "user:alice", "src/", "read,write,lock")
	e.setACL(t, "proj", "user:alice", "secret/", "")
	s3put(t, admin, "proj", "src/a.c", "a")
	s3put(t, admin, "proj", "secret/key", "k")
	s3put(t, admin, "proj", "README", "r")

	s3put(t, alice, "proj", "src/b.c", "b")
	_, err := alice.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("proj"), Key: aws.String("README"), Body: strings.NewReader("x")})
	wantS3Err(t, err, "AccessDenied")
	_, err = alice.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("proj"), Key: aws.String("secret/key")})
	wantS3Err(t, err, "AccessDenied")
	out, err := alice.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("proj")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range out.Contents {
		got = append(got, aws.ToString(o.Key))
	}
	if !slices.Equal(got, []string{"README", "src/a.c", "src/b.c"}) {
		t.Fatalf("alice sees %q", got)
	}
	_, err = bob.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("proj"), Key: aws.String("README")})
	wantS3Err(t, err, "NoSuchBucket")

	// The svn preset needs locks on *.docx: S3 writes are refused with
	// AccessDenied naming the problem.
	_, err = alice.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("proj"), Key: aws.String("src/spec.docx"), Body: strings.NewReader("x")})
	if s3code(err) != "AccessDenied" || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("unlocked docx: %v", err)
	}
	if _, err := e.alice.Lock(ctx, "proj", "main", "src/spec.docx", 0); err != nil {
		t.Fatal(err)
	}
	s3put(t, alice, "proj", "src/spec.docx", "x")
	_, err = admin.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("proj"), Key: aws.String("src/spec.docx"), Body: strings.NewReader("y")})
	if s3code(err) != "AccessDenied" || !strings.Contains(err.Error(), "locked by user:alice") {
		t.Fatalf("locked by alice: %v", err)
	}

	// Revoked and unknown keys.
	if err := e.db.RevokeS3Key(ctx, "user:bob", e.s3keys["bob"].AccessKey); err != nil {
		t.Fatal(err)
	}
	_, err = bob.ListBuckets(ctx, &s3.ListBucketsInput{})
	wantS3Err(t, err, "InvalidAccessKeyId")
	wrong := s3Client(e.s3url, e.s3keys["alice"].AccessKey, "wrong-secret")
	_, err = wrong.ListBuckets(ctx, &s3.ListBucketsInput{})
	wantS3Err(t, err, "SignatureDoesNotMatch")
}

// Presigned URLs work for GET and PUT and expire.
func TestS3Presigned(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "bkt")
	ps := s3.NewPresignClient(c)
	up, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("p/q r")}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(up.Method, up.URL, strings.NewReader("presigned"))
	for k, v := range up.SignedHeader {
		req.Header[k] = v
	}
	if status, body := rawS3(t, req); status != 200 {
		t.Fatalf("presigned PUT: %d %s", status, body)
	}
	down, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("p/q r")}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(down.Method, down.URL, nil)
	if status, body := rawS3(t, req); status != 200 || body != "presigned" {
		t.Fatalf("presigned GET: %d %s", status, body)
	}
	// A tampered URL fails.
	req, _ = http.NewRequest(down.Method, strings.Replace(down.URL, "q%20r", "q%20s", 1), nil)
	if status, body := rawS3(t, req); status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered GET: %d %s", status, body)
	}
	// An expired URL fails.
	old, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("p/q r")}, s3.WithPresignExpires(time.Second),
		func(o *s3.PresignOptions) {
			o.ClientOptions = append(o.ClientOptions, func(o *s3.Options) {})
		})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	req, _ = http.NewRequest(old.Method, old.URL, nil)
	if status, body := rawS3(t, req); status != 403 || !strings.Contains(body, "expired") {
		t.Fatalf("expired GET: %d %s", status, body)
	}
}

// Checksums: the SDK's default (CRC32), explicit algorithms, returned with
// checksum mode, and refused when wrong.
func TestS3Checksums(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "bkt")
	data := bytes.Repeat([]byte("checksum "), 10000)
	for _, alg := range []types.ChecksumAlgorithm{"", types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c,
		types.ChecksumAlgorithmCrc64nvme, types.ChecksumAlgorithmSha1, types.ChecksumAlgorithmSha256} {
		key := "obj-" + string(alg)
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key), Body: bytes.NewReader(data),
			ChecksumAlgorithm: alg}); err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		if err != nil {
			t.Fatal(err)
		}
		// The SDK validates the returned checksum while reading.
		got, err := io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: read %d bytes, %v", alg, len(got), err)
		}
		// With the SDK defaults, a CRC32 is sent when no algorithm is
		// chosen; F1 returns what was sent.
		if out.ChecksumCRC32 == nil && out.ChecksumCRC32C == nil && out.ChecksumCRC64NVME == nil && out.ChecksumSHA1 == nil && out.ChecksumSHA256 == nil {
			t.Fatalf("%s: no checksum returned", alg)
		}
	}
	// A client that sends no checksum gets none back.
	plain := e.s3client("admin", func(o *s3.Options) { o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired })
	s3put(t, plain, "bkt", "plain", "no checksum")
	h, err := plain.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("plain"), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil || h.ChecksumCRC32 != nil || h.ChecksumCRC64NVME != nil {
		t.Fatalf("HeadObject without checksum = %+v, %v", h, err)
	}
	// A wrong checksum value is refused.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("bad"), Body: bytes.NewReader(data),
		ChecksumCRC32: aws.String("AAAAAA==")})
	wantS3Err(t, err, "BadDigest")
}

// Files written by the native API have no S3 attributes: S3 serves the
// MD5 from their DigestRecord as ETag and the commit time as LastModified.
func TestS3ReadsNativeFiles(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "native", Preset: "git"}); err != nil {
		t.Fatal(err)
	}
	res := put(t, e.admin, "native", "src/main.go", "package main\n", "new")
	put(t, e.admin, "native", "README", "readme\n", res.Commit)
	h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("native"), Key: aws.String("src/main.go")})
	if err != nil || strings.Trim(aws.ToString(h.ETag), `"`) != md5hex("package main\n") || time.Since(aws.ToTime(h.LastModified)) > time.Minute ||
		aws.ToString(h.ContentType) != "binary/octet-stream" {
		t.Fatalf("HeadObject = %+v, %v", h, err)
	}
	out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("native")})
	if err != nil || len(out.Contents) != 2 {
		t.Fatalf("ListObjectsV2 = %+v, %v", out, err)
	}
	for _, o := range out.Contents {
		if time.Since(aws.ToTime(o.LastModified)) > time.Minute || aws.ToTime(o.LastModified).Before(time.Now().Add(-time.Hour)) {
			t.Fatalf("%s: LastModified %v", aws.ToString(o.Key), o.LastModified)
		}
	}
}
