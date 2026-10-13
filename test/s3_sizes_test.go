package test

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Size transparency (D19) over S3: PutObject, GetObject with ranges,
// multipart upload and CopyObject behave the same for every size, and the
// server's memory stays bounded.
func TestSizeTransparencyS3(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	s := startSubprocess(t)
	c := s3Client(s.s3url, s.s3key, s.s3secret)
	clientPeak := sampleHeap(t)
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("sizes-s3")}); err != nil {
		t.Fatal(err)
	}
	for _, n := range testSizes() {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			g := newGen(fmt.Sprint("s3 size ", n), n)
			wantSHA := shaOf(t, io.NewSectionReader(g, 0, n))
			m := md5.New()
			io.Copy(m, io.NewSectionReader(g, 0, n))
			wantMD5 := hex.EncodeToString(m.Sum(nil))

			check := func(key, etag string) {
				t.Helper()
				out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key)})
				if err != nil {
					t.Fatal(err)
				}
				got := shaOf(t, out.Body)
				out.Body.Close()
				if got != wantSHA || aws.ToInt64(out.ContentLength) != n || (etag != "" && strings.Trim(aws.ToString(out.ETag), `"`) != etag) {
					t.Fatalf("%s: sha256 %s length %d etag %s", key, got, aws.ToInt64(out.ContentLength), aws.ToString(out.ETag))
				}
				if n == 0 {
					return
				}
				for _, off := range []int64{0, n / 2, max(n-1000, 0)} {
					l := min(int64(1000), n-off)
					out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key),
						Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+l-1))})
					if err != nil {
						t.Fatal(err)
					}
					b, _ := io.ReadAll(out.Body)
					out.Body.Close()
					want := make([]byte, l)
					g.ReadAt(want, off)
					if string(b) != string(want) {
						t.Fatalf("%s: range at %d differs", key, off)
					}
				}
			}

			// PutObject (single request; the S3 limit is 5 GiB).
			if n <= 5<<30 {
				key := fmt.Sprintf("put/%d", n)
				if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key),
					Body: io.NewSectionReader(g, 0, n), ContentLength: aws.Int64(n)}); err != nil {
					t.Fatal(err)
				}
				check(key, wantMD5)
				// CopyObject to another key (same repository: no copy).
				if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key + ".copy"),
					CopySource: aws.String("sizes-s3/" + key)}); err != nil {
					t.Fatal(err)
				}
				check(key+".copy", wantMD5)
			}

			// Multipart: up to four parts of at least 5 MiB.
			key := fmt.Sprintf("multipart/%d", n)
			cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}
			partSize := max(5<<20, (n+3)/4)
			var parts []types.CompletedPart
			for i, off := int32(1), int64(0); off < n || i == 1; i, off = i+1, off+partSize {
				l := min(partSize, n-off)
				out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key), UploadId: cu.UploadId,
					PartNumber: aws.Int32(i), Body: io.NewSectionReader(g, off, l), ContentLength: aws.Int64(l)})
				if err != nil {
					t.Fatal(err)
				}
				parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(i), ETag: out.ETag})
			}
			if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("sizes-s3"), Key: aws.String(key),
				UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}); err != nil {
				t.Fatal(err)
			}
			check(key, "")
		})
	}
	checkMemory(t, s, clientPeak)
}

func shaOf(t *testing.T, r io.Reader) string {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
