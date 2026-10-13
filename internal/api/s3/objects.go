package s3

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// maxKeyLength is the S3 key length limit in bytes.
const maxKeyLength = 1024

// maxMetaSize is the S3 limit for user metadata (names and values).
const maxMetaSize = 2 << 10

// defaultContentType is what S3 returns for objects stored without one.
const defaultContentType = "binary/octet-stream"

// storedHeaders are standard headers kept with an object, in attrs.meta
// under ":<lower-case name>" (a name no x-amz-meta- header can produce).
var storedHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// objInfo is what S3 reports about an object version.
type objInfo struct {
	entry        object.Entry
	size         int64
	etag         string // unquoted
	lastModified time.Time
}

func (o *objInfo) attrs() *object.Attrs {
	if o.entry.Attrs == nil {
		return &object.Attrs{}
	}
	return o.entry.Attrs
}

// readable reports whether an entry is an object S3 can serve: everything
// with content (files, executables, LFS files, symlinks as their text).
func readable(e *object.Entry) bool {
	return e != nil && e.Kind != object.KindDir && e.Kind != object.KindGitlink
}

// etag returns an entry's ETag: attrs.etag, or the hex MD5 of the content
// from its DigestRecord.
func (s *Server) etag(ctx context.Context, rp *repo.Repo, e *object.Entry) (string, error) {
	if e.Attrs != nil && e.Attrs.ETag != "" {
		return e.Attrs.ETag, nil
	}
	d, err := rp.Digests(ctx, e.Target)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(d.MD5[:]), nil
}

// info completes the description of an entry at commit.
func (s *Server) info(ctx context.Context, b *bucket, commit hash.Hash, key string, e object.Entry) (objInfo, error) {
	o := objInfo{entry: e}
	size, err := b.rp.FileSize(ctx, e.Target)
	if err != nil {
		return o, err
	}
	o.size = int64(size)
	if o.etag, err = s.etag(ctx, b.rp, &e); err != nil {
		return o, err
	}
	if e.Attrs != nil && e.Attrs.MTime != 0 {
		o.lastModified = time.Unix(0, e.Attrs.MTime)
	} else {
		lm, err := s.lastModified(ctx, b.rp, commit, []string{key})
		if err != nil {
			return o, err
		}
		o.lastModified = lm[key]
	}
	return o, nil
}

