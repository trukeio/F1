// Package s3 implements the F1 S3 compatibility API (doc/plan.md, "S3
// compatibility API"; ADR-0007): a bucket is a namespace (or an alias for a
// namespace and ref), a key is a path, and writes are commits.
package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// xmlNS is the S3 XML namespace.
const xmlNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// Repos gives access to the server's repositories.
type Repos interface {
	Get(ns string) (*repo.Repo, error)
	Names() []string
	Create(ctx context.Context, ns string, cfg policy.Config) (*repo.Repo, error)
	// Delete removes a namespace from the server (DeleteBucket).
	Delete(ctx context.Context, ns string) error
}

// Alias maps a bucket name to a namespace and ref.
type Alias struct {
	Namespace string
	Ref       string
}

// Config configures the S3 API.
type Config struct {
	Region       string           // region of credential scopes and GetBucketLocation ("us-east-1")
	Domain       string           // enables virtual-host addressing: <bucket>.<Domain>
	CreateBucket bool             // allow CreateBucket and DeleteBucket (server administrators only)
	UploadExpiry time.Duration    // open multipart uploads are aborted after this (7 days)
	MinPartSize  int64            // minimum size of every part but the last (5 MiB)
	Aliases      map[string]Alias // bucket name → namespace and ref
}

// Server serves the S3 API.
type Server struct {
	Repos    Repos
	DB       *serverdb.DB
	ACL      *auth.Authorizer
	GroupTTL time.Duration // groups of external principals from their last token use
	Config   Config
	Log      *slog.Logger

	once    sync.Once
	sig     *auth.SigV4
	lastMod *lastModCache
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.Config.Region == "" {
			s.Config.Region = "us-east-1"
		}
		if s.Config.UploadExpiry == 0 {
			s.Config.UploadExpiry = 7 * 24 * time.Hour
		}
		if s.Config.MinPartSize == 0 {
			s.Config.MinPartSize = 5 << 20
		}
		s.sig = &auth.SigV4{Region: s.Config.Region, Service: "s3", Secret: s.secret}
		s.lastMod = newLastModCache(1 << 16)
	})
}

func (s *Server) secret(ctx context.Context, accessKey string) (string, error) {
	k, err := s.DB.LookupS3Key(ctx, accessKey)
	if err != nil {
		if errors.Is(err, serverdb.ErrBadToken) {
			return "", auth.ErrUnknownAccessKey
		}
		return "", err
	}
	return k.Secret, nil
}

// request is the context of one S3 request.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	caller *auth.Caller
	sig    *auth.SigV4Result
	bucket string
	key    string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	w.Header().Set("x-amz-request-id", requestID())
	w.Header().Set("Server", "F1")
	if err := s.serve(w, r); err != nil {
		s.writeError(w, r, err)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) error {
	bucket, key := s.route(r)
	sig, err := s.sig.Verify(r.Context(), r)
	if err != nil {
		s.Log.Warn("security: S3 authentication failed", "remote", r.RemoteAddr, "reason", err.Error())
		return err
	}
	caller, err := s.callerFor(r.Context(), sig.AccessKey)
	if err != nil {
		s.Log.Warn("security: S3 authentication failed", "remote", r.RemoteAddr, "reason", err.Error())
		return errCode("InvalidAccessKeyId", "the access key is not valid")
	}
	q := &request{w: w, r: r, caller: caller, sig: sig, bucket: bucket, key: key}
	if bucket == "" {
		if r.Method == http.MethodGet {
			return s.listBuckets(q)
		}
		return errCode("MethodNotAllowed", "the specified method is not allowed against this resource")
	}
	if key == "" {
		return s.bucketOp(q)
	}
	return s.objectOp(q)
}

func (s *Server) callerFor(ctx context.Context, accessKey string) (*auth.Caller, error) {
	k, err := s.DB.LookupS3Key(ctx, accessKey)
	if err != nil {
		return nil, err
	}
	return auth.S3KeyCaller(ctx, s.DB, k, s.GroupTTL)
}

// route splits a request into bucket and key, for virtual-host addressing
// (<bucket>.<domain>) and path-style addressing (/<bucket>/<key>).
func (s *Server) route(r *http.Request) (bucket, key string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if s.Config.Domain != "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if b, ok := strings.CutSuffix(host, "."+s.Config.Domain); ok && b != "" {
			return b, path
		}
	}
	bucket, key, _ = strings.Cut(path, "/")
	return bucket, key
}

// notImplementedSub lists subresources F1 does not implement; requests
// that use them get 501 NotImplemented instead of being mistaken for plain
// object or bucket requests.
var notImplementedSub = []string{
	"acl", "tagging", "policy", "policyStatus", "cors", "lifecycle", "website", "logging", "notification",
	"replication", "encryption", "object-lock", "ownershipControls", "publicAccessBlock", "accelerate",
	"requestPayment", "analytics", "inventory", "metrics", "intelligent-tiering", "retention", "legal-hold",
	"torrent", "restore", "select", "attributes", "renameObject", "metadataTable", "session",
}

