package test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// s3client returns an AWS SDK client for user, signing with the user's S3
// key, against the in-process server (path-style addressing). Checksum
// behavior is the SDK's default configuration (CRC32 when supported, and
// response validation); retries are off.
func (e *env) s3client(user string, opts ...func(*s3.Options)) *s3.Client {
	return s3Client(e.s3url, e.s3keys[user].AccessKey, e.s3keys[user].Secret, opts...)
}

func s3Client(endpoint, key, secret string, opts ...func(*s3.Options)) *s3.Client {
	o := s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(endpoint),
		UsePathStyle:     true,
		RetryMaxAttempts: 1,
		// What config.LoadDefaultConfig sets.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret}, nil
		}),
	}
	return s3.New(o, opts...)
}

// s3code returns the S3 error code of an SDK error ("" for nil).
func s3code(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// s3status returns the HTTP status of an SDK error (0 for nil).
func s3status(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func wantS3Err(t *testing.T, err error, code string) {
	t.Helper()
	if got := s3code(err); got != code {
		t.Fatalf("error = %v (code %q), want code %q", err, got, code)
	}
}

func s3put(t *testing.T, c *s3.Client, bucket, key, data string, mod ...func(*s3.PutObjectInput)) *s3.PutObjectOutput {
	t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(data))}
	for _, m := range mod {
		m(in)
	}
	out, err := c.PutObject(ctx, in)
	if err != nil {
		t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
	return out
}

func s3get(t *testing.T, c *s3.Client, bucket, key string) string {
	t.Helper()
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rawS3 sends a request signed by the caller-provided function and returns
// the response status and body.
func rawS3(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// createBucket creates a bucket through S3 as the server administrator.
func (e *env) createBucket(t testing.TB, name string) {
	t.Helper()
	if _, err := e.s3client("admin").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("CreateBucket %s: %v", name, err)
	}
}
