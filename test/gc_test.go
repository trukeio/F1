package test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/testutil"
)

// f1d gc through a running server: a dry run reports exactly what the
// collection then deletes; open multipart uploads and the latest versions
// survive; fsck is clean; an offline collection works on the stopped
// server's repository.
func TestGCServer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs f1d")
	}
	bin := f1dBinary(t)
	e := start(t, envOpts{})
	e.createBucket(t, "gcb") // retention = none
	c := e.s3client("admin")
	for i := range 20 {
		s3put(t, c, "gcb", "k", string(testutil.Bytes(fmt.Sprint("version ", i), 100<<10)))
	}
	last := string(testutil.Bytes("version 19", 100<<10))
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("gcb"), Key: aws.String("mp")})
	if err != nil {
		t.Fatal(err)
	}
	partData := [][]byte{testutil.Bytes("part 1", 5<<20), testutil.Bytes("part 2", 1000)}
	var parts []types.CompletedPart
	for i, d := range partData {
		res, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("gcb"), Key: aws.String("mp"), UploadId: cu.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(d)})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: res.ETag})
	}

	cfg := filepath.Join(t.TempDir(), "f1d.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\ndata_dir = %q\n", e.dataDir)), 0o600)
	count := regexp.MustCompile(`(?:would delete|deleted) (\d+) objects`)
	dry := runCLI(t, bin, "gc", "--config", cfg, "--grace", "0", "--dry-run", "gcb")
	real := runCLI(t, bin, "gc", "--config", cfg, "--grace", "0", "gcb")
	d, r := count.FindStringSubmatch(dry), count.FindStringSubmatch(real)
	if d == nil || r == nil || d[1] != r[1] || d[1] == "0" {
		t.Fatalf("dry run: %q\nreal: %q", dry, real)
	}
	if again := runCLI(t, bin, "gc", "--config", cfg, "--grace", "0", "gcb"); !regexp.MustCompile(`^deleted 0 objects`).MatchString(again) {
		t.Fatalf("second collection: %q", again)
	}
	if got := s3get(t, c, "gcb", "k"); got != last {
		t.Fatal("latest version lost")
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("gcb"), Key: aws.String("mp"),
		UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}); err != nil {
		t.Fatalf("completing an upload after GC: %v", err)
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("gcb"), Key: aws.String("mp")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if !bytes.Equal(got, append(append([]byte(nil), partData[0]...), partData[1]...)) {
		t.Fatal("multipart content after GC differs")
	}

	e.stop()
	if out := runCLI(t, bin, "fsck", "--config", cfg, "gcb"); !regexp.MustCompile(`fsck: ok`).MatchString(out) {
		t.Fatalf("fsck: %s", out)
	}
	dir := filepath.Join(e.dataDir, server.ReposDir, "gcb")
	if out := runCLI(t, bin, "gc", "--grace", "0", "--dir", dir); !regexp.MustCompile(`^deleted \d+ objects`).MatchString(out) {
		t.Fatalf("offline gc: %q", out)
	}
	if out := runCLI(t, bin, "fsck", "--dir", dir); !regexp.MustCompile(`fsck: ok`).MatchString(out) {
		t.Fatalf("fsck after offline gc: %s", out)
	}
}
