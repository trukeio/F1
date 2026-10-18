package web

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// adminTokenDays are the expiries offered for a token an administrator
// creates; 0 never expires, as with f1d token create.
var adminTokenDays = []int{30, 90, 365, 0}

// presets are the namespace presets offered on the namespaces page.
var presets = []string{"", "git", "svn", "s3"}

// adminRoutes registers server administration (ADR-0020, item 8). Every
// page needs a recent sign-in; F1 refuses users who are not server
// administrators.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/-/admin/users", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /-/admin/users", s.recent(s.adminUsers))
	mux.HandleFunc("POST /-/admin/users", s.recent(s.adminCreateUser))
	mux.HandleFunc("GET /-/admin/users/{name}", s.recent(s.adminUser))
	mux.HandleFunc("POST /-/admin/users/{name}/groups", s.recent(s.adminSetGroups))
	mux.HandleFunc("GET /-/admin/users/{name}/disable", s.recent(s.adminConfirmDisable))
	mux.HandleFunc("POST /-/admin/users/{name}/disable", s.recent(s.adminSetDisabled(true)))
	mux.HandleFunc("POST /-/admin/users/{name}/enable", s.recent(s.adminSetDisabled(false)))
	mux.HandleFunc("POST /-/admin/users/{name}/tokens", s.recent(s.adminCreateToken))
	mux.HandleFunc("GET /-/admin/tokens", s.recent(s.adminTokens))
	mux.HandleFunc("GET /-/admin/tokens/{id}/revoke", s.recent(s.adminConfirmRevokeToken))
	mux.HandleFunc("POST /-/admin/tokens/{id}/revoke", s.recent(s.adminRevokeToken))
	mux.HandleFunc("GET /-/admin/s3keys", s.recent(s.adminS3Keys))
	mux.HandleFunc("GET /-/admin/s3keys/{key}/revoke", s.recent(s.adminConfirmRevokeS3Key))
	mux.HandleFunc("POST /-/admin/s3keys/{key}/revoke", s.recent(s.adminRevokeS3Key))
	mux.HandleFunc("GET /-/admin/namespaces", s.recent(s.adminNamespaces))
	mux.HandleFunc("POST /-/admin/namespaces", s.recent(s.adminCreateNamespace))
}

// adminPage is the data of the administration pages.
type adminPage struct {
	Tab        string // users, tokens, s3keys, namespaces
	Principals []f1client.Principal
	Principal  *f1client.Principal // the user page's
	Admins     map[string]bool     // principals that are server administrators
	Tokens     []f1client.Token
	S3Keys     []f1client.S3Key
	Namespaces []f1client.Namespace
	Days       []int
	Presets    []string
	Now        time.Time
	Secret     *secret
	Form       map[string]string // values of a refused form
}

// Local reports whether the user page shows a local user.
func (p *adminPage) Local() bool { return p.Principal != nil && p.Principal.Issuer == "local" }

// Expired reports whether t has expired.
func (p *adminPage) Expired(t f1client.Token) bool {
	return t.Expires != nil && !p.Now.Before(*t.Expires)
}

// userURL is the administration page of a principal.
func userURL(principal string) string {
	return "/-/admin/users/" + url.PathEscape(strings.TrimPrefix(principal, "user:"))
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, rq *request, status int, title, msg string, p *adminPage) error {
	p.Now = time.Now()
	s.render(w, r, status, "admin_"+p.Tab, pageData{User: rq.sess.principal, Title: title, Message: msg, Data: p})
	return nil
}

// serverAdmins returns the principals that the server's ACL makes server
// administrators: users named in an admin entry at the root, and the
// members of groups named in one.
func serverAdmins(acl []f1client.ACLEntry, ps []f1client.Principal) map[string]bool {
	admin := map[string]bool{}
	for _, e := range acl {
		if e.Prefix != "" {
			continue
		}
		for _, p := range e.Perms {
			if p == "admin" {
				admin[e.Principal] = true
			}
		}
	}
	out := map[string]bool{}
	for _, p := range ps {
		if admin[p.Name] {
			out[p.Name] = true
		}
		for _, g := range p.Groups {
			if admin[g] {
				out[p.Name] = true
			}
		}
	}
	return out
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request, rq *request) error {
	return s.showUsers(w, r, rq, http.StatusOK, "", nil)
}

func (s *Server) showUsers(w http.ResponseWriter, r *http.Request, rq *request, status int, msg string, form map[string]string) error {
	ps, err := rq.api.AdminPrincipals(r.Context())
	if err != nil {
		return err
	}
	acl, err := rq.api.ACL(r.Context(), serverNS)
	if err != nil {
		return err
	}
	p := &adminPage{Tab: "users", Principals: ps, Admins: serverAdmins(acl, ps), Form: form}
	return s.renderAdmin(w, r, rq, status, "Users", msg, p)
}