// lastModified returns the commit times at which paths were last changed,
// for entries without attrs.mtime (written by Git or the native API).
func (s *Server) lastModified(ctx context.Context, rp *repo.Repo, commit hash.Hash, paths []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	var missing []string
	for _, p := range paths {
		if t, ok := s.lastMod.get(rp, commit, p); ok {
			out[p] = t
		} else {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	found, err := rp.LastModified(ctx, commit, missing)
	if err != nil {
		return nil, err
	}
	for p, t := range found {
		out[p] = t
		s.lastMod.put(rp, commit, p, t)
	}
	return out, nil
}

// lastModCache caches LastModified results by (repository, commit, path).
type lastModCache struct {
	mu  sync.Mutex
	max int
	m   map[lastModKey]time.Time
}

type lastModKey struct {
	rp     *repo.Repo
	commit hash.Hash
	path   string
}

func newLastModCache(max int) *lastModCache {
	return &lastModCache{max: max, m: map[lastModKey]time.Time{}}
}

func (c *lastModCache) get(rp *repo.Repo, commit hash.Hash, p string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[lastModKey{rp, commit, p}]
	return t, ok
}

func (c *lastModCache) put(rp *repo.Repo, commit hash.Hash, p string, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		c.m = map[lastModKey]time.Time{}
	}
	c.m[lastModKey{rp, commit, p}] = t
}

// --- reads ---

func (s *Server) getObject(q *request, head bool) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermRead)); err != nil {
		return err
	}
	query := q.r.URL.Query()
	commit := b.head()
	versionID := ""
	if v := query.Get("versionId"); v != "" {
		if commit, err = s.resolveVersion(ctx, b, q.key, v); err != nil {
			return err
		}
		versionID = v
	}
	rd, e, err := b.rp.Open(ctx, commit, q.key)
	if err != nil {
		if versionID != "" && errors.Is(err, repo.ErrNotFound) {
			return s.missingVersion(ctx, b, commit, q.key, versionID)
		}
		return keyErr(err, q.key)
	}
	if !readable(&e) {
		return keyErr(repo.ErrNotFound, q.key)
	}
	o, err := s.info(ctx, b, commit, q.key, e)
	if err != nil {
		return err
	}
	h := q.w.Header()
	setObjectHeaders(h, &o)
	if versionID != "" {
		h.Set("x-amz-version-id", versionID)
	}
	if err := checkConditions(q.r.Header, "", &o); err != nil {
		return err
	}
	if pn := query.Get("partNumber"); pn != "" {
		if pn != "1" || strings.Contains(o.etag, "-") {
			return notImplemented("GetObject by part number of a multipart object")
		}
	}
	for _, p := range []struct{ param, header string }{
		{"response-content-type", "Content-Type"}, {"response-content-language", "Content-Language"},
		{"response-expires", "Expires"}, {"response-cache-control", "Cache-Control"},
		{"response-content-disposition", "Content-Disposition"}, {"response-content-encoding", "Content-Encoding"},
	} {
		if v := query.Get(p.param); v != "" {
			h.Set(p.header, v)
		}
	}

	start, length, partial, err := parseRange(q.r.Header.Get("Range"), o.size)
	if err != nil {
		return err
	}
	if !partial && strings.EqualFold(q.r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") {
		setChecksumHeaders(h, o.attrs().Checksums)
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, o.size))
		status = http.StatusPartialContent
	}
	q.w.WriteHeader(status)
	if head || length == 0 {
		return nil
	}
	if _, err := rd.Seek(start, io.SeekStart); err != nil {
		return nil // headers are sent
	}
	if _, err := io.CopyN(q.w, rd, length); err != nil && ctx.Err() == nil {
		s.Log.Warn("S3 GetObject interrupted", "bucket", b.name, "key", q.key, "err", err)
	}
	return nil
}

// keyErr maps a read error for a key to NoSuchKey where it means "no such
// object" (missing, a directory, a key the layout cannot hold).
func keyErr(err error, key string) error {
	if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrIsDir) || errors.Is(err, repo.ErrInvalidPath) || errors.Is(err, repo.ErrPathConflict) {
		e := *errNoSuchKey
		e.Key = key
		return &e
	}
	return err
}

func setObjectHeaders(h http.Header, o *objInfo) {
	a := o.attrs()
	ct := a.ContentType
	if ct == "" {
		ct = defaultContentType
	}
	h.Set("Content-Type", ct)
	h.Set("ETag", `"`+o.etag+`"`)
	h.Set("Last-Modified", o.lastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	for k, v := range a.Meta {
		if name, ok := strings.CutPrefix(k, ":"); ok {
			h.Set(name, v)
		} else {
			// Lower case, as S3 sends them: clients such as botocore take
			// metadata names from the header names as received.
			h["x-amz-meta-"+k] = []string{v}
		}
	}
}

// setChecksumHeaders returns stored checksums: "<alg>" keys are full-object
// values, "<alg>-<parts>" keys composite values of multipart uploads.
func setChecksumHeaders(h http.Header, cks map[string][]byte) {
	for k, v := range cks {
		alg, parts, composite := strings.Cut(k, "-")
		val := base64.StdEncoding.EncodeToString(v)
		if composite {
			val += "-" + parts
			h.Set("x-amz-checksum-type", Composite)
		} else {
			h.Set("x-amz-checksum-type", FullObject)
		}
		h.Set(checksumHeader(alg), val)
	}
}

// checkConditions evaluates If-Match, If-None-Match, If-Modified-Since and
// If-Unmodified-Since (with prefix "x-amz-copy-source-" for CopyObject
// sources) against an object, in the RFC 7232 order S3 uses. Copy-source
// conditions fail with 412 only.
func checkConditions(h http.Header, prefix string, o *objInfo) error {
	get := func(name string) string { return h.Get(prefix + name) }
	lm := o.lastModified.Truncate(time.Second)
	if im := get("If-Match"); im != "" {
		if !etagMatches(im, o.etag) {
			return errPrecondition
		}
	} else if ius := get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil && lm.After(t) {
			return errPrecondition
		}
	}
	notModified := s3err(http.StatusNotModified, "NotModified", "Not Modified")
	if prefix != "" {
		notModified = errPrecondition
	}
	if inm := get("If-None-Match"); inm != "" {
		if etagMatches(inm, o.etag) {
			return notModified
		}
	} else if ims := get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !lm.After(t) {
			return notModified
		}
	}
	return nil
}

