package git

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// lfsMediaType is the Git LFS API media type.
const lfsMediaType = "application/vnd.git-lfs+json"

type lfsRef struct {
	Name string `json:"name"`
}

type lfsObject struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Auth    bool                 `json:"authenticated,omitempty"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsObjectError      `json:"error,omitempty"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lfsBatchRequest struct {
	Operation string      `json:"operation"`
	Transfers []string    `json:"transfers"`
	Ref       *lfsRef     `json:"ref"`
	Objects   []lfsObject `json:"objects"`
	HashAlgo  string      `json:"hash_algo"`
}

type lfsBatchResponse struct {
	Transfer string      `json:"transfer"`
	Objects  []lfsObject `json:"objects"`
	HashAlgo string      `json:"hash_algo"`
}

type lfsLockOwner struct {
	Name string `json:"name"`
}

type lfsLock struct {
	ID       string       `json:"id"`
	Path     string       `json:"path"`
	LockedAt string       `json:"locked_at"`
	Owner    lfsLockOwner `json:"owner"`
}

// lfsError is an error of the LFS API, answered as JSON.
type lfsError struct {
	status int
	msg    string
	lock   *lfsLock
}

func (e *lfsError) Error() string { return e.msg }

func lfsErr(status int, format string, args ...any) error {
	return &lfsError{status: status, msg: fmt.Sprintf(format, args...)}
}

// lfs serves the Git LFS batch, transfer and locking APIs under
// /git/{ns}.git/info/lfs/ (doc/plan.md, "Git LFS").
func (s *Server) lfs(w http.ResponseWriter, r *http.Request, rp *repo.Repo, c *auth.Caller, g *auth.Grants, path string) error {
	err := s.serveLFS(w, r, rp, c, g, path)
	var le *lfsError
	if errors.As(err, &le) {
		w.Header().Set("Content-Type", lfsMediaType)
		w.WriteHeader(le.status)
		body := map[string]any{"message": le.msg}
		if le.lock != nil {
			body["lock"] = le.lock
		}
		return json.NewEncoder(w).Encode(body)
	}
	return err
}

func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request, rp *repo.Repo, c *auth.Caller, g *auth.Grants, path string) error {
	// LFS objects are addressed by content hash: whole-namespace read
	// (D13), as for Git.
	if !g.Whole(serverdb.PermRead) {
		return lfsErr(http.StatusForbidden, "Git LFS needs read permission on the whole namespace")
	}
	switch {
	case path == "objects/batch" && r.Method == http.MethodPost:
		return s.lfsBatch(w, r, rp, g)
	case strings.HasPrefix(path, "objects/") && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		return s.lfsTransfer(w, r, rp, g, strings.TrimPrefix(path, "objects/"))
	case path == "locks" && r.Method == http.MethodPost:
		return s.lfsLock(w, r, rp, c, g)
	case path == "locks" && r.Method == http.MethodGet:
		return s.lfsListLocks(w, r, rp)
	case path == "locks/verify" && r.Method == http.MethodPost:
		return s.lfsVerifyLocks(w, r, rp, c)
	case strings.HasPrefix(path, "locks/") && strings.HasSuffix(path, "/unlock") && r.Method == http.MethodPost:
		return s.lfsUnlock(w, r, rp, c, g, strings.TrimSuffix(strings.TrimPrefix(path, "locks/"), "/unlock"))
	}
	return lfsErr(http.StatusNotFound, "not found")
}

func writeLFS(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", lfsMediaType)
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func readLFS(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v); err != nil {
		return lfsErr(http.StatusUnprocessableEntity, "invalid JSON: %v", err)
	}
	return nil
}

func parseOID(s string) ([32]byte, bool) {
	var oid [32]byte
	if len(s) != 64 || strings.ToLower(s) != s {
		return oid, false
	}
	_, err := hex.Decode(oid[:], []byte(s))
	return oid, err == nil
}

// objectURL is the transfer URL of an LFS object on this server.
func objectURL(r *http.Request, oid string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	base := strings.TrimSuffix(r.URL.Path, "/objects/batch")
	return scheme + "://" + r.Host + base + "/objects/" + oid
}