// splitList splits a comma- or space-separated list.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return s.showUsers(w, r, rq, http.StatusBadRequest, "The form is too large.", nil)
	}
	name, groups := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("groups")
	u, err := rq.api.AdminCreateUser(r.Context(), f1client.CreateUserRequest{Name: name, Groups: splitList(groups)})
	if msg, ok := refusedForm(err); ok {
		return s.showUsers(w, r, rq, http.StatusBadRequest, msg, map[string]string{"name": name, "groups": groups})
	}
	if err != nil {
		return err
	}
	http.Redirect(w, r, userURL(u.Name), http.StatusSeeOther)
	return nil
}

// refusedForm is refused, for F1's other answers to a form that are the
// user's to fix: a name that is taken.
func refusedForm(err error) (string, bool) {
	if msg, ok := refused(err); ok {
		return msg, true
	}
	if f1client.Code(err) == f1client.CodeExists {
		return "F1 refused this: the name is taken.", true
	}
	return "", false
}

func (s *Server) adminUser(w http.ResponseWriter, r *http.Request, rq *request) error {
	return s.showUser(w, r, rq, http.StatusOK, "", nil)
}

// showUser renders a principal's page, with a message or a new token.
func (s *Server) showUser(w http.ResponseWriter, r *http.Request, rq *request, status int, msg string, sec *secret) error {
	ctx := r.Context()
	name := "user:" + r.PathValue("name")
	ps, err := rq.api.AdminPrincipals(ctx)
	if err != nil {
		return err
	}
	p := &adminPage{Tab: "user", Days: adminTokenDays, Secret: sec}
	for i := range ps {
		if ps[i].Name == name {
			p.Principal = &ps[i]
		}
	}
	if p.Principal == nil {
		return errNotFound
	}
	acl, err := rq.api.ACL(ctx, serverNS)
	if err != nil {
		return err
	}
	p.Admins = serverAdmins(acl, ps)
	if p.Tokens, err = rq.api.AdminTokens(ctx, name); err != nil {
		return err
	}
	if p.S3Keys, err = rq.api.AdminS3Keys(ctx, name); err != nil {
		return err
	}
	return s.renderAdmin(w, r, rq, status, strings.TrimPrefix(name, "user:"), msg, p)
}

func (s *Server) adminSetGroups(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	name := r.PathValue("name")
	err := rq.api.AdminSetGroups(r.Context(), name, splitList(r.PostFormValue("groups")))
	if msg, ok := refused(err); ok {
		return s.showUser(w, r, rq, http.StatusBadRequest, msg, nil)
	}
	if err != nil {
		return err
	}
	s.forgetAdmin(rq.sess, serverNS)
	http.Redirect(w, r, userURL(name), http.StatusSeeOther)
	return nil
}

func (s *Server) adminConfirmDisable(w http.ResponseWriter, r *http.Request, rq *request) error {
	name := r.PathValue("name")
	return s.show(w, r, rq, "confirm", "Disable user", &confirmPage{Admin: true, Tab: "users",
		Question: "Disable " + name + "?",
		Detail:   "F1 refuses every token, S3 key and OIDC sign-in of a disabled user until they are enabled again. Nothing is deleted.",
		Action:   userURL(name) + "/disable", Button: "Disable", Back: userURL(name)})
}

func (s *Server) adminSetDisabled(disabled bool) func(w http.ResponseWriter, r *http.Request, rq *request) error {
	return func(w http.ResponseWriter, r *http.Request, rq *request) error {
		name := r.PathValue("name")
		err := rq.api.AdminSetDisabled(r.Context(), name, disabled)
		if msg, ok := refused(err); ok {
			return s.showUser(w, r, rq, http.StatusBadRequest, msg, nil)
		}
		if err != nil {
			return err
		}
		http.Redirect(w, r, userURL(name), http.StatusSeeOther)
		return nil
	}
}

func (s *Server) adminCreateToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return s.showUser(w, r, rq, http.StatusBadRequest, "The form is too large.", nil)
	}
	req := f1client.AdminTokenRequest{Principal: r.PathValue("name"), Description: strings.TrimSpace(r.PostFormValue("description"))}
	days, err := strconv.Atoi(r.PostFormValue("days"))
	if err != nil || days < 0 {
		return s.showUser(w, r, rq, http.StatusBadRequest, "Choose when the token expires.", nil)
	}
	if days > 0 {
		exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		req.Expires = &exp
	}
	t, err := rq.api.AdminCreateToken(r.Context(), req)
	if msg, ok := refused(err); ok {
		return s.showUser(w, r, rq, http.StatusBadRequest, msg, nil)
	}
	if err != nil {
		return err
	}
	return s.showUser(w, r, rq, http.StatusOK, "", &secret{ID: t.ID, Value: t.Secret})
}

