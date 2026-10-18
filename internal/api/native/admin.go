package native

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/serverdb"
	api "github.com/trukeio/f1/pkg/f1client"
)

// Server administration (ADR-0020, item 5): users, groups and every
// credential, for server administrators only. f1d's identity commands do
// the same on the server host, unguarded.

// serverAdmin returns the caller if it is a server administrator.
func (s *Server) serverAdmin(r *http.Request) (*auth.Caller, error) {
	c := callerFrom(r.Context())
	g, err := s.ACL.For(r.Context(), c, serverdb.ServerNamespace)
	if err != nil {
		return nil, err
	}
	return c, need(g.ServerAdmin())
}

// withPrefix adds kind ("user:" or "group:") to a bare name, as f1d does.
func withPrefix(kind, name string) string {
	if strings.HasPrefix(name, kind) {
		return name
	}
	return kind + name
}

func principalInfo(p serverdb.Principal) api.Principal {
	out := api.Principal{Name: p.Name, Issuer: p.Issuer, Groups: p.Groups, Disabled: p.Disabled, Created: p.Created}
	if out.Groups == nil {
		out.Groups = []string{}
	}
	if !p.GroupsSeen.IsZero() {
		out.GroupsSeen = &p.GroupsSeen
	}
	return out
}

// identityError maps serverdb's errors to API errors.
func identityError(err error) error {
	switch {
	case errors.Is(err, serverdb.ErrNoPrincipal):
		return newErr(http.StatusNotFound, api.CodeNotFound, "%v", err)
	case errors.Is(err, serverdb.ErrUserExists), errors.Is(err, serverdb.ErrPrincipalTaken):
		return newErr(http.StatusConflict, api.CodeExists, "%v", err)
	case errors.Is(err, serverdb.ErrLastAdmin):
		return newErr(http.StatusConflict, api.CodeLastAdmin, "the server would have no enabled administrator")
	case errors.Is(err, serverdb.ErrNoToken):
		return newErr(http.StatusNotFound, api.CodeNotFound, "no such token or key")
	case errors.Is(err, serverdb.ErrInvalid):
		// A bad name, or the groups of an OIDC user.
		return badRequest("%s", strings.TrimPrefix(err.Error(), serverdb.ErrInvalid.Error()+": "))
	}
	return err
}

func (s *Server) adminPrincipals(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.serverAdmin(r); err != nil {
		return err
	}
	ps, err := s.DB.Principals(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.Principal, 0, len(ps))
	for _, p := range ps {
		out = append(out, principalInfo(p))
	}
	return writeJSON(w, out)
}

func groupNames(groups []string) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, withPrefix("group:", g))
		}
	}
	return out
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	var req api.CreateUserRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	name, groups := withPrefix("user:", strings.TrimSpace(req.Name)), groupNames(req.Groups)
	if err := identityError(s.DB.AddUser(r.Context(), name, groups)); err != nil {
		return err
	}
	s.adminChange(c, "user add", "target", name, "groups", strings.Join(groups, ","))
	p, err := s.DB.LookupPrincipal(r.Context(), name)
	if err != nil {
		return err
	}
	p.Groups = groups
	w.WriteHeader(http.StatusCreated)
	return writeJSON(w, principalInfo(p))
}

func (s *Server) adminSetGroups(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	var req struct {
		Groups []string `json:"groups"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	name, groups := withPrefix("user:", r.PathValue("name")), groupNames(req.Groups)
	if err := identityError(s.DB.SetUserGroupsGuarded(r.Context(), name, groups)); err != nil {
		return err
	}
	s.adminChange(c, "user groups", "target", name, "groups", strings.Join(groups, ","))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminSetDisabled(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Disabled == nil {
		return badRequest("disabled is required")
	}
	name := withPrefix("user:", r.PathValue("principal"))
	if *req.Disabled && name == c.Principal {
		return badRequest("you cannot disable yourself")
	}
	if err := identityError(s.DB.SetDisabledGuarded(r.Context(), name, *req.Disabled)); err != nil {
		return err
	}
	op := "user enable"
	if *req.Disabled {
		op = "user disable"
	}
	s.adminChange(c, op, "target", name)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminTokens(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.serverAdmin(r); err != nil {
		return err
	}
	principal := r.URL.Query().Get("principal")
	if principal != "" {
		principal = withPrefix("user:", principal)
	}
	toks, err := s.DB.Tokens(r.Context(), principal)
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

func (s *Server) adminCreateToken(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	var req api.AdminTokenRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if len(req.Description) > maxTokenDescription {
		return badRequest("the description is longer than %d bytes", maxTokenDescription)
	}
	var ttl time.Duration
	if req.Expires != nil {
		if ttl = time.Until(*req.Expires); ttl <= 0 {
			return badRequest("expires is in the past")
		}
	}
	principal := withPrefix("user:", strings.TrimSpace(req.Principal))
	t, secret, err := s.DB.CreateToken(r.Context(), principal, req.Description, ttl)
	if err != nil {
		return identityError(err)
	}
	s.adminChange(c, "token create", "target", principal, "token", t.ID, "expires", t.Expires)
	out := tokenInfo(t)
	out.Secret = secret
	return writeJSON(w, out)
}

func (s *Server) adminDeleteToken(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	if err := identityError(s.DB.RevokeToken(r.Context(), "", r.PathValue("id"))); err != nil {
		return err
	}
	s.adminChange(c, "token revoke", "token", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminS3Keys(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.serverAdmin(r); err != nil {
		return err
	}
	principal := r.URL.Query().Get("principal")
	if principal != "" {
		principal = withPrefix("user:", principal)
	}
	keys, err := s.DB.S3Keys(r.Context(), principal)
	if err != nil {
		return err
	}
	out := []api.S3Key{}
	for _, k := range keys {
		out = append(out, api.S3Key{AccessKey: k.AccessKey, Principal: k.Principal, Created: k.Created})
	}
	return writeJSON(w, out)
}

func (s *Server) adminDeleteS3Key(w http.ResponseWriter, r *http.Request) error {
	c, err := s.serverAdmin(r)
	if err != nil {
		return err
	}
	if err := identityError(s.DB.RevokeS3Key(r.Context(), "", r.PathValue("key"))); err != nil {
		return err
	}
	s.adminChange(c, "s3key revoke", "key", r.PathValue("key"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