func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request, rp *repo.Repo, g *auth.Grants) error {
	var req lfsBatchRequest
	if err := readLFS(r, &req); err != nil {
		return err
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		return lfsErr(http.StatusConflict, "unsupported hash algorithm %q", req.HashAlgo)
	}
	if len(req.Transfers) > 0 && !contains(req.Transfers, "basic") {
		return lfsErr(http.StatusConflict, "only the basic transfer adapter is supported")
	}
	switch req.Operation {
	case "download":
	case "upload":
		if !g.Any(serverdb.PermWrite) {
			return lfsErr(http.StatusForbidden, "uploads need write permission")
		}
	default:
		return lfsErr(http.StatusUnprocessableEntity, "unknown operation %q", req.Operation)
	}
	// The client sends the same credentials to the transfer URLs.
	header := map[string]string{}
	if a := r.Header.Get("Authorization"); a != "" {
		header["Authorization"] = a
	}
	res := lfsBatchResponse{Transfer: "basic", HashAlgo: "sha256", Objects: []lfsObject{}}
	for _, o := range req.Objects {
		out := lfsObject{OID: o.OID, Size: o.Size}
		oid, ok := parseOID(o.OID)
		if !ok || o.Size < 0 {
			out.Error = &lfsObjectError{Code: http.StatusUnprocessableEntity, Message: "invalid object id or size"}
			res.Objects = append(res.Objects, out)
			continue
		}
		f, stored, err := rp.Store().Cache().FileBySHA256(r.Context(), oid)
		if err != nil {
			return err
		}
		if stored {
			size, err := rp.FileSize(r.Context(), f)
			if err != nil {
				return err
			}
			stored = int64(size) == o.Size
		}
		action := lfsAction{Href: objectURL(r, o.OID), Header: header, ExpiresIn: 3600}
		switch {
		case req.Operation == "upload" && !stored:
			out.Actions = map[string]lfsAction{"upload": action}
			out.Auth = true
		case req.Operation == "download" && stored:
			out.Actions = map[string]lfsAction{"download": action}
			out.Auth = true
		case req.Operation == "download":
			out.Error = &lfsObjectError{Code: http.StatusNotFound, Message: "object does not exist"}
		}
		res.Objects = append(res.Objects, out)
	}
	return writeLFS(w, http.StatusOK, res)
}