// etagMatches compares an If-Match/If-None-Match header (a list of quoted
// ETags, or "*") with an ETag.
func etagMatches(header, etag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, "W/")
		if v == "*" || strings.Trim(v, `"`) == etag {
			return true
		}
	}
	return false
}

// parseRange parses a single byte range. Unsupported forms (several
// ranges) are ignored, as S3 does: the whole object is returned.
func parseRange(v string, size int64) (start, length int64, partial bool, err error) {
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, size, false, nil
	}
	a, bs, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, size, false, nil
	}
	invalid := &Error{Status: http.StatusRequestedRangeNotSatisfiable, Code: "InvalidRange", Message: "The requested range is not satisfiable",
		Header: map[string]string{"Content-Range": fmt.Sprintf("bytes */%d", size)}}
	if a == "" {
		n, err := strconv.ParseInt(bs, 10, 64)
		if err != nil || n < 0 {
			return 0, size, false, nil
		}
		if n == 0 || size == 0 {
			return 0, 0, false, invalid
		}
		n = min(n, size)
		return size - n, n, true, nil
	}
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil || first < 0 {
		return 0, size, false, nil
	}
	last := size - 1
	if bs != "" {
		if last, err = strconv.ParseInt(bs, 10, 64); err != nil || last < first {
			return 0, size, false, nil
		}
	}
	if first >= size {
		return 0, 0, false, invalid
	}
	last = min(last, size-1)
	return first, last - first + 1, true, nil
}

// --- writes ---

// requestMeta collects user metadata and the stored standard headers.
func requestMeta(h http.Header) (map[string]string, error) {
	meta := map[string]string{}
	size := 0
	for k, vs := range h {
		name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-")
		if !ok {
			continue
		}
		v := strings.Join(vs, ",")
		meta[name] = v
		size += len(name) + len(v)
	}
	if size > maxMetaSize {
		return nil, errCode("MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size.")
	}
	for _, name := range storedHeaders {
		v := h.Get(name)
		if name == "Content-Encoding" {
			v = stripAWSChunked(v)
		}
		if v != "" {
			meta[":"+strings.ToLower(name)] = v
		}
	}
	if len(meta) == 0 {
		return nil, nil
	}
	return meta, nil
}

// stripAWSChunked removes the aws-chunked transfer coding from a
// Content-Encoding value: it describes the request, not the object.
func stripAWSChunked(v string) string {
	var keep []string
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" && !strings.EqualFold(c, "aws-chunked") {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, ",")
}

func validKey(b *bucket, key string) error {
	if len(key) > maxKeyLength {
		return errCode("KeyTooLongError", "Your key is too long")
	}
	return b.rp.CheckPath(key)
}

// writePrecondition builds the check for conditional writes (If-Match,
// If-None-Match with "*" or ETags), evaluated against the current entry at
// commit time.
func (s *Server) writePrecondition(ctx context.Context, b *bucket, h http.Header) (repo.Precondition, error) {
	im, inm := h.Get("If-Match"), h.Get("If-None-Match")
	if im == "" && inm == "" {
		return nil, nil
	}
	return func(cur *object.Entry) error {
		if !readable(cur) {
			cur = nil
		}
		if cur == nil {
			if im != "" {
				e := *errNoSuchKey
				return &e
			}
			return nil
		}
		etag, err := s.etag(ctx, b.rp, cur)
		if err != nil {
			return err
		}
		if (im != "" && !etagMatches(im, etag)) || (inm != "" && etagMatches(inm, etag)) {
			return errPrecondition
		}
		return nil
	}, nil
}