func (s *Server) adminTokens(w http.ResponseWriter, r *http.Request, rq *request) error {
	toks, err := rq.api.AdminTokens(r.Context(), "")
	if err != nil {
		return err
	}
	sort.SliceStable(toks, func(i, j int) bool { return toks[i].Principal < toks[j].Principal })
	return s.renderAdmin(w, r, rq, http.StatusOK, "Tokens", "", &adminPage{Tab: "tokens", Tokens: toks})
}

// back is where a revocation returns to: the page it was asked from, if
// that is an administration page.
func back(r *http.Request, def string) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/-/admin/") && safeNext(b) == b {
		return b
	}
	return def
}

func (s *Server) adminConfirmRevokeToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	id := r.PathValue("id")
	to := back(r, "/-/admin/tokens")
	return s.show(w, r, rq, "confirm", "Revoke token", &confirmPage{Admin: true, Tab: "tokens",
		Question: "Revoke token " + id + "?",
		Detail:   "Programs that use it stop working at once.",
		Action:   "/-/admin/tokens/" + url.PathEscape(id) + "/revoke", Button: "Revoke", Back: to,
		Fields: map[string]string{"back": to}})
}

func (s *Server) adminRevokeToken(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	if err := rq.api.AdminDeleteToken(r.Context(), r.PathValue("id")); err != nil && f1client.Code(err) != f1client.CodeNotFound {
		return err
	}
	http.Redirect(w, r, back(r, "/-/admin/tokens"), http.StatusSeeOther)
	return nil
}

func (s *Server) adminS3Keys(w http.ResponseWriter, r *http.Request, rq *request) error {
	keys, err := rq.api.AdminS3Keys(r.Context(), "")
	if err != nil {
		return err
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Principal < keys[j].Principal })
	return s.renderAdmin(w, r, rq, http.StatusOK, "S3 keys", "", &adminPage{Tab: "s3keys", S3Keys: keys})
}

func (s *Server) adminConfirmRevokeS3Key(w http.ResponseWriter, r *http.Request, rq *request) error {
	key := r.PathValue("key")
	to := back(r, "/-/admin/s3keys")
	return s.show(w, r, rq, "confirm", "Revoke S3 key", &confirmPage{Admin: true, Tab: "s3keys",
		Question: "Revoke S3 key " + key + "?",
		Detail:   "S3 tools and programs that use it stop working at once.",
		Action:   "/-/admin/s3keys/" + url.PathEscape(key) + "/revoke", Button: "Revoke", Back: to,
		Fields: map[string]string{"back": to}})
}

func (s *Server) adminRevokeS3Key(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	if err := rq.api.AdminDeleteS3Key(r.Context(), r.PathValue("key")); err != nil && f1client.Code(err) != f1client.CodeNotFound {
		return err
	}
	http.Redirect(w, r, back(r, "/-/admin/s3keys"), http.StatusSeeOther)
	return nil
}

func (s *Server) adminNamespaces(w http.ResponseWriter, r *http.Request, rq *request) error {
	return s.showNamespaces(w, r, rq, http.StatusOK, "", nil)
}

func (s *Server) showNamespaces(w http.ResponseWriter, r *http.Request, rq *request, status int, msg string, form map[string]string) error {
	// Only server administrators may create namespaces; reading the
	// server's ACL checks that before listing.
	if _, err := rq.api.ACL(r.Context(), serverNS); err != nil {
		return err
	}
	nss, err := rq.api.Namespaces(r.Context())
	if err != nil {
		return err
	}
	p := &adminPage{Tab: "namespaces", Namespaces: nss, Presets: presets, Form: form}
	return s.renderAdmin(w, r, rq, status, "Namespaces", msg, p)
}

func (s *Server) adminCreateNamespace(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	name, preset := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("preset")
	_, err := rq.api.CreateNamespace(r.Context(), f1client.CreateNamespaceRequest{Name: name, Preset: preset})
	if msg, ok := refusedForm(err); ok {
		return s.showNamespaces(w, r, rq, http.StatusBadRequest, msg, map[string]string{"name": name, "preset": preset})
	}
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/"+url.PathEscape(name), http.StatusSeeOther)
	return nil
}