// lfsTransfer uploads (PUT) or downloads (GET) an LFS object, streamed
// into Ingest or out of the store; an upload must hash to its id.
func (s *Server) lfsTransfer(w http.ResponseWriter, r *http.Request, rp *repo.Repo, g *auth.Grants, oidStr string) error {
	ctx := r.Context()
	oid, ok := parseOID(oidStr)
	if !ok {
		return lfsErr(http.StatusUnprocessableEntity, "invalid object id")
	}
	if r.Method == http.MethodGet {
		f, ok, err := rp.Store().Cache().FileBySHA256(ctx, oid)
		if err != nil {
			return err
		}
		if !ok {
			return lfsErr(http.StatusNotFound, "object does not exist")
		}
		rd, err := rp.OpenFile(ctx, f)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(rd.Size(), 10))
		if _, err := io.Copy(w, rd); err != nil && ctx.Err() == nil {
			s.Log.Warn("LFS download interrupted", "oid", oidStr, "err", err)
		}
		return nil
	}
	if !g.Any(serverdb.PermWrite) {
		return lfsErr(http.StatusForbidden, "uploads need write permission")
	}
	res, err := rp.Ingest(ctx, r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	if res.Digests.SHA256 != oid {
		return lfsErr(http.StatusUnprocessableEntity, "content does not hash to %s", oidStr)
	}
	if err := rp.Durable(ctx); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// --- locks ---

func lfsLockOf(l repo.Lock) *lfsLock {
	return &lfsLock{ID: l.ID, Path: l.Path, LockedAt: l.Created.UTC().Format(time.RFC3339), Owner: lfsLockOwner{Name: l.Owner}}
}

// lockRef maps an LFS ref ("refs/heads/main") to an F1 ref; no ref means
// the default ref.
func lockRef(rp *repo.Repo, ref *lfsRef) string {
	if ref == nil || ref.Name == "" {
		return policy.FullRefName(rp.Meta().DefaultRef)
	}
	return policy.FullRefName(strings.TrimPrefix(ref.Name, "refs/"))
}

func (s *Server) lfsLock(w http.ResponseWriter, r *http.Request, rp *repo.Repo, c *auth.Caller, g *auth.Grants) error {
	var req struct {
		Path string  `json:"path"`
		Ref  *lfsRef `json:"ref"`
	}
	if err := readLFS(r, &req); err != nil {
		return err
	}
	if !g.Allow(req.Path, serverdb.PermLock) {
		return lfsErr(http.StatusForbidden, "no lock permission on %s", req.Path)
	}
	ref := lockRef(rp, req.Ref)
	for _, l := range rp.Locks(ref, req.Path) {
		if l.Path == req.Path {
			return &lfsError{status: http.StatusConflict, msg: "already created lock", lock: lfsLockOf(l)}
		}
	}
	l, err := rp.LockPath(r.Context(), ref, req.Path, c.Principal, 0)
	if errors.Is(err, repo.ErrLockHeld) {
		for _, l := range rp.Locks(ref, req.Path) {
			if l.Path == req.Path {
				return &lfsError{status: http.StatusConflict, msg: "already created lock", lock: lfsLockOf(l)}
			}
		}
	}
	if err != nil {
		return lfsErr(http.StatusUnprocessableEntity, "%v", err)
	}
	return writeLFS(w, http.StatusCreated, map[string]any{"lock": lfsLockOf(l)})
}

func (s *Server) lfsListLocks(w http.ResponseWriter, r *http.Request, rp *repo.Repo) error {
	q := r.URL.Query()
	ref := lockRef(rp, &lfsRef{Name: q.Get("refspec")})
	locks := []*lfsLock{}
	for _, l := range rp.Locks(ref, q.Get("path")) {
		if (q.Get("path") != "" && l.Path != q.Get("path")) || (q.Get("id") != "" && l.ID != q.Get("id")) {
			continue
		}
		locks = append(locks, lfsLockOf(l))
	}
	return writeLFS(w, http.StatusOK, map[string]any{"locks": locks})
}

func (s *Server) lfsVerifyLocks(w http.ResponseWriter, r *http.Request, rp *repo.Repo, c *auth.Caller) error {
	var req struct {
		Ref *lfsRef `json:"ref"`
	}
	if err := readLFS(r, &req); err != nil {
		return err
	}
	ours, theirs := []*lfsLock{}, []*lfsLock{}
	for _, l := range rp.Locks(lockRef(rp, req.Ref), "") {
		if l.Owner == c.Principal {
			ours = append(ours, lfsLockOf(l))
		} else {
			theirs = append(theirs, lfsLockOf(l))
		}
	}
	return writeLFS(w, http.StatusOK, map[string]any{"ours": ours, "theirs": theirs})
}

func (s *Server) lfsUnlock(w http.ResponseWriter, r *http.Request, rp *repo.Repo, c *auth.Caller, g *auth.Grants, id string) error {
	var req struct {
		Force bool    `json:"force"`
		Ref   *lfsRef `json:"ref"`
	}
	if err := readLFS(r, &req); err != nil {
		return err
	}
	var target *repo.Lock
	for _, l := range rp.Locks("", "") {
		if l.ID == id {
			target = &l
			break
		}
	}
	if target == nil {
		return lfsErr(http.StatusNotFound, "no such lock")
	}
	perm := serverdb.PermLock
	if req.Force {
		perm = serverdb.PermAdmin
	}
	if !g.Allow(target.Path, perm) {
		return lfsErr(http.StatusForbidden, "no %s permission on %s", perm, target.Path)
	}
	l, err := rp.Unlock(r.Context(), id, c.Principal, req.Force)
	if errors.Is(err, repo.ErrNotLockOwner) {
		return lfsErr(http.StatusForbidden, "the lock belongs to %s; use --force to break it", target.Owner)
	}
	if err != nil {
		return err
	}
	return writeLFS(w, http.StatusOK, map[string]any{"lock": lfsLockOf(l)})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
