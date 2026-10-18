package native

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/serverdb"
	api "github.com/trukeio/f1/pkg/f1client"
)

// ACL administration (ADR-0020, item 6): administrators of a namespace
// (admin at its root) and server administrators manage its ACL; the ACL
// of "*" needs a server administrator.

// aclAdmin checks that the caller administers the request's namespace and
// returns its name. A namespace the caller cannot see is 404, as for
// every other operation.
func (s *Server) aclAdmin(r *http.Request) (string, *auth.Caller, error) {
	name := r.PathValue("ns")
	if name == serverdb.ServerNamespace {
		c := callerFrom(r.Context())
		g, err := s.ACL.For(r.Context(), c, name)
		if err != nil {
			return "", nil, err
		}
		return name, c, need(g.ServerAdmin())
	}
	_, c, g, err := s.ns(r)
	if err != nil {
		return "", nil, err
	}
	return name, c, need(g.Allow("", serverdb.PermAdmin))
}

func (s *Server) getACL(w http.ResponseWriter, r *http.Request) error {
	ns, _, err := s.aclAdmin(r)
	if err != nil {
		return err
	}
	entries, err := s.DB.ACL(r.Context(), ns)
	if err != nil {
		return err
	}
	out := make([]api.ACLEntry, 0, len(entries))
	known := map[string]bool{}
	for _, e := range entries {
		k, seen := known[e.Principal]
		if !seen {
			if k, err = s.DB.Known(r.Context(), e.Principal); err != nil {
				return err
			}
			known[e.Principal] = k
		}
		perms := e.Perms
		if perms == nil {
			perms = []string{}
		}
		out = append(out, api.ACLEntry{Principal: e.Principal, Prefix: e.Prefix, Perms: perms, Known: k})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		return out[i].Principal < out[j].Principal
	})
	return writeJSON(w, out)
}

func (s *Server) putACL(w http.ResponseWriter, r *http.Request) error {
	ns, c, err := s.aclAdmin(r)
	if err != nil {
		return err
	}
	var req api.ACLEntry
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := serverdb.ValidPrincipal(req.Principal); err != nil {
		return badRequest("%v", err)
	}
	var perms []string
	for _, p := range req.Perms {
		switch p {
		case "none":
		case serverdb.PermRead, serverdb.PermWrite, serverdb.PermLock, serverdb.PermAdmin:
			perms = append(perms, p)
		default:
			return badRequest("unknown permission %q: use read, write, lock, admin or none", p)
		}
	}
	if len(perms) > 0 && len(perms) < len(req.Perms) {
		return badRequest("none cannot be combined with other permissions")
	}
	e := serverdb.ACLEntry{Namespace: ns, Principal: req.Principal, Prefix: req.Prefix, Perms: perms}
	if err := aclError(s.DB.SetACLGuarded(r.Context(), e)); err != nil {
		return err
	}
	s.adminChange(c, "acl set", "namespace", ns, "target", req.Principal, "prefix", req.Prefix, "perms", strings.Join(perms, ","))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) deleteACL(w http.ResponseWriter, r *http.Request) error {
	ns, c, err := s.aclAdmin(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	principal, prefix := q.Get("principal"), q.Get("prefix")
	if principal == "" {
		return badRequest("principal is required")
	}
	if err := aclError(s.DB.RemoveACLGuarded(r.Context(), ns, principal, prefix)); err != nil {
		return err
	}
	s.adminChange(c, "acl remove", "namespace", ns, "target", principal, "prefix", prefix)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func aclError(err error) error {
	switch {
	case errors.Is(err, serverdb.ErrLastAdmin):
		return newErr(http.StatusConflict, api.CodeLastAdmin, "the server would have no enabled administrator")
	case errors.Is(err, serverdb.ErrNoACLEntry):
		return newErr(http.StatusNotFound, api.CodeNotFound, "no such ACL entry")
	}
	return err
}

// adminChange logs a change made through the administration endpoints as
// a security event (ADR-0020, item 7). Secrets are never among args.
func (s *Server) adminChange(c *auth.Caller, op string, args ...any) {
	s.Log.Info("security: admin change", append([]any{"principal", c.Principal, "op", op}, args...)...)
}
