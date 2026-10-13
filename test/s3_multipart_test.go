package test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// genReader is deterministic, incompressible test content that can be read
// from any offset in O(1): byte o is byte o%8 of splitmix64(seed + o/8). It
// is an io.ReadSeeker, as the SDK needs for payload signing over HTTP.
type genReader struct {
	seed      uint64
	size, off int64
}

func newGen(seed string, size int64) *genReader {
	h := sha256.Sum256([]byte(seed))
	return &genReader{seed: binary.LittleEndian.Uint64(h[:]), size: size}
}

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

func (g *genReader) Read(p []byte) (int, error) {
	n, err := g.ReadAt(p, g.off)
	g.off += int64(n)
	return n, err
}

// ReadAt implements io.ReaderAt.
func (g *genReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= g.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), g.size-off))
	var w [8]byte
	for i := range n {
		o := off + int64(i)
		if i == 0 || o%8 == 0 {
			binary.LittleEndian.PutUint64(w[:], splitmix(g.seed+uint64(o/8)))
		}
		p[i] = w[o%8]
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (g *genReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += g.off
	case io.SeekEnd:
		off += g.size
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	g.off = off
	return off, nil
}

func digestOf(r io.Reader) (md5sum []byte, sha string) {
	m, s := md5.New(), sha256.New()
	io.Copy(io.MultiWriter(m, s), r)
	return m.Sum(nil), hex.EncodeToString(s.Sum(nil))
}

// uploadParts uploads parts in parallel and returns them for completion.
func uploadParts(t *testing.T, c *s3.Client, bucket, key, id string, parts []*genReader, alg types.ChecksumAlgorithm) []types.CompletedPart {
	t.Helper()
	out := make([]types.CompletedPart, len(parts))
	var wg sync.WaitGroup
	errs := make(chan error, len(parts))
	for i, p := range parts {
		wg.Go(func() {
			res, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
				PartNumber: aws.Int32(int32(i + 1)), Body: p, ContentLength: aws.Int64(p.size), ChecksumAlgorithm: alg})
			if err != nil {
				errs <- fmt.Errorf("part %d: %w", i+1, err)
				return
			}
			out[i] = types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: res.ETag, ChecksumCRC32: res.ChecksumCRC32,
				ChecksumCRC32C: res.ChecksumCRC32C, ChecksumCRC64NVME: res.ChecksumCRC64NVME, ChecksumSHA1: res.ChecksumSHA1, ChecksumSHA256: res.ChecksumSHA256}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return out
}

// A large object uploaded in parallel parts reads back identically and has
// the ETag of the AWS formula (doc/plan.md, phase 4 acceptance).
func TestS3MultipartLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("large transfer")
	}
	nparts, partSize := 20, int64(512<<20)/20
	switch {
	case os.Getenv("F1_TEST_LARGE") != "":
		nparts, partSize = 100, int64(5<<30)/100
	case raceEnabled:
		partSize = 5 << 20
	}
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "big")
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("blob"),
		ContentType: aws.String("application/x-blob")})
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]*genReader, nparts)
	for i := range parts {
		size := partSize
		if i == nparts-1 {
			size = partSize/2 + 7 // a short last part
		}
		parts[i] = newGen(fmt.Sprint("part", i), size)
	}
	done := uploadParts(t, c, "big", "blob", aws.ToString(cu.UploadId), parts, "")
	res, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("big"), Key: aws.String("blob"),
		UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	if err != nil {
		t.Fatal(err)
	}
	// ETag: MD5 of the concatenated part MD5s, "-<parts>"; content: the
	// parts in order.
	var md5s bytes.Buffer
	whole := sha256.New()
	var total int64
	for _, p := range parts {
		p.Seek(0, io.SeekStart)
		m, _ := digestOf(io.TeeReader(p, whole))
		md5s.Write(m)
		total += p.size
	}
	sum := md5.Sum(md5s.Bytes())
	wantETag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), nparts)
	if aws.ToString(res.ETag) != wantETag {
		t.Fatalf("ETag %s, want %s", aws.ToString(res.ETag), wantETag)
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("big"), Key: aws.String("blob")})
	if err != nil {
		t.Fatal(err)
	}
	_, got := digestOf(out.Body)
	out.Body.Close()
	if got != hex.EncodeToString(whole.Sum(nil)) || aws.ToInt64(out.ContentLength) != total || aws.ToString(out.ETag) != wantETag ||
		aws.ToString(out.ContentType) != "application/x-blob" {
		t.Fatalf("download: sha256 %s, length %d, etag %s", got, aws.ToInt64(out.ContentLength), aws.ToString(out.ETag))
	}
	// A range across a part boundary.
	off := partSize - 100
	rg, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("big"), Key: aws.String("blob"), Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+199))})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rg.Body)
	rg.Body.Close()
	want := make([]byte, 200)
	parts[0].Seek(off, io.SeekStart)
	io.ReadFull(parts[0], want[:100])
	parts[1].Seek(0, io.SeekStart)
	io.ReadFull(parts[1], want[100:])
	if !bytes.Equal(b, want) {
		t.Fatal("range across a part boundary differs")
	}
	// The upload is gone.
	lu, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("big")})
	if err != nil || len(lu.Uploads) != 0 {
		t.Fatalf("uploads after complete: %+v, %v", lu, err)
	}
}