func unsupportedSub(r *http.Request) string {
	q := r.URL.Query()
	for _, sub := range notImplementedSub {
		if q.Has(sub) {
			return sub
		}
	}
	return ""
}

func (s *Server) bucketOp(q *request) error {
	query := q.r.URL.Query()
	if sub := unsupportedSub(q.r); sub != "" {
		return notImplemented("bucket subresource ?" + sub)
	}
	switch q.r.Method {
	case http.MethodPut:
		if query.Has("versioning") {
			return notImplemented("PutBucketVersioning (versioning follows the namespace retention policy)")
		}
		return s.createBucket(q)
	case http.MethodDelete:
		return s.deleteBucket(q)
	case http.MethodHead:
		return s.headBucket(q)
	case http.MethodGet:
		switch {
		case query.Has("location"):
			return s.getBucketLocation(q)
		case query.Has("versioning"):
			return s.getBucketVersioning(q)
		case query.Has("uploads"):
			return s.listUploads(q)
		case query.Has("versions"):
			return s.listVersions(q)
		case query.Get("list-type") == "2":
			return s.listObjects(q, true)
		}
		return s.listObjects(q, false)
	case http.MethodPost:
		if query.Has("delete") {
			return s.deleteObjects(q)
		}
	}
	return errCode("MethodNotAllowed", "the specified method is not allowed against this resource")
}

func (s *Server) objectOp(q *request) error {
	query := q.r.URL.Query()
	if sub := unsupportedSub(q.r); sub != "" {
		return notImplemented("object subresource ?" + sub)
	}
	switch q.r.Method {
	case http.MethodGet:
		if query.Has("uploadId") {
			return s.listParts(q)
		}
		return s.getObject(q, false)
	case http.MethodHead:
		return s.getObject(q, true)
	case http.MethodPut:
		if query.Has("uploadId") || query.Has("partNumber") {
			if q.r.Header.Get("X-Amz-Copy-Source") != "" {
				return s.uploadPartCopy(q)
			}
			return s.uploadPart(q)
		}
		if q.r.Header.Get("X-Amz-Copy-Source") != "" {
			return s.copyObject(q)
		}
		return s.putObject(q)
	case http.MethodDelete:
		if query.Has("uploadId") {
			return s.abortUpload(q)
		}
		return s.deleteObject(q)
	case http.MethodPost:
		if query.Has("uploads") {
			return s.createUpload(q)
		}
		if query.Has("uploadId") {
			return s.completeUpload(q)
		}
	}
	return errCode("MethodNotAllowed", "the specified method is not allowed against this resource")
}

// bucket is a resolved bucket: a namespace, the ref S3 reads and writes,
// and the caller's grants in the namespace.
type bucket struct {
	name string
	ns   string
	ref  string // full ref name
	rp   *repo.Repo
	g    *auth.Grants
}

// head returns the commit the bucket's ref points to (zero if none).
func (b *bucket) head() hash.Hash { return b.rp.Ref(b.ref) }

// versioned reports whether the bucket keeps object versions: every
// retention policy except "none" (doc/plan.md, "Operations").
func (b *bucket) versioned() bool { return b.rp.Meta().Retention != "none" }

// bucket resolves a bucket name. A caller without any permission in the
// namespace gets NoSuchBucket, so that namespace names do not leak
// (ADR-0006).
func (s *Server) bucketFor(ctx context.Context, c *auth.Caller, name string) (*bucket, error) {
	ns, ref := name, ""
	if a, ok := s.Config.Aliases[name]; ok {
		ns, ref = a.Namespace, a.Ref
	}
	g, err := s.ACL.For(ctx, c, ns)
	if err != nil {
		return nil, err
	}
	rp, err := s.Repos.Get(ns)
	if err != nil || !(g.Any(serverdb.PermRead) || g.Any(serverdb.PermWrite)) {
		e := errCode("NoSuchBucket", "The specified bucket does not exist")
		return nil, e
	}
	if ref == "" {
		ref = rp.Meta().DefaultRef
	}
	return &bucket{name: name, ns: ns, ref: policy.FullRefName(ref), rp: rp, g: g}, nil
}

func (s *Server) bucket(q *request) (*bucket, error) {
	return s.bucketFor(q.r.Context(), q.caller, q.bucket)
}

func need(ok bool) error {
	if !ok {
		return errAccessDenied
	}
	return nil
}

// writeCheck enforces write ACLs on every changed file of a commit.
func writeCheck(g *auth.Grants) func(string, repo.PathChange) error {
	return func(_ string, c repo.PathChange) error {
		if !g.Allow(c.Path, serverdb.PermWrite) {
			return errAccessDenied
		}
		return nil
	}
}

func person(c *auth.Caller) repo.Person {
	name := strings.TrimPrefix(c.Principal, "user:")
	return repo.Person{Name: name, Email: name}
}

func writeXML(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return nil
	}
	xml.NewEncoder(w).Encode(v)
	return nil
}

func readXML(r *http.Request, v any) error {
	if err := xml.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v); err != nil {
		return errCode("MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
	}
	return nil
}

func requestID() string {
	var b [8]byte
	rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// s3Time formats a time as S3 XML does.
func s3Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
