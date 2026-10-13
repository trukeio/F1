package test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// streamingPut sends a PUT with an aws-chunked body in the given payload
// mode, signed with the AWS SDK signer (seed and chunk signatures). The
// trailer signature (signed trailer mode) follows the AWS documentation.
// corrupt damages the second chunk's signature.
func streamingPut(t *testing.T, e *env, bucket, key string, data []byte, mode string, chunk int, corrupt bool) (int, string) {
	t.Helper()
	k := e.s3keys["admin"]
	creds := aws.Credentials{AccessKeyID: k.AccessKey, SecretAccessKey: k.Secret}
	now := time.Now().UTC()
	signed := mode != "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	trailer := strings.HasSuffix(mode, "TRAILER")
	crc := crc32.ChecksumIEEE(data)
	crcB64 := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})

	req, _ := http.NewRequest(http.MethodPut, e.s3url+"/"+bucket+"/"+key, nil)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(data)))
	req.Header.Set("X-Amz-Content-Sha256", mode)
	if trailer {
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	}
	// The body length must be known before signing: build it with
	// placeholder signatures of the right length, sign, then rebuild.
	build := func(seed string) []byte {
		var body bytes.Buffer
		prev := seed
		ss := v4.NewStreamSigner(creds, "s3", "us-east-1", mustHex(seed))
		emit := func(p []byte, n int) {
			if !signed {
				fmt.Fprintf(&body, "%x\r\n", len(p))
			} else {
				sig, err := ss.GetSignature(ctx, nil, p, now)
				if err != nil {
					t.Fatal(err)
				}
				s := hex.EncodeToString(sig)
				if corrupt && n == 1 {
					s = strings.Repeat("0", 64)
				}
				prev = s
				fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(p), s)
			}
			body.Write(p)
			if len(p) > 0 {
				body.WriteString("\r\n")
			}
		}
		n := 0
		for off := 0; off < len(data); off += chunk {
			emit(data[off:min(off+chunk, len(data))], n)
			n++
		}
		emit(nil, n)
		if !trailer {
			body.WriteString("\r\n")
			return body.Bytes()
		}
		body.WriteString("x-amz-checksum-crc32:" + crcB64 + "\r\n")
		if signed {
			scope := now.Format("20060102") + "/us-east-1/s3/aws4_request"
			th := sha256.Sum256([]byte("x-amz-checksum-crc32:" + crcB64 + "\n"))
			sts := "AWS4-HMAC-SHA256-TRAILER\n" + now.Format("20060102T150405Z") + "\n" + scope + "\n" + prev + "\n" + hex.EncodeToString(th[:])
			body.WriteString("x-amz-trailer-signature:" + hex.EncodeToString(hmacSHA(signingKey(k.Secret, now.Format("20060102")), sts)) + "\r\n")
		}
		body.WriteString("\r\n")
		return body.Bytes()
	}
	placeholder := build(strings.Repeat("0", 64))
	req.ContentLength = int64(len(placeholder))
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, mode, "s3", "us-east-1", now); err != nil {
		t.Fatal(err)
	}
	_, seed, _ := strings.Cut(req.Header.Get("Authorization"), "Signature=")
	body := build(seed)
	req.Body = io.NopCloser(bytes.NewReader(body))
	return rawS3(t, req)
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func hmacSHA(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func signingKey(secret, date string) []byte {
	k := hmacSHA([]byte("AWS4"+secret), date)
	k = hmacSHA(k, "us-east-1")
	k = hmacSHA(k, "s3")
	return hmacSHA(k, "aws4_request")
}

// Every payload mode: signed aws-chunked (with and without trailer),
// unsigned trailer; a bad chunk signature fails and nothing is written.
func TestS3StreamingPayloads(t *testing.T) {
	e := start(t, envOpts{})
	c := e.s3client("admin")
	e.createBucket(t, "stream")
	data := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320000 bytes
	for _, mode := range []string{"STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", "STREAMING-UNSIGNED-PAYLOAD-TRAILER"} {
		for _, chunk := range []int{8192, 65536, len(data)} {
			key := fmt.Sprintf("%s-%d", strings.ToLower(mode), chunk)
			if status, body := streamingPut(t, e, "stream", key, data, mode, chunk, false); status != 200 {
				t.Fatalf("%s: %d %s", key, status, body)
			}
			if s3get(t, c, "stream", key) != string(data) {
				t.Fatalf("%s: content differs", key)
			}
		}
		if mode == "STREAMING-UNSIGNED-PAYLOAD-TRAILER" {
			continue
		}
		status, body := streamingPut(t, e, "stream", "corrupt", data, mode, 65536, true)
		if status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
			t.Fatalf("%s corrupt: %d %s", mode, status, body)
		}
		_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("stream"), Key: aws.String("corrupt")})
		if s3status(err) != 404 {
			t.Fatalf("object written despite a bad chunk signature: %v", err)
		}
	}
}

// Over TLS, the SDK sends unseekable bodies with STREAMING-UNSIGNED-
// PAYLOAD-TRAILER and a CRC32 trailer (its default checksum behavior).
func TestS3SDKOverTLS(t *testing.T) {
	e := start(t, envOpts{})
	e.createBucket(t, "tls")
	backend, _ := url.Parse(e.s3url)
	front := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(backend))
	defer front.Close()
	c := s3Client(front.URL, e.s3keys["admin"].AccessKey, e.s3keys["admin"].Secret, func(o *s3.Options) { o.HTTPClient = front.Client() })
	data := bytes.Repeat([]byte("tls "), 100000)
	// An unseekable body.
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("tls"), Key: aws.String("k"), Body: io.NopCloser(bytes.NewReader(data)),
		ContentLength: aws.Int64(int64(len(data)))}); err != nil {
		t.Fatal(err)
	}
	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("tls"), Key: aws.String("k"), ChecksumMode: "ENABLED"})
	if err != nil || out.ChecksumCRC32 == nil {
		t.Fatalf("HeadObject = %+v, %v (want the CRC32 sent as trailer)", out, err)
	}
	if s3get(t, c, "tls", "k") != string(data) {
		t.Fatal("content differs")
	}
}
