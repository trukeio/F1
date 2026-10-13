package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/content"
)

// maxObjectSize is the S3 limit for a single PUT (5 GiB); larger objects
// use multipart upload.
const maxObjectSize = 5 << 30

// body is a request body being ingested: the decoded content stream, and
// what must be checked once it has been read (doc/plan.md,
// "Authentication and integrity"). Every check runs after ingest and before
// the commit, so a body that fails one is stored but never referenced.
type body struct {
	r       io.Reader
	size    int64 // decoded length, -1 if unknown
	sig     *auth.SigV4Result
	chunked *chunkedReader
	md5     []byte               // Content-MD5
	sent    map[string][]byte    // checksums sent as headers
	trailer string               // checksum algorithm sent as a trailer
	hashers map[string]hash.Hash // checksums computed over the content
	extra   string               // checksum algorithm the caller needs (multipart)
}

// newBody prepares the request body. extraAlg names a checksum to compute
// even if the client sends none (the upload's algorithm for parts).
func newBody(r *http.Request, sig *auth.SigV4Result, extraAlg string) (*body, error) {
	b := &body{sig: sig, hashers: map[string]hash.Hash{}, extra: extraAlg}
	if v := r.Header.Get("Content-MD5"); v != "" {
		m, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(m) != md5.Size {
			return nil, errCode("InvalidDigest", "The Content-MD5 you specified was invalid.")
		}
		b.md5 = m
	}
	var err error
	if b.sent, b.trailer, err = requestChecksums(r.Header); err != nil {
		return nil, err
	}
	var in io.Reader = r.Body
	b.size = r.ContentLength
	switch sig.PayloadHash {
	case auth.StreamingSigned, auth.StreamingSignedTrailer, auth.StreamingUnsignedTrail:
		var s *auth.SigV4Result
		if sig.PayloadHash != auth.StreamingUnsignedTrail {
			s = sig
		}
		trailer := sig.PayloadHash != auth.StreamingSigned
		if trailer && b.trailer == "" {
			return nil, errCode("InvalidRequest", "a trailer payload mode needs the x-amz-trailer header")
		}
		b.chunked = newChunkedReader(r.Body, s, trailer)
		in = b.chunked
		b.size = -1
		if d := r.Header.Get("X-Amz-Decoded-Content-Length"); d != "" {
			if b.size, err = strconv.ParseInt(d, 10, 64); err != nil || b.size < 0 {
				return nil, errCode("InvalidArgument", "invalid x-amz-decoded-content-length")
			}
		}
	default:
		if b.trailer != "" {
			return nil, errCode("InvalidRequest", "trailers need an aws-chunked payload")
		}
		if b.size < 0 && r.Header.Get("Transfer-Encoding") == "" && len(r.TransferEncoding) == 0 {
			return nil, errCode("MissingContentLength", "You must provide the Content-Length HTTP header.")
		}
	}
	for alg := range b.sent {
		b.hashers[alg] = newChecksum(alg)
	}
	if b.trailer != "" {
		b.hashers[b.trailer] = newChecksum(b.trailer)
	}
	if extraAlg != "" && b.hashers[extraAlg] == nil {
		b.hashers[extraAlg] = newChecksum(extraAlg)
	}
	if len(b.hashers) > 0 {
		ws := make([]io.Writer, 0, len(b.hashers))
		for _, h := range b.hashers {
			ws = append(ws, h)
		}
		in = io.TeeReader(in, io.MultiWriter(ws...))
	}
	b.r = in
	return b, nil
}

// verify checks the ingested content against the payload hash, Content-MD5
// and checksums, and returns the checksums to store (attrs.cks): those the
// client sent, which are verified.
func (b *body) verify(res content.Result) (map[string][]byte, error) {
	if p := b.sig.PayloadHash; !strings.HasPrefix(p, "STREAMING-") && p != auth.UnsignedPayload {
		if !strings.EqualFold(p, hex.EncodeToString(res.Digests.SHA256[:])) {
			return nil, errCode("XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
		}
	}
	if b.md5 != nil && !bytes.Equal(b.md5, res.Digests.MD5[:]) {
		return nil, errCode("BadDigest", "The Content-MD5 you specified did not match what we received.")
	}
	cks := map[string][]byte{}
	for alg, want := range b.sent {
		got := b.hashers[alg].Sum(nil)
		if !bytes.Equal(got, want) {
			return nil, errCode("BadDigest", "The %s you specified did not match the calculated checksum.", checksumHeader(alg))
		}
		cks[alg] = got
	}
	if b.trailer != "" {
		v, ok := b.chunked.Trailers[checksumHeader(b.trailer)]
		if !ok {
			return nil, errCode("InvalidRequest", "the declared trailer %s was not sent", checksumHeader(b.trailer))
		}
		want, err := base64.StdEncoding.DecodeString(v)
		got := b.hashers[b.trailer].Sum(nil)
		if err != nil || !bytes.Equal(got, want) {
			return nil, errCode("BadDigest", "The %s you specified did not match the calculated checksum.", checksumHeader(b.trailer))
		}
		cks[b.trailer] = got
	}
	return cks, nil
}

// checksum returns the computed checksum of an algorithm (nil if it was
// not computed).
func (b *body) checksum(alg string) []byte {
	if h := b.hashers[alg]; h != nil {
		return h.Sum(nil)
	}
	return nil
}
