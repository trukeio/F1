package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// Multipart limits (as in S3).
const (
	maxPartNumber = 10000
	maxPartSize   = 5 << 30
)

type initiateResult struct {
	XMLName  struct{} `xml:"InitiateMultipartUploadResult"`
	NS       string   `xml:"xmlns,attr"`
	Bucket   string
	Key      string
	UploadId string
}

// createUpload starts a multipart upload. Upload state lives in cache.db
// (doc/plan.md, "Multipart internals").
func (s *Server) createUpload(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	if err := validKey(b, q.key); err != nil {
		return err
	}
	meta, err := requestMeta(q.r.Header)
	if err != nil {
		return err
	}
	attrs := cache.UploadAttrs{ContentType: q.r.Header.Get("Content-Type"), Meta: meta}
	if a := q.r.Header.Get("X-Amz-Checksum-Algorithm"); a != "" {
		alg, ok := validAlgorithm(a)
		if !ok {
			return errCode("InvalidRequest", "Checksum algorithm %q is not supported", a)
		}
		attrs.Checksum = alg
		attrs.ChecksumType = Composite
		if alg == CRC64NVME {
			attrs.ChecksumType = FullObject // CRC-64/NVME is full-object only
		}
	}
	if t := q.r.Header.Get("X-Amz-Checksum-Type"); t != "" {
		switch {
		case attrs.Checksum == "":
			return errCode("InvalidRequest", "x-amz-checksum-type needs x-amz-checksum-algorithm")
		case t == FullObject && (attrs.Checksum == SHA1 || attrs.Checksum == SHA256):
			return errCode("InvalidRequest", "The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToUpper(attrs.Checksum))
		case t == Composite && attrs.Checksum == CRC64NVME:
			return errCode("InvalidRequest", "The COMPOSITE checksum type cannot be used with the CRC64NVME checksum algorithm.")
		case t != FullObject && t != Composite:
			return errCode("InvalidRequest", "Invalid x-amz-checksum-type %q", t)
		}
		attrs.ChecksumType = t
	}
	u := cache.Upload{ID: ulid.Make().String(), Ref: b.ref, Key: q.key, Principal: q.caller.Principal, Created: time.Now(), Attrs: attrs}
	if err := b.rp.Store().Cache().CreateUpload(q.r.Context(), u); err != nil {
		return err
	}
	if attrs.Checksum != "" {
		q.w.Header().Set("x-amz-checksum-algorithm", strings.ToUpper(attrs.Checksum))
		q.w.Header().Set("x-amz-checksum-type", attrs.ChecksumType)
	}
	return writeXML(q.w, http.StatusOK, initiateResult{NS: xmlNS, Bucket: b.name, Key: q.key, UploadId: u.ID})
}

// upload returns the open upload of a request, checking that it belongs to
// the bucket and key.
func (s *Server) upload(ctx context.Context, b *bucket, id, key string) (cache.Upload, error) {
	u, err := b.rp.Store().Cache().GetUpload(ctx, id)
	if err != nil {
		return u, err
	}
	if u.Ref != b.ref || u.Key != key {
		return u, errNoSuchUpload
	}
	return u, nil
}

func partNumber(q *request) (int, error) {
	n, err := strconv.Atoi(q.r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > maxPartNumber {
		return 0, errCode("InvalidArgument", "Part number must be an integer between 1 and %d, inclusive", maxPartNumber)
	}
	return n, nil
}

// uploadPart stores one part as a File. The part is acknowledged only once
// its pack is durable.
func (s *Server) uploadPart(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	n, err := partNumber(q)
	if err != nil {
		return err
	}
	u, err := s.upload(ctx, b, q.r.URL.Query().Get("uploadId"), q.key)
	if err != nil {
		return err
	}
	if q.r.ContentLength > maxPartSize {
		return errCode("EntityTooLarge", "Your proposed upload exceeds the maximum allowed part size.")
	}
	body, err := newBody(q.r, q.sig, u.Attrs.Checksum)
	if err != nil {
		return err
	}
	res, err := b.rp.Ingest(ctx, body.r, body.size)
	if err != nil {
		return err
	}
	cks, err := body.verify(res)
	if err != nil {
		return err
	}
	if err := b.rp.Durable(ctx); err != nil {
		return err
	}
	etag := hex.EncodeToString(res.Digests.MD5[:])
	p := cache.Part{Number: n, File: res.File, Size: int64(res.Size), ETag: etag, Created: time.Now()}
	if u.Attrs.Checksum != "" {
		p.Checksum = body.checksum(u.Attrs.Checksum)
	}
	if err := b.rp.Store().Cache().PutPart(ctx, u.ID, p); err != nil {
		return err
	}
	h := q.w.Header()
	h.Set("ETag", `"`+etag+`"`)
	for alg, v := range cks {
		h.Set(checksumHeader(alg), base64.StdEncoding.EncodeToString(v))
	}
	if p.Checksum != nil {
		h.Set(checksumHeader(u.Attrs.Checksum), base64.StdEncoding.EncodeToString(p.Checksum))
	}
	q.w.WriteHeader(http.StatusOK)
	return nil
}

type copyPartResult struct {
	XMLName           struct{} `xml:"CopyPartResult"`
	NS                string   `xml:"xmlns,attr"`
	ETag              string
	LastModified      string
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
}

// uploadPartCopy stores a part copied from a range of an existing object.
func (s *Server) uploadPartCopy(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	n, err := partNumber(q)
	if err != nil {
		return err
	}
	u, err := s.upload(ctx, b, q.r.URL.Query().Get("uploadId"), q.key)
	if err != nil {
		return err
	}
	cs, err := s.copySource(q)
	if err != nil {
		return err
	}
	off, length := int64(0), cs.info.size
	if rg := q.r.Header.Get("X-Amz-Copy-Source-Range"); rg != "" {
		spec, ok := strings.CutPrefix(rg, "bytes=")
		a, z, ok2 := strings.Cut(spec, "-")
		first, err1 := strconv.ParseInt(a, 10, 64)
		last, err2 := strconv.ParseInt(z, 10, 64)
		if !ok || !ok2 || err1 != nil || err2 != nil || first < 0 || last < first {
			return errCode("InvalidArgument", "The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy")
		}
		if last >= cs.info.size {
			return errCode("InvalidRange", "The requested range is not satisfiable")
		}
		off, length = first, last-first+1
	}
	if length > maxPartSize {
		return errCode("EntityTooLarge", "Your proposed upload exceeds the maximum allowed part size.")
	}
	// The part needs its MD5 (ETag) and the upload's checksum: ingest the
	// range. A whole object in the same repository could reuse its File,
	// but its digests would still have to be read.
	rd, err := cs.b.rp.OpenFile(ctx, cs.info.entry.Target)
	if err != nil {
		return err
	}
	if _, err := rd.Seek(off, io.SeekStart); err != nil {
		return err
	}
	var in io.Reader = io.LimitReader(rd, length)
	sum := newChecksum(u.Attrs.Checksum)
	if sum != nil {
		in = io.TeeReader(in, sum)
	}
	res, err := b.rp.Ingest(ctx, in, length)
	if err != nil {
		return err
	}
	if err := b.rp.Durable(ctx); err != nil {
		return err
	}
	etag := hex.EncodeToString(res.Digests.MD5[:])
	now := time.Now()
	p := cache.Part{Number: n, File: res.File, Size: int64(res.Size), ETag: etag, Created: now}
	out := copyPartResult{NS: xmlNS, ETag: `"` + etag + `"`, LastModified: s3Time(now)}
	if sum != nil {
		p.Checksum = sum.Sum(nil)
		v := base64.StdEncoding.EncodeToString(p.Checksum)
		switch u.Attrs.Checksum {
		case CRC32:
			out.ChecksumCRC32 = v
		case CRC32C:
			out.ChecksumCRC32C = v
		case CRC64NVME:
			out.ChecksumCRC64NVME = v
		case SHA1:
			out.ChecksumSHA1 = v
		case SHA256:
			out.ChecksumSHA256 = v
		}
	}
	if err := b.rp.Store().Cache().PutPart(ctx, u.ID, p); err != nil {
		return err
	}
	if cs.version != "" {
		q.w.Header().Set("x-amz-copy-source-version-id", cs.version)
	}
	return writeXML(q.w, http.StatusOK, out)
}

type completePart struct {
	PartNumber        int
	ETag              string
	ChecksumCRC32     string
	ChecksumCRC32C    string
	ChecksumCRC64NVME string
	ChecksumSHA1      string
	ChecksumSHA256    string
}

func (p *completePart) checksum(alg string) string {
	switch alg {
	case CRC32:
		return p.ChecksumCRC32
	case CRC32C:
		return p.ChecksumCRC32C
	case CRC64NVME:
		return p.ChecksumCRC64NVME
	case SHA1:
		return p.ChecksumSHA1
	case SHA256:
		return p.ChecksumSHA256
	}
	return ""
}

type completeRequest struct {
	Parts []completePart `xml:"Part"`
}

type completeResult struct {
	XMLName           struct{} `xml:"CompleteMultipartUploadResult"`
	NS                string   `xml:"xmlns,attr"`
	Location          string
	Bucket            string
	Key               string
	ETag              string
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
	ChecksumType      string `xml:",omitempty"`
}

// completeUpload assembles the object from its parts: the new File's chunk
// list is the concatenation of the parts' chunk lists (no data is copied),
// its ETag is the MD5 of the part MD5s followed by "-<parts>", and its
// DigestRecord is computed in the background.
func (s *Server) completeUpload(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	id := q.r.URL.Query().Get("uploadId")
	u, err := s.upload(ctx, b, id, q.key)
	if errors.Is(err, cache.ErrNoUpload) {
		// A repeated completion gets the first one's result, as in S3.
		if done, cerr := b.rp.Store().Cache().GetCompleted(ctx, id); cerr == nil && done.Ref == b.ref && done.Key == q.key {
			return s.writeComplete(q, b, done)
		}
	}
	if err != nil {
		return err
	}
	var req completeRequest
	if err := readXML(q.r, &req); err != nil {
		return err
	}
	if len(req.Parts) == 0 {
		return errCode("MalformedXML", "You must specify at least one part")
	}
	stored, err := b.rp.Store().Cache().Parts(ctx, u.ID, 0, 0)
	if err != nil {
		return err
	}
	byNumber := map[int]cache.Part{}
	for _, p := range stored {
		byNumber[p.Number] = p
	}
	invalidPart := errCode("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.")
	var (
		files []hash.Hash
		md5s  bytes.Buffer
		sums  [][]byte
		sizes []int64
	)
	// Order and existence of every part first, then sizes.
	for i, rp := range req.Parts {
		if i > 0 && rp.PartNumber <= req.Parts[i-1].PartNumber {
			return errCode("InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.")
		}
		p, ok := byNumber[rp.PartNumber]
		if !ok || strings.Trim(rp.ETag, `"`) != p.ETag {
			return invalidPart
		}
	}
	for i, rp := range req.Parts {
		p := byNumber[rp.PartNumber]
		if i < len(req.Parts)-1 && p.Size < s.Config.MinPartSize {
			return errCode("EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
		}
		if u.Attrs.Checksum != "" {
			if v := rp.checksum(u.Attrs.Checksum); v != "" && v != base64.StdEncoding.EncodeToString(p.Checksum) {
				return invalidPart
			}
			sums = append(sums, p.Checksum)
		}
		raw, _ := hex.DecodeString(p.ETag)
		md5s.Write(raw)
		files = append(files, p.File)
		sizes = append(sizes, p.Size)
	}
	sum := md5.Sum(md5s.Bytes())
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:]), len(req.Parts))
	var cks map[string][]byte
	ckValue, ckType := "", ""
	if alg := u.Attrs.Checksum; alg != "" {
		if u.Attrs.ChecksumType == FullObject {
			v, ok := combineFullObject(alg, sums, sizes)
			if !ok {
				return errCode("InvalidRequest", "FULL_OBJECT checksums need a CRC algorithm")
			}
			cks = map[string][]byte{alg: v}
			ckValue, ckType = base64.StdEncoding.EncodeToString(v), FullObject
		} else {
			v := composite(alg, sums)
			cks = map[string][]byte{fmt.Sprintf("%s-%d", alg, len(sums)): v}
			ckValue, ckType = fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(v), len(sums)), Composite
		}
	}
	// A checksum of the whole object sent with the completion must match.
	if alg := u.Attrs.Checksum; alg != "" {
		if v := q.r.Header.Get(checksumHeader(alg)); v != "" && v != ckValue && v+"-"+fmt.Sprint(len(sums)) != ckValue {
			return errCode("BadDigest", "The %s you specified did not match the calculated checksum.", checksumHeader(alg))
		}
	}
	pre, err := s.writePrecondition(ctx, b, q.r.Header)
	if err != nil {
		return err
	}
	file, _, err := b.rp.Concat(ctx, files)
	if err != nil {
		return err
	}
	attrs := &object.Attrs{ContentType: u.Attrs.ContentType, Meta: u.Attrs.Meta, ETag: etag, MTime: time.Now().UnixNano(), Checksums: cks}
	ch := repo.Change{Op: repo.OpPut, Path: q.key, Entry: object.Entry{Kind: object.KindFile, Target: file, Attrs: attrs}}
	commit, err := s.commit(ctx, b, q.caller, []repo.Change{ch}, pre, "s3: CompleteMultipartUpload "+q.key)
	if err != nil {
		return err
	}
	// The object is committed; the part Files are now reachable from it.
	done := cache.CompletedUpload{ID: u.ID, Ref: b.ref, Key: q.key, ETag: etag, CksAlg: u.Attrs.Checksum, Cks: ckValue, CksType: ckType}
	if b.versioned() {
		done.Version = versionID(commit)
	}
	if err := b.rp.Store().Cache().CompleteUpload(ctx, done); err != nil && !errors.Is(err, cache.ErrNoUpload) {
		s.Log.Warn("recording completed upload", "upload", u.ID, "err", err)
	}
	b.rp.DigestLater(file)
	return s.writeComplete(q, b, done)
}