func TestS3Multipart(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "mpu")
	const part = 5 << 20
	newUpload := func(key string, mod ...func(*s3.CreateMultipartUploadInput)) string {
		in := &s3.CreateMultipartUploadInput{Bucket: aws.String("mpu"), Key: aws.String(key)}
		for _, m := range mod {
			m(in)
		}
		out, err := c.CreateMultipartUpload(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return aws.ToString(out.UploadId)
	}
	complete := func(key, id string, parts []types.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
		return c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("mpu"), Key: aws.String(key),
			UploadId: aws.String(id), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	}

	// Listing uploads and parts, with paging.
	ids := map[string]string{}
	for _, k := range []string{"x/1", "x/2", "y", "x/1"} {
		ids[k] = newUpload(k)
	}
	lu, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("mpu"), Prefix: aws.String("x/")})
	if err != nil || len(lu.Uploads) != 3 {
		t.Fatalf("ListMultipartUploads = %+v, %v", lu, err)
	}
	lu, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("mpu"), Delimiter: aws.String("/")})
	if err != nil || len(lu.Uploads) != 1 || len(lu.CommonPrefixes) != 1 || aws.ToString(lu.CommonPrefixes[0].Prefix) != "x/" {
		t.Fatalf("ListMultipartUploads with delimiter = %+v, %v", lu, err)
	}
	var seen []string
	p := s3.NewListMultipartUploadsPaginator(c, &s3.ListMultipartUploadsInput{Bucket: aws.String("mpu"), MaxUploads: aws.Int32(1)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range page.Uploads {
			seen = append(seen, aws.ToString(u.Key))
		}
	}
	if !slices.Equal(seen, []string{"x/1", "x/1", "x/2", "y"}) {
		t.Fatalf("paged uploads %q", seen)
	}
	id := ids["y"]
	small := []*genReader{newGen("s1", part), newGen("s2", part), newGen("s3", 10)}
	parts := uploadParts(t, c, "mpu", "y", id, small, "")
	lp, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("mpu"), Key: aws.String("y"), UploadId: aws.String(id), MaxParts: aws.Int32(2)})
	if err != nil || len(lp.Parts) != 2 || !aws.ToBool(lp.IsTruncated) || aws.ToString(lp.NextPartNumberMarker) != "2" {
		t.Fatalf("ListParts = %+v, %v", lp, err)
	}

	// Completion errors.
	_, err = complete("y", id, []types.CompletedPart{parts[1], parts[0]})
	wantS3Err(t, err, "InvalidPartOrder")
	bad := slices.Clone(parts)
	bad[1].ETag = aws.String(`"0000"`)
	_, err = complete("y", id, bad)
	wantS3Err(t, err, "InvalidPart")
	_, err = complete("y", id, []types.CompletedPart{parts[2], {PartNumber: aws.Int32(4), ETag: parts[2].ETag}})
	wantS3Err(t, err, "InvalidPart")
	_, err = complete("y", "nonexistent", parts)
	wantS3Err(t, err, "NoSuchUpload")
	// A part below the minimum size may only be last.
	tiny := uploadParts(t, c, "mpu", "x/2", ids["x/2"], []*genReader{newGen("t1", 100), newGen("t2", 100)}, "")
	_, err = complete("x/2", ids["x/2"], tiny)
	wantS3Err(t, err, "EntityTooSmall")
	first, err := complete("y", id, parts)
	if err != nil {
		t.Fatal(err)
	}
	// A repeated completion returns the same result, as in S3.
	again, err := complete("y", id, parts)
	if err != nil || aws.ToString(again.ETag) != aws.ToString(first.ETag) {
		t.Fatalf("repeated completion = %+v, %v", again, err)
	}
	_, err = c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("mpu"), Key: aws.String("y"), UploadId: aws.String(id),
		PartNumber: aws.Int32(1), Body: strings.NewReader("x")})
	wantS3Err(t, err, "NoSuchUpload")

	// Abort.
	if _, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("mpu"), Key: aws.String("x/2"), UploadId: aws.String(ids["x/2"])}); err != nil {
		t.Fatal(err)
	}
	_, err = c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("mpu"), Key: aws.String("x/2"), UploadId: aws.String(ids["x/2"]),
		PartNumber: aws.Int32(1), Body: strings.NewReader("x")})
	wantS3Err(t, err, "NoSuchUpload")

	// UploadPartCopy: parts copied from ranges of an existing object, here
	// "y" itself in reverse order, and from another bucket.
	e.createBucket(t, "other")
	s3put(t, c, "other", "src", strings.Repeat("o", 1000))
	cid := newUpload("copied")
	var cparts []types.CompletedPart
	for i, src := range []struct{ source, rng string }{
		{"mpu/y", fmt.Sprintf("bytes=%d-%d", part, 2*part-1)},
		{"mpu/y", fmt.Sprintf("bytes=0-%d", part-1)},
		{"other/src", "bytes=10-19"},
	} {
		out, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String("mpu"), Key: aws.String("copied"), UploadId: aws.String(cid),
			PartNumber: aws.Int32(int32(i + 1)), CopySource: aws.String(src.source), CopySourceRange: aws.String(src.rng)})
		if err != nil {
			t.Fatalf("UploadPartCopy %d: %v", i+1, err)
		}
		cparts = append(cparts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.CopyPartResult.ETag})
	}
	_, err = c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String("mpu"), Key: aws.String("copied"), UploadId: aws.String(cid),
		PartNumber: aws.Int32(9), CopySource: aws.String("other/src"), CopySourceRange: aws.String("bytes=10-5000")})
	wantS3Err(t, err, "InvalidRange")
	if _, err := complete("copied", cid, cparts); err != nil {
		t.Fatal(err)
	}
	got := s3get(t, c, "mpu", "copied")
	var want bytes.Buffer
	small[1].Seek(0, io.SeekStart)
	io.Copy(&want, small[1])
	small[0].Seek(0, io.SeekStart)
	io.Copy(&want, small[0])
	want.WriteString(strings.Repeat("o", 10))
	if got != want.String() {
		t.Fatalf("UploadPartCopy result: %d bytes, want %d", len(got), want.Len())
	}
}

