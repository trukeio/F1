package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	testKey    = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func testVerifier() *SigV4 {
	return &SigV4{Region: "us-east-1", Service: "s3", Now: func() time.Time { return testNow },
		Secret: func(_ context.Context, k string) (string, error) {
			if k != testKey {
				return "", ErrUnknownAccessKey
			}
			return testSecret, nil
		}}
}

var creds = aws.Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret}

// s3Signer signs as the SDK's S3 client does: paths are not escaped twice.
func s3Signer() *v4.Signer {
	return v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
}

// sign signs a request with the AWS SDK signer, as a server sees it.
func sign(t *testing.T, method, target, payload string, at time.Time, mod func(*http.Request)) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("X-Amz-Content-Sha256", payload)
	if mod != nil {
		mod(r)
	}
	// The signer reads the URL as a client would send it.
	out := r.Clone(context.Background())
	out.URL.Scheme, out.URL.Host = "http", r.Host
	if err := s3Signer().SignHTTP(context.Background(), creds, out, payload, "s3", "us-east-1", at); err != nil {
		t.Fatal(err)
	}
	r.Header = out.Header
	return r
}

func code(err error) string {
	var se *SigV4Error
	if errors.As(err, &se) {
		return se.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestSigV4Header(t *testing.T) {
	v := testVerifier()
	for _, target := range []string{
		"/bucket/key",
		"/bucket/a%20b/c+d/%C3%A9?x-id=GetObject",
		"/bucket?list-type=2&prefix=a%2Fb&delimiter=%2F&max-keys=2",
		"/bucket?uploads",
		"/bucket/k?partNumber=2&uploadId=XYZ",
		"/",
	} {
		r := sign(t, http.MethodGet, target, UnsignedPayload, testNow, nil)
		res, err := v.Verify(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if res.AccessKey != testKey || res.PayloadHash != UnsignedPayload || res.Presigned {
			t.Fatalf("%s: %+v", target, res)
		}
	}
	// Signed headers with repeated spaces and several values.
	r := sign(t, http.MethodPut, "/b/k", emptySHA256, testNow, func(r *http.Request) {
		r.Header.Set("X-Amz-Meta-A", "  x   y ")
		r.Header.Add("X-Amz-Meta-B", "1")
		r.Header.Add("X-Amz-Meta-B", "2")
	})
	if _, err := v.Verify(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// retarget sends a signed request's headers to another URL.
func retarget(r *http.Request, target string) *http.Request {
	out := httptest.NewRequest(r.Method, target, nil)
	out.Header = r.Header
	return out
}

func TestSigV4Rejects(t *testing.T) {
	v := testVerifier()
	ok := func() *http.Request { return sign(t, http.MethodGet, "/b/k", UnsignedPayload, testNow, nil) }
	tests := []struct {
		name string
		req  func() *http.Request
		code string
	}{
		{"anonymous", func() *http.Request { return httptest.NewRequest(http.MethodGet, "/b/k", nil) }, "AccessDenied"},
		{"tampered path", func() *http.Request { return retarget(ok(), "/b/other") }, "SignatureDoesNotMatch"},
		{"tampered query", func() *http.Request { return retarget(ok(), "/b/k?versionId=1") }, "SignatureDoesNotMatch"},
		{"tampered encoding", func() *http.Request { return retarget(ok(), "/b/%6B") }, ""}, // the same path
		{"tampered plus", func() *http.Request {
			return retarget(sign(t, http.MethodGet, "/b/a+b", UnsignedPayload, testNow, nil), "/b/a%20b")
		}, "SignatureDoesNotMatch"},
		{"tampered header", func() *http.Request {
			r := sign(t, http.MethodGet, "/b/k", UnsignedPayload, testNow, func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") })
			r.Header.Set("Range", "bytes=0-9")
			return r
		}, "SignatureDoesNotMatch"},
		{"tampered payload mode", func() *http.Request { r := ok(); r.Header.Set("X-Amz-Content-Sha256", emptySHA256); return r }, "SignatureDoesNotMatch"},
		{"unknown key", func() *http.Request {
			r := ok()
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), testKey, "AKIDOTHER", 1))
			return r
		}, "InvalidAccessKeyId"},
		{"skewed", func() *http.Request {
			return sign(t, http.MethodGet, "/b/k", UnsignedPayload, testNow.Add(-20*time.Minute), nil)
		}, "RequestTimeTooSkewed"},
		{"no payload hash", func() *http.Request { r := ok(); r.Header.Del("X-Amz-Content-Sha256"); return r }, "InvalidRequest"},
		{"bad payload hash", func() *http.Request {
			return sign(t, http.MethodPut, "/b/k", "not-a-hash", testNow, nil)
		}, "InvalidArgument"},
		{"wrong region", func() *http.Request {
			r := ok()
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/us-east-1/", "/eu-west-1/", 1))
			return r
		}, "AuthorizationHeaderMalformed"},
		{"malformed", func() *http.Request {
			r := ok()
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x")
			return r
		}, "AuthorizationHeaderMalformed"},
		{"sigv2", func() *http.Request { r := ok(); r.Header.Set("Authorization", "AWS AKID:sig"); return r }, "AuthorizationHeaderMalformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tt.req())
			if code(err) != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}
}

func presign(t *testing.T, method, target string, at time.Time, expires time.Duration) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	out := r.Clone(context.Background())
	out.URL.Scheme, out.URL.Host = "http", r.Host
	q := out.URL.Query()
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	out.URL.RawQuery = q.Encode()
	u, _, err := s3Signer().PresignHTTP(context.Background(), creds, out, UnsignedPayload, "s3", "us-east-1", at)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(method, u[strings.Index(u, r.Host)+len(r.Host):], nil)
}

func TestSigV4Presigned(t *testing.T) {
	v := testVerifier()
	r := presign(t, http.MethodGet, "/b/a%20b?response-content-type=text%2Fplain", testNow.Add(-time.Minute), time.Hour)
	res, err := v.Verify(context.Background(), r)
	if err != nil || !res.Presigned || res.PayloadHash != UnsignedPayload {
		t.Fatalf("presigned = %+v, %v", res, err)
	}
	r = presign(t, http.MethodGet, "/b/k", testNow.Add(-2*time.Hour), time.Hour)
	if _, err := v.Verify(context.Background(), r); code(err) != "AccessDenied" || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	r = presign(t, http.MethodGet, "/b/k", testNow.Add(time.Hour), time.Hour)
	if _, err := v.Verify(context.Background(), r); code(err) != "AccessDenied" {
		t.Fatalf("not yet valid: %v", err)
	}
	r = presign(t, http.MethodGet, "/b/k", testNow, 8*24*time.Hour)
	if _, err := v.Verify(context.Background(), r); code(err) != "AuthorizationQueryParametersError" {
		t.Fatalf("too long: %v", err)
	}
	r = presign(t, http.MethodGet, "/b/k", testNow, time.Hour)
	r.URL.RawQuery = strings.Replace(r.URL.RawQuery, "X-Amz-Expires=3600", "X-Amz-Expires=7200", 1)
	if _, err := v.Verify(context.Background(), r); code(err) != "SignatureDoesNotMatch" {
		t.Fatalf("extended expiry: %v", err)
	}
}

// Chunk signatures match the SDK's stream signer, which uses the same
// string to sign as aws-chunked uploads.
func TestSigV4ChunkSignature(t *testing.T) {
	v := testVerifier()
	r := sign(t, http.MethodPut, "/b/k", StreamingSigned, testNow, nil)
	res, err := v.Verify(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	seed := []byte(res.Signature)
	ss := v4.NewStreamSigner(creds, "s3", "us-east-1", mustHex(t, string(seed)))
	prev := res.Signature
	for _, chunk := range [][]byte{[]byte("hello"), make([]byte, 65536), nil} {
		want, err := ss.GetSignature(context.Background(), nil, chunk, testNow)
		if err != nil {
			t.Fatal(err)
		}
		got := res.ChunkSignature(prev, sha(chunk))
		if got != hexs(want) {
			t.Fatalf("chunk signature %s, want %s", got, hexs(want))
		}
		prev = got
	}
}

// FuzzSigV4 checks that malformed authorization data never panics and is
// never accepted.
func FuzzSigV4(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261013/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc", "20261013T120000Z", "/b/k?x=1")
	f.Add("AWS4-HMAC-SHA256 Credential=/////", "", "/")
	f.Add("AWS4-HMAC-SHA256 ,,,=,", "20261013T120000Z", "/%zz")
	v := testVerifier()
	f.Fuzz(func(t *testing.T, authz, date, target string) {
		if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, " \r\n\t#") {
			return
		}
		r, err := http.NewRequest(http.MethodGet, "http://example.com"+target, nil)
		if err != nil {
			return
		}
		r.Header.Set("Authorization", authz)
		r.Header.Set("X-Amz-Date", date)
		r.Header.Set("X-Amz-Content-Sha256", UnsignedPayload)
		if _, err := v.Verify(context.Background(), r); err == nil {
			t.Fatalf("accepted %q %q %q", authz, date, target)
		}
	})
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func hexs(b []byte) string { return hex.EncodeToString(b) }

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
