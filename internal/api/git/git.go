package git

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// Repos gives access to the server's repositories.
type Repos interface {
	Get(ns string) (*repo.Repo, error)
}

// Server serves Git smart HTTP under /git/{namespace}.git/.
type Server struct {
	Repos Repos
	Auth  *auth.Chain
	ACL   *auth.Authorizer
	Log   *slog.Logger
}

// httpError is an error with an HTTP status; its message is shown to the
// Git client.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

var errNotFound = &httpError{http.StatusNotFound, "repository not found"}

// Prefix is the URL prefix the Git handler is mounted at.
const Prefix = "/git/"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.serve(w, r); err != nil {
		var he *httpError
		if !errors.As(err, &he) {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.Log.Error("git request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			he = &httpError{http.StatusInternalServerError, "internal error"}
		}
		if he.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="F1"`)
		}
		http.Error(w, he.msg, he.status)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) error {
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	ns, rest, ok := strings.Cut(path, "/")
	if !ok {
		return errNotFound
	}
	ns = strings.TrimSuffix(ns, ".git")
	caller, err := s.authenticate(r)
	if err != nil {
		return err
	}
	g, err := s.ACL.For(r.Context(), caller, ns)
	if err != nil {
		return err
	}
	rp, err := s.Repos.Get(ns)
	if err != nil || !rp.Meta().Git || !(g.Any(serverdb.PermRead) || g.Any(serverdb.PermWrite)) {
		return errNotFound // inaccessible namespaces look like missing ones (ADR-0006)
	}
	if strings.HasPrefix(rest, "info/lfs/") {
		return s.lfs(w, r, rp, caller, g, strings.TrimPrefix(rest, "info/lfs/"))
	}
	service := r.URL.Query().Get("service")
	push := false
	switch {
	case r.Method == http.MethodGet && rest == "info/refs" && service == "git-upload-pack",
		r.Method == http.MethodPost && rest == "git-upload-pack":
	case r.Method == http.MethodGet && rest == "info/refs" && service == "git-receive-pack",
		r.Method == http.MethodPost && rest == "git-receive-pack":
		push = true
	default:
		return errNotFound
	}
	// Git reads address objects by id: whole-namespace read (D13).
	if !g.Whole(serverdb.PermRead) {
		return &httpError{http.StatusForbidden, "Git access needs read permission on the whole namespace"}
	}
	if push && !g.Any(serverdb.PermWrite) {
		return &httpError{http.StatusForbidden, "push needs write permission"}
	}
	version := protocolVersion(r)
	ctx := r.Context()
	if r.Method == http.MethodGet {
		if push {
			return s.advertiseReceive(ctx, w, rp, version)
		}
		return s.advertise(ctx, w, rp, version)
	}
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return badRequest("invalid gzip body")
		}
		defer zr.Close()
		body = zr
	}
	if push {
		return s.receive(ctx, w, body, rp, caller, g)
	}
	if version == 2 {
		return s.serveV2(ctx, w, body, rp)
	}
	return s.serveV0(ctx, w, body, rp)
}

// protocolVersion reads the Git-Protocol header ("version=2").
func protocolVersion(r *http.Request) int {
	for _, kv := range strings.Split(r.Header.Get("Git-Protocol"), ":") {
		switch kv {
		case "version=2":
			return 2
		case "version=1":
			return 1
		}
	}
	return 0
}

// authenticate accepts HTTP Basic with a token as password (any user
// name), or a bearer token (doc/plan.md, "Credentials per protocol").
func (s *Server) authenticate(r *http.Request) (*auth.Caller, error) {
	token := ""
	if _, pw, ok := r.BasicAuth(); ok {
		token = pw
	} else if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		token = strings.TrimSpace(t)
	}
	c, err := s.Auth.Authenticate(r.Context(), token, r.RemoteAddr)
	if err != nil {
		return nil, &httpError{http.StatusUnauthorized, "authentication required"}
	}
	return c, nil
}

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, 64<<10) }