// Multipart checksums: composite (CRC32, SHA256) and full-object (CRC32C,
// CRC64NVME, combined from the part CRCs without reading the content). The
// SDK validates full-object checksums on GetObject.
func TestS3MultipartChecksums(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "cks")
	for _, tc := range []struct {
		alg types.ChecksumAlgorithm
		typ types.ChecksumType
	}{
		{types.ChecksumAlgorithmCrc32, types.ChecksumTypeComposite},
		{types.ChecksumAlgorithmSha256, types.ChecksumTypeComposite},
		{types.ChecksumAlgorithmCrc32, types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmCrc32c, types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmCrc64nvme, types.ChecksumTypeFullObject},
	} {
		key := string(tc.alg) + "-" + string(tc.typ)
		cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("cks"), Key: aws.String(key),
			ChecksumAlgorithm: tc.alg, ChecksumType: tc.typ})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		parts := []*genReader{newGen(key+"1", 5<<20), newGen(key+"2", 5<<20+3), newGen(key+"3", 1234)}
		done := uploadParts(t, c, "cks", key, aws.ToString(cu.UploadId), parts, tc.alg)
		res, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("cks"), Key: aws.String(key),
			UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}, ChecksumType: tc.typ})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if res.ChecksumType != tc.typ {
			t.Fatalf("%s: checksum type %q", key, res.ChecksumType)
		}
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("cks"), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, out.Body) // validated by the SDK for FULL_OBJECT
		out.Body.Close()
		if err != nil || n != 10<<20+3+1234 {
			t.Fatalf("%s: read %d, %v", key, n, err)
		}
		if out.ChecksumType != tc.typ {
			t.Fatalf("%s: GetObject checksum type %q", key, out.ChecksumType)
		}
	}
	// Invalid combinations.
	_, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("cks"), Key: aws.String("x"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha1, ChecksumType: types.ChecksumTypeFullObject})
	wantS3Err(t, err, "InvalidRequest")
}