// maxCommitRetries bounds compare-and-swap retries of a single S3 write.
const maxCommitRetries = 20

// commit writes changes to the bucket's ref: through the group committer in
// autocommit namespaces, otherwise as one commit per request (CAS on the
// current head, retried on conflict). pre, if set, is checked against the
// entry at changes[0].Path.
func (s *Server) commit(ctx context.Context, b *bucket, c *auth.Caller, changes []repo.Change, pre repo.Precondition, msg string) (hash.Hash, error) {
	if b.rp.Meta().Autocommit && len(changes) == 1 {
		return b.rp.Submit(ctx, repo.SubmitRequest{Ref: b.ref, Change: changes[0], Precondition: pre,
			Author: person(c), Principal: c.Principal, Message: msg})
	}
	for attempt := 0; ; attempt++ {
		head := b.head()
		if pre != nil {
			var cur *object.Entry
			e, err := b.rp.Stat(ctx, head, changes[0].Path)
			switch {
			case err == nil:
				cur = &e
			case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrIsDir), errors.Is(err, repo.ErrPathConflict):
			default:
				return hash.Zero, err
			}
			if err := pre(cur); err != nil {
				return hash.Zero, err
			}
		}
		h, err := b.rp.Commit(ctx, repo.CommitRequest{Ref: b.ref, Parent: &head, Changes: changes,
			Author: person(c), Message: msg, Principal: c.Principal, CheckPath: writeCheck(b.g)})
		if errors.Is(err, repo.ErrConflict) && attempt < maxCommitRetries {
			continue
		}
		return h, err
	}
}

func (s *Server) putObject(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	if err := validKey(b, q.key); err != nil {
		return err
	}
	if q.r.ContentLength > maxObjectSize {
		return errCode("EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size; use multipart upload.")
	}
	meta, err := requestMeta(q.r.Header)
	if err != nil {
		return err
	}
	pre, err := s.writePrecondition(ctx, b, q.r.Header)
	if err != nil {
		return err
	}
	body, err := newBody(q.r, q.sig, "")
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
	etag := hex.EncodeToString(res.Digests.MD5[:])
	attrs := &object.Attrs{ContentType: q.r.Header.Get("Content-Type"), Meta: meta, ETag: etag,
		MTime: time.Now().UnixNano(), Checksums: nilIfEmpty(cks)}
	ch := repo.Change{Op: repo.OpPut, Path: q.key, Entry: object.Entry{Kind: object.KindFile, Target: res.File, Attrs: attrs}}
	commit, err := s.commit(ctx, b, q.caller, []repo.Change{ch}, pre, "s3: PutObject "+q.key)
	if err != nil {
		return err
	}
	h := q.w.Header()
	h.Set("ETag", `"`+etag+`"`)
	for alg, v := range cks {
		h.Set(checksumHeader(alg), base64.StdEncoding.EncodeToString(v))
	}
	if b.versioned() {
		h.Set("x-amz-version-id", versionID(commit))
	}
	q.w.WriteHeader(http.StatusOK)
	return nil
}

func nilIfEmpty(m map[string][]byte) map[string][]byte {
	if len(m) == 0 {
		return nil
	}
	return m
}