func (s *Server) writeComplete(q *request, b *bucket, done cache.CompletedUpload) error {
	if done.Version != "" {
		q.w.Header().Set("x-amz-version-id", done.Version)
	}
	res := completeResult{NS: xmlNS, Location: "/" + b.name + "/" + q.key, Bucket: b.name, Key: q.key, ETag: `"` + done.ETag + `"`, ChecksumType: done.CksType}
	switch done.CksAlg {
	case CRC32:
		res.ChecksumCRC32 = done.Cks
	case CRC32C:
		res.ChecksumCRC32C = done.Cks
	case CRC64NVME:
		res.ChecksumCRC64NVME = done.Cks
	case SHA1:
		res.ChecksumSHA1 = done.Cks
	case SHA256:
		res.ChecksumSHA256 = done.Cks
	}
	return writeXML(q.w, http.StatusOK, res)
}

func (s *Server) abortUpload(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	u, err := s.upload(ctx, b, q.r.URL.Query().Get("uploadId"), q.key)
	if err != nil {
		return err
	}
	if err := b.rp.Store().Cache().DeleteUpload(ctx, u.ID); err != nil {
		return err
	}
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

type partXML struct {
	PartNumber        int
	LastModified      string
	ETag              string
	Size              int64
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
}

type listPartsResult struct {
	XMLName              struct{} `xml:"ListPartsResult"`
	NS                   string   `xml:"xmlns,attr"`
	Bucket               string
	Key                  string
	UploadId             string
	Initiator            ownerXML
	Owner                ownerXML
	StorageClass         string
	PartNumberMarker     int
	NextPartNumberMarker int
	MaxParts             int
	IsTruncated          bool
	ChecksumAlgorithm    string    `xml:",omitempty"`
	ChecksumType         string    `xml:",omitempty"`
	Parts                []partXML `xml:"Part"`
}

func (s *Server) listParts(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite) || b.g.Allow(q.key, serverdb.PermRead)); err != nil {
		return err
	}
	query := q.r.URL.Query()
	u, err := s.upload(ctx, b, query.Get("uploadId"), q.key)
	if err != nil {
		return err
	}
	max, err := maxKeys(query, "max-parts")
	if err != nil {
		return err
	}
	marker := 0
	if v := query.Get("part-number-marker"); v != "" {
		if marker, err = strconv.Atoi(v); err != nil || marker < 0 {
			return errCode("InvalidArgument", "invalid part-number-marker")
		}
	}
	parts, err := b.rp.Store().Cache().Parts(ctx, u.ID, marker, max+1)
	if err != nil {
		return err
	}
	res := listPartsResult{NS: xmlNS, Bucket: b.name, Key: q.key, UploadId: u.ID, Initiator: owner(u.Principal), Owner: owner(u.Principal),
		StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: max, Parts: []partXML{}}
	if u.Attrs.Checksum != "" {
		res.ChecksumAlgorithm, res.ChecksumType = strings.ToUpper(u.Attrs.Checksum), u.Attrs.ChecksumType
	}
	if len(parts) > max {
		parts, res.IsTruncated = parts[:max], true
	}
	for _, p := range parts {
		px := partXML{PartNumber: p.Number, LastModified: s3Time(p.Created), ETag: `"` + p.ETag + `"`, Size: p.Size}
		if p.Checksum != nil {
			v := base64.StdEncoding.EncodeToString(p.Checksum)
			switch u.Attrs.Checksum {
			case CRC32:
				px.ChecksumCRC32 = v
			case CRC32C:
				px.ChecksumCRC32C = v
			case CRC64NVME:
				px.ChecksumCRC64NVME = v
			case SHA1:
				px.ChecksumSHA1 = v
			case SHA256:
				px.ChecksumSHA256 = v
			}
		}
		res.Parts = append(res.Parts, px)
		res.NextPartNumberMarker = p.Number
	}
	return writeXML(q.w, http.StatusOK, res)
}

