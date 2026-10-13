package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SigV4 payload hash values (x-amz-content-sha256) other than a hex SHA-256.
const (
	UnsignedPayload        = "UNSIGNED-PAYLOAD"
	StreamingSigned        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedTrail = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	// MaxPresignExpiry is the longest validity of a presigned URL (7 days,
	// as in AWS).
	MaxPresignExpiry = 7 * 24 * time.Hour
	// DefaultMaxSkew is the allowed clock difference for signed requests.
	DefaultMaxSkew = 15 * time.Minute
	emptySHA256    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// SigV4Error is a SigV4 verification failure with its S3 error code.
type SigV4Error struct {
	Code    string // S3 error code, for example SignatureDoesNotMatch
	Message string
}

func (e *SigV4Error) Error() string { return e.Code + ": " + e.Message }

func sigErr(code, format string, args ...any) error {
	return &SigV4Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrUnknownAccessKey is returned by a SigV4 secret lookup for an unknown or
// revoked access key.
var ErrUnknownAccessKey = errors.New("auth: unknown access key")

// SigV4 verifies AWS Signature Version 4 signed requests (header and
// presigned query forms).
type SigV4 struct {
	Region  string // the only accepted region in credential scopes
	Service string // "s3"
	// Secret returns the secret of an access key, or ErrUnknownAccessKey.
	Secret  func(ctx context.Context, accessKey string) (string, error)
	Now     func() time.Time // default time.Now
	MaxSkew time.Duration    // default DefaultMaxSkew
}

// SigV4Result describes a verified request. The payload itself is verified
// by the caller according to PayloadHash: a hex SHA-256 against the body,
// UNSIGNED-PAYLOAD not at all, and the STREAMING-* modes chunk by chunk with
// ChunkSignature and TrailerSignature.
type SigV4Result struct {
	AccessKey   string
	PayloadHash string
	Presigned   bool
	Signature   string // the request signature: the seed of chunk signatures
	date        string // x-amz-date
	scope       string // date/region/service/aws4_request
	key         []byte // derived signing key
}

// Verify checks the signature of r. It does not read the body.
func (v *SigV4) Verify(ctx context.Context, r *http.Request) (*SigV4Result, error) {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	skew := v.MaxSkew
	if skew == 0 {
		skew = DefaultMaxSkew
	}
	var (
		cred, signedHeaders, signature, date, payload string
		presigned                                     bool
		expires                                       time.Duration
	)
	q := r.URL.Query()
	if authz := r.Header.Get("Authorization"); authz != "" {
		var err error
		if cred, signedHeaders, signature, err = parseAuthorization(authz); err != nil {
			return nil, err
		}
		date = r.Header.Get("X-Amz-Date")
		if date == "" {
			if d, err := http.ParseTime(r.Header.Get("Date")); err == nil {
				date = d.UTC().Format(amzDateFormat)
			}
		}
		payload = r.Header.Get("X-Amz-Content-Sha256")
		if payload == "" {
			return nil, sigErr("InvalidRequest", "missing required header x-amz-content-sha256")
		}
	} else if q.Get("X-Amz-Algorithm") != "" {
		presigned = true
		if q.Get("X-Amz-Algorithm") != sigV4Algorithm {
			return nil, sigErr("AuthorizationQueryParametersError", "X-Amz-Algorithm must be %s", sigV4Algorithm)
		}
		cred, signedHeaders, signature, date = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		if cred == "" || signedHeaders == "" || signature == "" || date == "" {
			return nil, sigErr("AuthorizationQueryParametersError", "presigned URL lacks X-Amz-Credential, X-Amz-SignedHeaders, X-Amz-Signature or X-Amz-Date")
		}
		secs, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
		if err == nil && secs < 1 {
			return nil, sigErr("AccessDenied", "request has expired")
		}
		if err != nil || time.Duration(secs)*time.Second > MaxPresignExpiry {
			return nil, sigErr("AuthorizationQueryParametersError", "X-Amz-Expires must be 1 to %d seconds", int64(MaxPresignExpiry/time.Second))
		}
		expires = time.Duration(secs) * time.Second
		payload = UnsignedPayload
		if p := r.Header.Get("X-Amz-Content-Sha256"); p != "" {
			payload = p
		}
	} else {
		return nil, sigErr("AccessDenied", "anonymous access is not allowed")
	}

	accessKey, scopeDate, region, service, err := parseCredential(cred)
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(amzDateFormat, date)
	if err != nil {
		return nil, sigErr("AccessDenied", "missing or invalid X-Amz-Date")
	}
	if scopeDate != date[:8] {
		return nil, sigErr("SignatureDoesNotMatch", "credential date %s does not match X-Amz-Date %s", scopeDate, date)
	}
	if region != v.Region {
		return nil, sigErr("AuthorizationHeaderMalformed", "the region '%s' is wrong; expecting '%s'", region, v.Region)
	}
	if service != v.Service {
		return nil, sigErr("AuthorizationHeaderMalformed", "the service '%s' is wrong; expecting '%s'", service, v.Service)
	}
	n := now()
	if presigned {
		if t.After(n.Add(skew)) {
			return nil, sigErr("AccessDenied", "request is not valid yet")
		}
		if n.After(t.Add(expires)) {
			return nil, sigErr("AccessDenied", "request has expired")
		}
	} else if d := n.Sub(t); d > skew || d < -skew {
		return nil, sigErr("RequestTimeTooSkewed", "the difference between the request time and the server's time is too large")
	}
	switch payload {
	case UnsignedPayload, StreamingSigned, StreamingSignedTrailer, StreamingUnsignedTrail:
	default:
		if b, err := hex.DecodeString(payload); err != nil || len(b) != sha256.Size {
			return nil, sigErr("InvalidArgument", "x-amz-content-sha256 must be a hex SHA-256 or a known payload mode")
		}
	}
	if presigned && strings.HasPrefix(payload, "STREAMING-") {
		return nil, sigErr("InvalidArgument", "streaming payloads need header authentication")
	}

	headers := strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(headers) || !contains(headers, "host") {
		return nil, sigErr("AuthorizationHeaderMalformed", "signed headers must be sorted and include host")
	}
	uris, err := canonicalURIs(r)
	if err != nil {
		return nil, err
	}
	secret, err := v.Secret(ctx, accessKey)
	if err != nil {
		if errors.Is(err, ErrUnknownAccessKey) {
			return nil, sigErr("InvalidAccessKeyId", "the access key does not exist")
		}
		return nil, err
	}
	scope := strings.Join([]string{scopeDate, region, service, "aws4_request"}, "/")
	key := signingKey(secret, scopeDate, region, service)
	rest := "\n" + canonicalQuery(r.URL.RawQuery, presigned) + "\n" + canonicalHeaders(r, headers) + "\n" + signedHeaders + "\n" + payload
	match := false
	for _, uri := range uris {
		creq := r.Method + "\n" + uri + rest
		sts := sigV4Algorithm + "\n" + date + "\n" + scope + "\n" + hexSHA256([]byte(creq))
		want := hex.EncodeToString(hmacSHA256(key, []byte(sts)))
		if hmac.Equal([]byte(want), []byte(signature)) {
			match = true
			break
		}
	}
	if !match {
		return nil, sigErr("SignatureDoesNotMatch", "the request signature does not match")
	}
	return &SigV4Result{AccessKey: accessKey, PayloadHash: payload, Presigned: presigned, Signature: signature,
		date: date, scope: scope, key: key}, nil
}

// ChunkSignature returns the expected signature of an aws-chunked chunk
// whose data has SHA-256 dataHash, given the previous chunk's signature
// (the request signature for the first chunk).
func (s *SigV4Result) ChunkSignature(prev string, dataHash []byte) string {
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + s.date + "\n" + s.scope + "\n" + prev + "\n" + emptySHA256 + "\n" + hex.EncodeToString(dataHash)
	return hex.EncodeToString(hmacSHA256(s.key, []byte(sts)))
}

// TrailerSignature returns the expected signature of aws-chunked trailers
// whose canonical text ("name:value\n" per trailer) has SHA-256 textHash.
func (s *SigV4Result) TrailerSignature(prev string, textHash []byte) string {
	sts := "AWS4-HMAC-SHA256-TRAILER\n" + s.date + "\n" + s.scope + "\n" + prev + "\n" + hex.EncodeToString(textHash)
	return hex.EncodeToString(hmacSHA256(s.key, []byte(sts)))
}

func parseAuthorization(h string) (cred, signedHeaders, signature string, err error) {
	rest, ok := strings.CutPrefix(h, sigV4Algorithm+" ")
	if !ok {
		return "", "", "", sigErr("AuthorizationHeaderMalformed", "only %s authorization is supported", sigV4Algorithm)
	}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signedHeaders = v
		case "Signature":
			signature = v
		}
	}
	if cred == "" || signedHeaders == "" || signature == "" {
		return "", "", "", sigErr("AuthorizationHeaderMalformed", "authorization header lacks Credential, SignedHeaders or Signature")
	}
	return cred, signedHeaders, signature, nil
}

