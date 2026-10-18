// Package native implements the F1 native HTTP API under /api/v1
// (doc/plan.md, "Native API"). JSON types are shared with the Go client in
// pkg/f1client.
package native

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
	"github.com/trukeio/f1/internal/store"
	api "github.com/trukeio/f1/pkg/f1client"
)

// Repos gives access to the server's open repositories.
type Repos interface {
	Get(ns string) (*repo.Repo, error)
	Names() []string
	Create(ctx context.Context, ns string, cfg policy.Config) (*repo.Repo, error)
}

// Server serves the native API.
type Server struct {
	Repos Repos
	Auth  *auth.Chain
	ACL   *auth.Authorizer
	DB    *serverdb.DB
	Log   *slog.Logger
	// TokenMaxTTL is the longest expiry of a token created through
	// /me/tokens (ADR-0020).
	TokenMaxTTL time.Duration
}

type callerKey struct{}

func callerFrom(ctx context.Context) *auth.Caller {
	c, _ := ctx.Value(callerKey{}).(*auth.Caller)
	return c
}

// Handler returns the API handler, to be mounted at /api/v1/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				s.writeError(w, r, err)
			}
		})
	}
	h("GET /api/v1/me", s.me)
	h("POST /api/v1/me/s3keys", s.createS3Key)
	h("GET /api/v1/me/s3keys", s.listS3Keys)
	h("DELETE /api/v1/me/s3keys/{key}", s.deleteS3Key)
	h("POST /api/v1/me/tokens", s.createToken)
	h("GET /api/v1/me/tokens", s.listTokens)
	h("DELETE /api/v1/me/tokens/{id}", s.deleteToken)
	h("GET /api/v1/ns", s.listNamespaces)
	h("POST /api/v1/ns", s.createNamespace)
	h("GET /api/v1/ns/{ns}/refs", s.listRefs)
	h("PUT /api/v1/ns/{ns}/refs/{ref...}", s.updateRef)
	h("GET /api/v1/ns/{ns}/files/{ref}/{path...}", s.getFile)
	h("HEAD /api/v1/ns/{ns}/files/{ref}/{path...}", s.getFile)
	h("PUT /api/v1/ns/{ns}/files/{ref}/{path...}", s.putFile)
	h("DELETE /api/v1/ns/{ns}/files/{ref}/{path...}", s.deleteFile)
	h("GET /api/v1/ns/{ns}/tree/{ref}/{prefix...}", s.listTree)
	h("GET /api/v1/ns/{ns}/log/{ref}", s.log)
	h("GET /api/v1/ns/{ns}/diff/{range}", s.diff)
	h("POST /api/v1/ns/{ns}/objects/missing", s.missingObjects)
	h("POST /api/v1/ns/{ns}/objects", s.uploadObjects)
	h("GET /api/v1/ns/{ns}/objects/{hash}", s.getObject)
	h("POST /api/v1/ns/{ns}/objects/fetch", s.fetchObjects)
	h("POST /api/v1/ns/{ns}/commits", s.commit)
	h("POST /api/v1/ns/{ns}/locks", s.lock)
	h("GET /api/v1/ns/{ns}/locks", s.listLocks)
	h("DELETE /api/v1/ns/{ns}/locks/{id}", s.unlock)
	h("GET /api/v1/ns/{ns}/archive", s.archive)
	h("GET /api/v1/ns/{ns}/acl", s.getACL)
	h("PUT /api/v1/ns/{ns}/acl", s.putACL)
	h("DELETE /api/v1/ns/{ns}/acl", s.deleteACL)
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := s.Auth.Authenticate(r.Context(), strings.TrimSpace(token), r.RemoteAddr)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="f1"`)
			s.writeError(w, r, errUnauthenticated)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// apiError is an error with an HTTP status and API error body.
type apiError struct {
	status int
	body   api.Error
}

func (e *apiError) Error() string { return e.body.Message }

func newErr(status int, code, format string, args ...any) *apiError {
	return &apiError{status, api.Error{Code: code, Message: fmt.Sprintf(format, args...)}}
}

var (
	errUnauthenticated = newErr(http.StatusUnauthorized, api.CodeUnauthenticated, "invalid or missing credentials")
	errForbidden       = newErr(http.StatusForbidden, api.CodeForbidden, "permission denied")
)

func badRequest(format string, args ...any) error {
	return newErr(http.StatusBadRequest, api.CodeBadRequest, format, args...)
}

// writeError maps core errors to API errors.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	var ce *repo.ConflictError
	var le *repo.LockedError
	switch {
	case errors.As(err, &ae):
	case errors.As(err, &ce):
		ae = newErr(http.StatusConflict, api.CodeConflict, "%s", err.Error())
		ae.body.Current = hexOrEmpty(ce.Current)
		ae.body.Paths = ce.Paths
	case errors.As(err, &le):
		ae = newErr(http.StatusLocked, api.CodeLocked, "%s", err.Error())
		ae.body.Owner = le.Owner
	case errors.Is(err, repo.ErrFrozen):
		ae = newErr(http.StatusLocked, api.CodeFrozen, "%s", err.Error())
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, store.ErrNotFound):
		ae = newErr(http.StatusNotFound, api.CodeNotFound, "%s", err.Error())
	case errors.Is(err, repo.ErrPathConflict), errors.Is(err, repo.ErrIsDir):
		ae = newErr(http.StatusConflict, api.CodePathConflict, "%s", err.Error())
	case errors.Is(err, repo.ErrLockHeld):
		ae = newErr(http.StatusConflict, api.CodeLocked, "%s", err.Error())
	case errors.Is(err, repo.ErrNotLockOwner):
		ae = newErr(http.StatusForbidden, api.CodeForbidden, "%s", err.Error())
	case errors.Is(err, store.ErrMissingReference):
		ae = newErr(http.StatusConflict, api.CodeMissingObjects, "%s", err.Error())
	case errors.Is(err, repo.ErrInvalidPath), errors.Is(err, content.ErrSizeMismatch):
		ae = newErr(http.StatusBadRequest, api.CodeBadRequest, "%s", err.Error())
	case errors.Is(err, context.Canceled):
		return // the client went away
	default:
		s.Log.Error("native API request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		ae = newErr(http.StatusInternalServerError, api.CodeInternal, "internal error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.status)
	if r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(ae.body)
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func hexOrEmpty(h hash.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()
}

func parseHash(s string) (hash.Hash, error) {
	if s == "" {
		return hash.Zero, nil
	}
	h, err := hash.Parse(s)
	if err != nil {
		return hash.Zero, badRequest("%v", err)
	}
	return h, nil
}

// ns opens the namespace of a request and loads the caller's grants. A
// caller without any permission in the namespace gets 404, so that
// namespace names do not leak.
func (s *Server) ns(r *http.Request) (*repo.Repo, *auth.Caller, *auth.Grants, error) {
	name := r.PathValue("ns")
	c := callerFrom(r.Context())
	g, err := s.ACL.For(r.Context(), c, name)
	if err != nil {
		return nil, nil, nil, err
	}
	rp, err := s.Repos.Get(name)
	if err != nil || !(g.Any(serverdb.PermRead) || g.Any(serverdb.PermWrite) || g.Any(serverdb.PermLock)) {
		return nil, nil, nil, newErr(http.StatusNotFound, api.CodeNotFound, "no such namespace: %s", name)
	}
	return rp, c, g, nil
}

func need(ok bool) error {
	if !ok {
		return errForbidden
	}
	return nil
}

// resolve resolves a ref name or commit hash.
func resolve(r *http.Request, rp *repo.Repo, ref string) (hash.Hash, error) {
	return rp.ResolveRef(r.Context(), ref)
}

func person(c *auth.Caller) repo.Person {
	name := strings.TrimPrefix(c.Principal, "user:")
	return repo.Person{Name: name, Email: name}
}

var kindNames = map[object.Kind]string{
	object.KindFile: "file", object.KindExec: "exec", object.KindSymlink: "symlink",
	object.KindDir: "dir", object.KindGitlink: "gitlink", object.KindLFS: "lfs",
}

func kindByName(name string) (object.Kind, bool) {
	for k, n := range kindNames {
		if n == name {
			return k, true
		}
	}
	return 0, false
}

func apiEntry(e *object.Entry) *api.Entry {
	if e == nil {
		return nil
	}
	out := &api.Entry{Kind: kindNames[e.Kind], Target: hexOrEmpty(e.Target)}
	if e.Attrs != nil {
		out.ContentType = e.Attrs.ContentType
	}
	return out
}

func entryFrom(e *api.Entry) (object.Entry, error) {
	if e == nil {
		return object.Entry{}, badRequest("put needs an entry")
	}
	k, ok := kindByName(e.Kind)
	if !ok {
		return object.Entry{}, badRequest("unknown entry kind %q", e.Kind)
	}
	if k == object.KindGitlink || k == object.KindLFS {
		return object.Entry{}, badRequest("entries of kind %s are created by the Git protocol", e.Kind)
	}
	t, err := parseHash(e.Target)
	if err != nil {
		return object.Entry{}, err
	}
	out := object.Entry{Kind: k, Target: t}
	if e.ContentType != "" {
		out.Attrs = &object.Attrs{ContentType: e.ContentType}
	}
	return out, nil
}

// writeCheck returns the CheckPath callback that enforces write ACLs on
// every changed file of a ref update.
func writeCheck(g *auth.Grants) func(string, repo.PathChange) error {
	return func(_ string, c repo.PathChange) error {
		if !g.Allow(c.Path, serverdb.PermWrite) {
			return newErr(http.StatusForbidden, api.CodeForbidden, "no write permission on %s", c.Path)
		}
		return nil
	}
}

// --- caller ---

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	c := callerFrom(r.Context())
	groups := c.Groups
	if groups == nil {
		groups = []string{}
	}
	return writeJSON(w, api.Me{Principal: c.Principal, Groups: groups})
}

func (s *Server) createS3Key(w http.ResponseWriter, r *http.Request) error {
	k, err := s.DB.CreateS3Key(r.Context(), callerFrom(r.Context()).Principal)
	if err != nil {
		return err
	}
	return writeJSON(w, api.S3Key{AccessKey: k.AccessKey, Secret: k.Secret, Created: k.Created})
}

func (s *Server) listS3Keys(w http.ResponseWriter, r *http.Request) error {
	keys, err := s.DB.S3Keys(r.Context(), callerFrom(r.Context()).Principal)
	if err != nil {
		return err
	}
	out := []api.S3Key{}
	for _, k := range keys {
		out = append(out, api.S3Key{AccessKey: k.AccessKey, Created: k.Created})
	}
	return writeJSON(w, out)
}

func (s *Server) deleteS3Key(w http.ResponseWriter, r *http.Request) error {
	if err := s.DB.RevokeS3Key(r.Context(), callerFrom(r.Context()).Principal, r.PathValue("key")); err != nil {
		return newErr(http.StatusNotFound, api.CodeNotFound, "no such key")
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// maxTokenDescription limits the description of a token created through
// the API.
const maxTokenDescription = 200

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	c := callerFrom(r.Context())
	var req api.CreateTokenRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if len(req.Description) > maxTokenDescription {
		return badRequest("the description is longer than %d bytes", maxTokenDescription)
	}
	ttl := time.Until(req.Expires)
	switch {
	case req.Expires.IsZero():
		return badRequest("expires is required")
	case ttl <= 0:
		return badRequest("expires is in the past")
	case ttl > s.TokenMaxTTL:
		return badRequest("expires is more than %s from now", s.TokenMaxTTL)
	}
	t, secret, err := s.DB.CreateToken(r.Context(), c.Principal, req.Description, ttl)
	if err != nil {
		return err
	}
	s.Log.Info("security: token created", "principal", c.Principal, "token", t.ID, "expires", t.Expires)
	out := tokenInfo(t)
	out.Secret = secret
	return writeJSON(w, out)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	toks, err := s.DB.Tokens(r.Context(), callerFrom(r.Context()).Principal)
	if err != nil {
		return err
	}
	out := []api.Token{}
	for _, t := range toks {
		if !t.Revoked {
			out = append(out, tokenInfo(t))
		}
	}
	return writeJSON(w, out)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) error {
	c := callerFrom(r.Context())
	if err := s.DB.RevokeToken(r.Context(), c.Principal, r.PathValue("id")); err != nil {
		if errors.Is(err, serverdb.ErrNoToken) {
			return newErr(http.StatusNotFound, api.CodeNotFound, "no such token")
		}
		return err
	}
	s.Log.Info("security: token revoked", "principal", c.Principal, "token", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func tokenInfo(t serverdb.Token) api.Token {
	out := api.Token{ID: t.ID, Principal: t.Principal, Description: t.Description, Created: t.Created}
	if !t.Expires.IsZero() {
		out.Expires = &t.Expires
	}
	return out
}

// --- namespaces and refs ---

func nsInfo(name string, rp *repo.Repo) api.Namespace {
	m := rp.Meta()
	return api.Namespace{Name: name, UUID: m.UUID, Frozen: rp.Frozen(), Config: api.NamespaceConfig{
		Layout: m.Layout, Retention: m.Retention, Autocommit: m.Autocommit, Branches: m.Branches,
		Locks: m.Locks, Git: m.Git, DefaultRef: m.DefaultRef, Merge: m.Merge,
	}}
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) error {
	c := callerFrom(r.Context())
	g, err := s.ACL.For(r.Context(), c, serverdb.ServerNamespace)
	if err != nil {
		return err
	}
	visible, err := s.DB.ACLNamespaces(r.Context(), c.Principals())
	if err != nil {
		return err
	}
	out := []api.Namespace{}
	for _, name := range s.Repos.Names() {
		if !g.ServerAdmin() && !visible[name] {
			continue
		}
		if rp, err := s.Repos.Get(name); err == nil {
			out = append(out, nsInfo(name, rp))
		}
	}
	return writeJSON(w, out)
}

func (s *Server) createNamespace(w http.ResponseWriter, r *http.Request) error {
	g, err := s.ACL.For(r.Context(), callerFrom(r.Context()), serverdb.ServerNamespace)
	if err != nil {
		return err
	}
	if !g.ServerAdmin() {
		return errForbidden
	}
	var req api.CreateNamespaceRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	cfg := policy.Default()
	switch {
	case req.Preset != "" && req.Config != nil:
		return badRequest("give a preset or a config, not both")
	case req.Preset != "":
		if cfg, err = policy.Preset(req.Preset); err != nil {
			return badRequest("%v", err)
		}
	case req.Config != nil:
		c := req.Config
		cfg = policy.Config{Layout: c.Layout, Retention: c.Retention, Autocommit: c.Autocommit, Branches: c.Branches,
			Locks: c.Locks, Git: c.Git, DefaultRef: c.DefaultRef, Merge: c.Merge}
	}
	if err := policy.ValidNamespace(req.Name); err != nil {
		return badRequest("%v", err)
	}
	if err := cfg.Validate(); err != nil {
		return badRequest("%v", err)
	}
	rp, err := s.Repos.Create(r.Context(), req.Name, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return newErr(http.StatusConflict, api.CodeExists, "%v", err)
		}
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	return json.NewEncoder(w).Encode(nsInfo(req.Name, rp))
}

func (s *Server) listRefs(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Any(serverdb.PermRead)); err != nil {
		return err
	}
	refs := rp.Refs()
	out := make([]api.Ref, 0, len(refs))
	for name, h := range refs {
		out = append(out, api.Ref{Name: name, Commit: h.String()})
	}
	sortRefs(out)
	return writeJSON(w, out)
}

func (s *Server) updateRef(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	// A ref update names commits by hash: whole-namespace read (D13).
	if err := need(g.Whole(serverdb.PermRead)); err != nil {
		return err
	}
	var req api.UpdateRefRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	old, err := parseHash(req.Old)
	if err != nil {
		return err
	}
	nw, err := parseHash(req.New)
	if err != nil {
		return err
	}
	ref := policy.FullRefName(r.PathValue("ref"))
	// Branch updates are checked file by file; tags and deletions change
	// no file and need write on the whole namespace.
	if !strings.HasPrefix(ref, "heads/") || nw.IsZero() {
		if err := need(g.Whole(serverdb.PermWrite)); err != nil {
			return err
		}
	}
	err = rp.UpdateRefs(r.Context(), []repo.RefUpdate{{Ref: ref, Old: old, New: nw}},
		repo.UpdateOptions{Principal: c.Principal, Reason: "api: update ref", CheckPath: writeCheck(g)})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- files ---

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	path := r.PathValue("path")
	if err := need(g.Allow(path, serverdb.PermRead)); err != nil {
		return err
	}
	commit, err := resolve(r, rp, r.PathValue("ref"))
	if err != nil {
		return err
	}
	rd, e, err := rp.Open(r.Context(), commit, path)
	if err != nil {
		return err
	}
	ct := "application/octet-stream"
	if e.Attrs != nil && e.Attrs.ContentType != "" {
		ct = e.Attrs.ContentType
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("ETag", `"`+e.Target.String()+`"`)
	h.Set("Cache-Control", cacheControl(rp, r.PathValue("ref"), commit))
	h.Set("F1-Commit", commit.String())
	h.Set("F1-File", e.Target.String())
	h.Set("F1-Kind", kindNames[e.Kind])
	h.Set("F1-Size", strconv.FormatInt(rd.Size(), 10))
	h.Set("Accept-Ranges", "bytes")
	// ServeContent handles Range, If-Range, If-Match and If-None-Match
	// against the ETag, and HEAD.
	http.ServeContent(w, r, "", time.Time{}, rd)
	return nil
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	path := r.PathValue("path")
	if err := need(g.Allow(path, serverdb.PermWrite)); err != nil {
		return err
	}
	if err := rp.CheckPath(path); err != nil {
		return err
	}
	parent, hasParent, err := parentHeader(r)
	if err != nil {
		return err
	}
	if !hasParent && !rp.Meta().Autocommit {
		return newErr(http.StatusPreconditionRequired, api.CodeParentRequired, "the F1-Parent header is required: give the commit you expect the ref to be at, or \"new\"")
	}
	res, err := rp.Ingest(r.Context(), r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	e := object.Entry{Kind: object.KindFile, Target: res.File}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		e.Attrs = &object.Attrs{ContentType: ct}
	}
	ch := repo.Change{Op: repo.OpPut, Path: path, Entry: e}
	commit, base, err := s.write(r, rp, c, g, parent, hasParent, ch, "put "+path)
	if err != nil {
		return err
	}
	return writeJSON(w, api.PutResult{
		Commit: commit.String(), Parent: hexOrEmpty(base), File: res.File.String(), Size: int64(res.Size),
		SHA256: hex.EncodeToString(res.Digests.SHA256[:]), SHA1Git: hex.EncodeToString(res.Digests.SHA1Git[:]),
		MD5: hex.EncodeToString(res.Digests.MD5[:]),
	})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	path := r.PathValue("path")
	if err := need(g.Allow(path, serverdb.PermWrite)); err != nil {
		return err
	}
	parent, hasParent, err := parentHeader(r)
	if err != nil {
		return err
	}
	if !hasParent && !rp.Meta().Autocommit {
		return newErr(http.StatusPreconditionRequired, api.CodeParentRequired, "the F1-Parent header is required")
	}
	at := parent
	if !hasParent {
		at = rp.Ref(r.PathValue("ref"))
	}
	if _, err := rp.Stat(r.Context(), at, path); err != nil {
		return err
	}
	commit, base, err := s.write(r, rp, c, g, parent, hasParent, repo.Change{Op: repo.OpDelete, Path: path}, "delete "+path)
	if err != nil {
		return err
	}
	return writeJSON(w, api.CommitResult{Commit: commit.String(), Parent: hexOrEmpty(base)})
}

// Cache-Control values (ADR-0016). Responses depend on the caller's
// permissions, so they are private.
const (
	cacheImmutable  = "private, max-age=31536000, immutable"
	cacheRevalidate = "private, no-cache"
)

// cacheControl returns the Cache-Control of a read at ref: immutable when
// ref is a commit hash rather than a ref name.
func cacheControl(rp *repo.Repo, ref string, commit hash.Hash) string {
	if h, err := hash.Parse(ref); err == nil && h == commit && rp.Ref(ref).IsZero() {
		return cacheImmutable
	}
	return cacheRevalidate
}

// parentHeader reads F1-Parent: a commit hash, or "new" for a ref that must
// not exist yet.
func parentHeader(r *http.Request) (hash.Hash, bool, error) {
	v := r.Header.Get("F1-Parent")
	switch v {
	case "":
		return hash.Zero, false, nil
	case "new":
		return hash.Zero, true, nil
	}
	h, err := parseHash(v)
	return h, true, err
}

// write commits one change: with an expected parent directly, otherwise
// through the group committer (autocommit). It returns the new commit and,
// with an expected parent, the commit it was built on.
func (s *Server) write(r *http.Request, rp *repo.Repo, c *auth.Caller, g *auth.Grants, parent hash.Hash, hasParent bool, ch repo.Change, msg string) (hash.Hash, hash.Hash, error) {
	ref := r.PathValue("ref")
	if m := r.Header.Get("F1-Message"); m != "" {
		msg = m
	}
	rebase, err := rebaseHeader(r)
	if err != nil {
		return hash.Zero, hash.Zero, err
	}
	if hasParent {
		return rp.CommitOn(r.Context(), repo.CommitRequest{Ref: ref, Parent: &parent, Changes: []repo.Change{ch},
			Author: person(c), Message: msg, Principal: c.Principal, CheckPath: writeCheck(g), Rebase: rebase})
	}
	h, err := rp.Submit(r.Context(), repo.SubmitRequest{Ref: ref, Change: ch, Author: person(c), Principal: c.Principal, Message: msg})
	return h, hash.Zero, err
}

// rebaseHeader reads F1-Rebase (ADR-0014).
func rebaseHeader(r *http.Request) (bool, error) {
	switch v := r.Header.Get("F1-Rebase"); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, badRequest("F1-Rebase must be true or false, not %q", v)
	}
}

// --- listings, history, diff ---

func (s *Server) listTree(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	commit, err := resolve(r, rp, r.PathValue("ref"))
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := 1000
	if l := q.Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > 10000 {
			return badRequest("limit must be 1 to 10000")
		}
	}
	opts := repo.ListOpts{Prefix: r.PathValue("prefix"), Delimiter: q.Get("delim"), After: q.Get("after")}
	resp := api.ListResponse{Commit: commit.String(), Items: []api.ListItem{}}
	errFull := errors.New("full")
	err = rp.List(r.Context(), commit, opts, func(it repo.ListItem) error {
		if len(resp.Items) == limit {
			resp.Next = resp.Items[len(resp.Items)-1].Path
			return errFull
		}
		if it.IsPrefix {
			if g.Under(it.Path, serverdb.PermRead) {
				resp.Items = append(resp.Items, api.ListItem{Path: it.Path, Prefix: true})
			}
			return nil
		}
		if !g.Allow(it.Path, serverdb.PermRead) {
			return nil
		}
		item := api.ListItem{Path: it.Path, Kind: kindNames[it.Entry.Kind], File: hexOrEmpty(it.Entry.Target)}
		if it.Entry.Kind != object.KindGitlink {
			size, err := rp.FileSize(r.Context(), it.Entry.Target)
			if err != nil {
				return err
			}
			item.Size = int64(size)
		}
		resp.Items = append(resp.Items, item)
		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		return err
	}
	return writeJSON(w, resp)
}

func apiCommit(h hash.Hash, c *object.Commit) api.Commit {
	out := api.Commit{Hash: h.String(), Tree: c.Tree.String(), Parents: []string{}, Message: string(c.Message),
		Author:    api.Person{Name: string(c.Author.Name), Email: string(c.Author.Email), Time: c.Author.Time, TZ: c.Author.TZ},
		Committer: api.Person{Name: string(c.Committer.Name), Email: string(c.Committer.Email), Time: c.Committer.Time, TZ: c.Committer.TZ},
	}
	for _, p := range c.Parents {
		out.Parents = append(out.Parents, p.String())
	}
	return out
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		err = need(g.Whole(serverdb.PermRead))
	} else {
		err = need(g.Allow(path, serverdb.PermRead))
	}
	if err != nil {
		return err
	}
	commit, err := resolve(r, rp, r.PathValue("ref"))
	if err != nil {
		return err
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 {
			return badRequest("invalid limit")
		}
	}
	skip := 0
	if v := r.URL.Query().Get("skip"); v != "" {
		if skip, err = strconv.Atoi(v); err != nil || skip < 0 {
			return badRequest("invalid skip")
		}
	}
	// A commit is no cursor: the walk merges branches by time, so a page
	// starts after the first skip commits of the same walk (ADR-0018).
	commits, err := rp.Log(r.Context(), commit, path, skip+limit)
	if err != nil {
		return err
	}
	commits = commits[min(skip, len(commits)):]
	out := make([]api.Commit, len(commits))
	for i, ci := range commits {
		out[i] = apiCommit(ci.Hash, ci.Commit)
	}
	return writeJSON(w, out)
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Any(serverdb.PermRead)); err != nil {
		return err
	}
	a, b, ok := strings.Cut(r.PathValue("range"), "..")
	if !ok {
		return badRequest("diff range must be <a>..<b>")
	}
	ha, err := resolve(r, rp, a)
	if err != nil {
		return err
	}
	hb, err := resolve(r, rp, b)
	if err != nil {
		return err
	}
	out := []api.Change{}
	err = rp.Diff(r.Context(), ha, hb, r.URL.Query().Get("prefix"), func(c repo.PathChange) error {
		if g.Allow(c.Path, serverdb.PermRead) {
			out = append(out, api.Change{Path: c.Path, Old: apiEntry(c.Old), New: apiEntry(c.New)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return writeJSON(w, out)
}

// --- locks ---

func apiLock(l repo.Lock) api.Lock {
	out := api.Lock{ID: l.ID, Ref: l.Ref, Path: l.Path, Owner: l.Owner, Created: l.Created}
	if !l.Expires.IsZero() {
		e := l.Expires
		out.Expires = &e
	}
	return out
}

func (s *Server) lock(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	var req api.LockRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := need(g.Allow(req.Path, serverdb.PermLock)); err != nil {
		return err
	}
	if req.Ref == "" {
		req.Ref = rp.Meta().DefaultRef
	}
	l, err := rp.LockPath(r.Context(), req.Ref, req.Path, c.Principal, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	return json.NewEncoder(w).Encode(apiLock(l))
}

func (s *Server) listLocks(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	out := []api.Lock{}
	for _, l := range rp.Locks(r.URL.Query().Get("ref"), r.URL.Query().Get("prefix")) {
		if g.Allow(l.Path, serverdb.PermRead) || g.Allow(l.Path, serverdb.PermLock) {
			out = append(out, apiLock(l))
		}
	}
	return writeJSON(w, out)
}

func (s *Server) unlock(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var target *repo.Lock
	for _, l := range rp.Locks("", "") {
		if l.ID == id {
			target = &l
			break
		}
	}
	if target == nil {
		return newErr(http.StatusNotFound, api.CodeNotFound, "no such lock")
	}
	force := r.URL.Query().Get("force") == "true"
	if force {
		err = need(g.Allow(target.Path, serverdb.PermAdmin))
	} else {
		err = need(g.Allow(target.Path, serverdb.PermLock))
	}
	if err != nil {
		return err
	}
	if _, err := rp.Unlock(r.Context(), id, c.Principal, force); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- archive ---

func (s *Server) archive(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Allow("", serverdb.PermAdmin)); err != nil {
		return err
	}
	name := r.PathValue("ns")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	if _, err := rp.Archive(r.Context(), w, name); err != nil {
		// Headers are sent; the client sees a truncated, invalid ZIP.
		s.Log.Error("archive failed", "namespace", name, "err", err)
	}
	return nil
}