func (s *Server) deleteObject(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	if v := q.r.URL.Query().Get("versionId"); v != "" && !(v == nullVersion && !b.versioned()) {
		res, err := s.deleteVersion(ctx, b, q.caller, q.key, v)
		if err != nil {
			return err
		}
		res.setHeaders(q.w.Header())
		q.w.WriteHeader(http.StatusNoContent)
		return nil
	}
	pre, err := s.deletePrecondition(ctx, b, q.key, q.r.Header)
	if err != nil {
		return err
	}
	commit, deleted, err := s.deleteKey(ctx, b, q.caller, q.key, pre)
	if err != nil {
		return err
	}
	if deleted && b.versioned() {
		q.w.Header().Set("x-amz-delete-marker", "true")
		q.w.Header().Set("x-amz-version-id", versionID(commit))
	}
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

// deletePrecondition builds the checks of a conditional delete: If-Match,
// x-amz-if-match-size and x-amz-if-match-last-modified-time. They apply
// only if the key exists; deleting a missing key always succeeds.
func (s *Server) deletePrecondition(ctx context.Context, b *bucket, key string, h http.Header) (repo.Precondition, error) {
	im, size, mtime := h.Get("If-Match"), h.Get("X-Amz-If-Match-Size"), h.Get("X-Amz-If-Match-Last-Modified-Time")
	if im == "" && size == "" && mtime == "" {
		return nil, nil
	}
	wantSize, err := strconv.ParseInt(size, 10, 64)
	if size != "" && err != nil {
		return nil, errCode("InvalidArgument", "invalid x-amz-if-match-size")
	}
	wantTime, err := http.ParseTime(mtime)
	if mtime != "" && err != nil {
		return nil, errCode("InvalidArgument", "invalid x-amz-if-match-last-modified-time")
	}
	return func(cur *object.Entry) error {
		if !readable(cur) {
			return nil
		}
		o, err := s.info(ctx, b, b.head(), key, *cur)
		if err != nil {
			return err
		}
		if (im != "" && !etagMatches(im, o.etag)) || (size != "" && o.size != wantSize) ||
			(mtime != "" && !o.lastModified.Truncate(time.Second).Equal(wantTime)) {
			return errPrecondition
		}
		return nil
	}, nil
}

// deleteKey removes a key. Deleting a missing key succeeds without a commit
// (S3 semantics).
func (s *Server) deleteKey(ctx context.Context, b *bucket, c *auth.Caller, key string, pre repo.Precondition) (hash.Hash, bool, error) {
	if _, err := b.rp.Stat(ctx, b.head(), key); err != nil {
		if keyErr(err, key) != err {
			return hash.Zero, false, nil
		}
		return hash.Zero, false, err
	}
	commit, err := s.commit(ctx, b, c, []repo.Change{{Op: repo.OpDelete, Path: key}}, pre, "s3: DeleteObject "+key)
	return commit, err == nil, err
}

type deleteRequest struct {
	Quiet   bool
	Objects []struct {
		Key       string
		VersionId string
	} `xml:"Object"`
}

type deletedXML struct {
	Key                   string
	VersionId             string `xml:",omitempty"`
	DeleteMarker          bool   `xml:",omitempty"`
	DeleteMarkerVersionId string `xml:",omitempty"`
}

type deleteErrorXML struct {
	Key       string
	VersionId string `xml:",omitempty"`
	Code      string
	Message   string
}

type deleteResult struct {
	XMLName struct{}         `xml:"DeleteResult"`
	NS      string           `xml:"xmlns,attr"`
	Deleted []deletedXML     `xml:"Deleted"`
	Errors  []deleteErrorXML `xml:"Error"`
}

// maxDeleteKeys is the S3 limit of keys per DeleteObjects request.
const maxDeleteKeys = 1000

// deleteObjects deletes up to 1000 keys. Plain deletes are applied in one
// commit; a key that fails (permission, lock) is reported and left out.
func (s *Server) deleteObjects(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	var req deleteRequest
	if err := readXML(q.r, &req); err != nil {
		return err
	}
	if len(req.Objects) > maxDeleteKeys {
		return errCode("MalformedXML", "at most %d keys can be deleted per request", maxDeleteKeys)
	}
	res := deleteResult{NS: xmlNS, Deleted: []deletedXML{}}
	fail := func(key, version string, err error) {
		e := s.toS3Error(q.r, err)
		res.Errors = append(res.Errors, deleteErrorXML{Key: key, VersionId: version, Code: e.Code, Message: e.Message})
	}
	head := b.head()
	pending := map[string]bool{}
	var plain []string
	for _, o := range req.Objects {
		if !b.g.Allow(o.Key, serverdb.PermWrite) {
			fail(o.Key, o.VersionId, errAccessDenied)
			continue
		}
		if o.VersionId != "" && !(o.VersionId == nullVersion && !b.versioned()) {
			vr, err := s.deleteVersion(ctx, b, q.caller, o.Key, o.VersionId)
			if err != nil {
				fail(o.Key, o.VersionId, err)
				continue
			}
			res.Deleted = append(res.Deleted, deletedXML{Key: o.Key, VersionId: o.VersionId, DeleteMarker: vr.deleteMarker})
			continue
		}
		if _, err := b.rp.Stat(ctx, head, o.Key); err == nil && !pending[o.Key] {
			pending[o.Key] = true
			plain = append(plain, o.Key)
		}
		res.Deleted = append(res.Deleted, deletedXML{Key: o.Key, VersionId: o.VersionId})
	}
	// Commit the existing keys, dropping keys whose change is refused.
	var commit hash.Hash
	for len(plain) > 0 {
		changes := make([]repo.Change, len(plain))
		for i, k := range plain {
			changes[i] = repo.Change{Op: repo.OpDelete, Path: k}
		}
		msg := fmt.Sprintf("s3: DeleteObjects (%d keys)", len(plain))
		commit, err = s.commitAll(ctx, b, q.caller, changes, msg)
		var le *repo.LockedError
		if errors.As(err, &le) && pending[le.Path] {
			fail(le.Path, "", err)
			delete(pending, le.Path)
			plain = removeString(plain, le.Path)
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	failed := map[string]bool{}
	for _, e := range res.Errors {
		failed[e.Key] = true
	}
	kept := res.Deleted[:0]
	for _, d := range res.Deleted {
		if failed[d.Key] && d.VersionId == "" {
			continue
		}
		if pending[d.Key] && d.VersionId == "" && b.versioned() {
			d.DeleteMarker = true
			d.DeleteMarkerVersionId = versionID(commit)
		}
		kept = append(kept, d)
	}
	res.Deleted = kept
	if req.Quiet {
		res.Deleted = nil
	}
	return writeXML(q.w, http.StatusOK, res)
}

// commitAll commits several changes as one commit (CAS, retried).
func (s *Server) commitAll(ctx context.Context, b *bucket, c *auth.Caller, changes []repo.Change, msg string) (hash.Hash, error) {
	for attempt := 0; ; attempt++ {
		head := b.head()
		h, err := b.rp.Commit(ctx, repo.CommitRequest{Ref: b.ref, Parent: &head, Changes: changes,
			Author: person(c), Message: msg, Principal: c.Principal, CheckPath: writeCheck(b.g)})
		if errors.Is(err, repo.ErrConflict) && attempt < maxCommitRetries {
			continue
		}
		return h, err
	}
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// --- copy ---

// copySource is a resolved x-amz-copy-source.
type copySource struct {
	b       *bucket
	key     string
	commit  hash.Hash
	version string // version id given, if any
	info    objInfo
}

func (s *Server) copySource(q *request) (*copySource, error) {
	ctx := q.r.Context()
	raw := q.r.Header.Get("X-Amz-Copy-Source")
	src, query, _ := strings.Cut(raw, "?")
	src, err := url.PathUnescape(strings.TrimPrefix(src, "/"))
	if err != nil {
		return nil, errCode("InvalidArgument", "Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	bname, key, ok := strings.Cut(src, "/")
	if !ok || bname == "" || key == "" {
		return nil, errCode("InvalidArgument", "Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	b, err := s.bucketFor(ctx, q.caller, bname)
	if err != nil {
		return nil, err
	}
	if err := need(b.g.Allow(key, serverdb.PermRead)); err != nil {
		return nil, err
	}
	cs := &copySource{b: b, key: key, commit: b.head()}
	if vals, err := url.ParseQuery(query); err == nil && vals.Get("versionId") != "" {
		cs.version = vals.Get("versionId")
		if cs.commit, err = s.resolveVersion(ctx, b, key, cs.version); err != nil {
			return nil, err
		}
	}
	e, err := b.rp.Stat(ctx, cs.commit, key)
	if err != nil {
		return nil, keyErr(err, key)
	}
	if !readable(&e) {
		return nil, keyErr(repo.ErrNotFound, key)
	}
	if cs.info, err = s.info(ctx, b, cs.commit, key, e); err != nil {
		return nil, err
	}
	if err := checkConditions(q.r.Header, "X-Amz-Copy-Source-", &cs.info); err != nil {
		return nil, err
	}
	return cs, nil
}

// copyFile returns a File with the source's content in the destination
// repository: the same File in the same repository (no copy), otherwise the
// content streamed into the destination (objects are per repository, D6).
func (s *Server) copyFile(ctx context.Context, dst *bucket, cs *copySource, off, n int64) (hash.Hash, string, error) {
	whole := off == 0 && n == cs.info.size
	if dst.rp == cs.b.rp && whole {
		return cs.info.entry.Target, cs.info.etag, nil
	}
	rd, err := cs.b.rp.OpenFile(ctx, cs.info.entry.Target)
	if err != nil {
		return hash.Zero, "", err
	}
	if _, err := rd.Seek(off, io.SeekStart); err != nil {
		return hash.Zero, "", err
	}
	res, err := dst.rp.Ingest(ctx, io.LimitReader(rd, n), n)
	if err != nil {
		return hash.Zero, "", err
	}
	etag := hex.EncodeToString(res.Digests.MD5[:])
	if whole && strings.Contains(cs.info.etag, "-") {
		etag = cs.info.etag // keep a multipart ETag, as for same-repository copies
	}
	return res.File, etag, nil
}

type copyObjectResult struct {
	XMLName      struct{} `xml:"CopyObjectResult"`
	NS           string   `xml:"xmlns,attr"`
	ETag         string
	LastModified string
}

func (s *Server) copyObject(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Allow(q.key, serverdb.PermWrite)); err != nil {
		return err
	}
	if err := validKey(b, q.key); err != nil {
		return err
	}
	cs, err := s.copySource(q)
	if err != nil {
		return err
	}
	replace := false
	switch d := q.r.Header.Get("X-Amz-Metadata-Directive"); d {
	case "", "COPY":
	case "REPLACE":
		replace = true
	default:
		return errCode("InvalidArgument", "Unknown metadata directive %q", d)
	}
	if cs.b.ns == b.ns && cs.b.ref == b.ref && cs.key == q.key && cs.version == "" && !replace {
		return errCode("InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	}
	file, etag, err := s.copyFile(ctx, b, cs, 0, cs.info.size)
	if err != nil {
		return err
	}
	src := cs.info.attrs()
	attrs := &object.Attrs{ContentType: src.ContentType, Meta: src.Meta, ETag: etag, MTime: time.Now().UnixNano(), Checksums: src.Checksums}
	if replace {
		if attrs.Meta, err = requestMeta(q.r.Header); err != nil {
			return err
		}
		attrs.ContentType = q.r.Header.Get("Content-Type")
	}
	kind := object.KindFile
	if cs.info.entry.Kind == object.KindExec && cs.b.rp == b.rp {
		kind = object.KindExec
	}
	pre, err := s.writePrecondition(ctx, b, q.r.Header)
	if err != nil {
		return err
	}
	ch := repo.Change{Op: repo.OpPut, Path: q.key, Entry: object.Entry{Kind: kind, Target: file, Attrs: attrs}}
	commit, err := s.commit(ctx, b, q.caller, []repo.Change{ch}, pre, "s3: CopyObject "+q.key)
	if err != nil {
		return err
	}
	h := q.w.Header()
	if b.versioned() {
		h.Set("x-amz-version-id", versionID(commit))
	}
	if cs.version != "" {
		h.Set("x-amz-copy-source-version-id", cs.version)
	}
	return writeXML(q.w, http.StatusOK, copyObjectResult{NS: xmlNS, ETag: `"` + etag + `"`, LastModified: s3Time(time.Unix(0, attrs.MTime))})
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