func parseCredential(cred string) (accessKey, date, region, service string, err error) {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" || len(parts[1]) != 8 {
		return "", "", "", "", sigErr("AuthorizationHeaderMalformed", "credential must be <key>/<date>/<region>/<service>/aws4_request")
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

// canonicalURIs returns the candidate canonical URIs of a request: the
// path re-encoded segment by segment with the AWS URI encoding (what SDKs
// sign, whatever the client escaped), and the path exactly as received
// (clients that sign the path they send, such as one with a literal "+").
func canonicalURIs(r *http.Request) ([]string, error) {
	p := r.URL.EscapedPath()
	if p == "" {
		return []string{"/"}, nil
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, sigErr("InvalidURI", "invalid path encoding")
		}
		segs[i] = awsEscape(u)
	}
	uris := []string{strings.Join(segs, "/")}
	raw := p
	if r.RequestURI != "" {
		raw, _, _ = strings.Cut(r.RequestURI, "?")
	}
	if raw != uris[0] && strings.HasPrefix(raw, "/") {
		uris = append(uris, raw)
	}
	return uris, nil
}

func canonicalQuery(raw string, presigned bool) string {
	type kv struct{ k, v string }
	var pairs []kv
	for _, p := range strings.Split(raw, "&") {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		uk, err1 := url.QueryUnescape(k)
		uv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			uk, uv = k, v
		}
		if presigned && uk == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, kv{awsEscape(uk), awsEscape(uv)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func canonicalHeaders(r *http.Request, names []string) string {
	var b strings.Builder
	for _, name := range names {
		var vals []string
		switch name {
		case "host":
			vals = []string{r.Host}
		case "content-length":
			vals = r.Header.Values("Content-Length")
			if len(vals) == 0 && r.ContentLength >= 0 {
				vals = []string{strconv.FormatInt(r.ContentLength, 10)}
			}
		case "transfer-encoding":
			vals = r.TransferEncoding
		default:
			vals = r.Header.Values(name)
		}
		// Values returns the header's own slice: build a copy.
		trimmed := make([]string, len(vals))
		for i, v := range vals {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(trimmed, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

// awsEscape is the SigV4 URI encoding: every byte except A-Z a-z 0-9 - . _ ~
// is percent-encoded with upper-case hex.
func awsEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