type uploadXML struct {
	Key               string
	UploadId          string
	Initiator         ownerXML
	Owner             ownerXML
	StorageClass      string
	Initiated         string
	ChecksumAlgorithm string `xml:",omitempty"`
	ChecksumType      string `xml:",omitempty"`
}

type listUploadsResult struct {
	XMLName            struct{} `xml:"ListMultipartUploadsResult"`
	NS                 string   `xml:"xmlns,attr"`
	Bucket             string
	KeyMarker          string
	UploadIdMarker     string
	NextKeyMarker      string
	NextUploadIdMarker string
	Prefix             string
	Delimiter          string `xml:",omitempty"`
	MaxUploads         int
	IsTruncated        bool
	Uploads            []uploadXML `xml:"Upload"`
	CommonPrefixes     []prefixXML `xml:"CommonPrefixes"`
}

func (s *Server) listUploads(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Any(serverdb.PermWrite) || b.g.Any(serverdb.PermRead)); err != nil {
		return err
	}
	query := q.r.URL.Query()
	max, err := maxKeys(query, "max-uploads")
	if err != nil {
		return err
	}
	prefix, delim := query.Get("prefix"), query.Get("delimiter")
	keyMarker, idMarker := query.Get("key-marker"), query.Get("upload-id-marker")
	if idMarker == "" {
		// Without an upload id marker, the listing starts after every upload
		// of the key marker.
		idMarker = "\U0010FFFF"
	}
	res := listUploadsResult{NS: xmlNS, Bucket: b.name, KeyMarker: keyMarker, UploadIdMarker: query.Get("upload-id-marker"),
		Prefix: prefix, Delimiter: delim, MaxUploads: max, Uploads: []uploadXML{}}
	lastPrefix := ""
	for n := 0; n < max; {
		ups, err := b.rp.Store().Cache().Uploads(ctx, b.ref, prefix, keyMarker, idMarker, 1000)
		if err != nil {
			return err
		}
		if len(ups) == 0 {
			break
		}
		for _, u := range ups {
			keyMarker, idMarker = u.Key, u.ID
			if !b.g.Allow(u.Key, serverdb.PermRead) && !b.g.Allow(u.Key, serverdb.PermWrite) {
				continue
			}
			if delim != "" {
				if i := strings.Index(u.Key[len(prefix):], delim); i >= 0 {
					cp := u.Key[:len(prefix)+i+len(delim)]
					if cp == lastPrefix {
						continue
					}
					if n == max {
						res.IsTruncated = true
						break
					}
					lastPrefix = cp
					res.CommonPrefixes = append(res.CommonPrefixes, prefixXML{Prefix: cp})
					res.NextKeyMarker, res.NextUploadIdMarker = u.Key, u.ID
					n++
					continue
				}
			}
			if n == max {
				res.IsTruncated = true
				break
			}
			ux := uploadXML{Key: u.Key, UploadId: u.ID, Initiator: owner(u.Principal), Owner: owner(u.Principal),
				StorageClass: "STANDARD", Initiated: s3Time(u.Created)}
			if u.Attrs.Checksum != "" {
				ux.ChecksumAlgorithm, ux.ChecksumType = strings.ToUpper(u.Attrs.Checksum), u.Attrs.ChecksumType
			}
			res.Uploads = append(res.Uploads, ux)
			res.NextKeyMarker, res.NextUploadIdMarker = u.Key, u.ID
			n++
		}
		if res.IsTruncated {
			break
		}
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextUploadIdMarker = "", ""
	}
	return writeXML(q.w, http.StatusOK, res)
}

// ExpireUploads aborts multipart uploads older than the configured expiry
// in every repository, releasing their parts (they stop being GC roots).
func (s *Server) ExpireUploads(ctx context.Context) {
	s.init()
	before := time.Now().Add(-s.Config.UploadExpiry)
	for _, ns := range s.Repos.Names() {
		rp, err := s.Repos.Get(ns)
		if err != nil {
			continue
		}
		ids, err := rp.Store().Cache().ExpiredUploads(ctx, before)
		if err != nil {
			s.Log.Warn("listing expired uploads", "namespace", ns, "err", err)
			continue
		}
		for _, id := range ids {
			if err := rp.Store().Cache().DeleteUpload(ctx, id); err == nil {
				s.Log.Info("multipart upload expired", "namespace", ns, "upload", id)
			}
		}
		if err := rp.Store().Cache().ForgetCompleted(ctx, before); err != nil {
			s.Log.Warn("forgetting completed uploads", "namespace", ns, "err", err)
		}
	}
}

// Run expires multipart uploads periodically until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.ExpireUploads(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
