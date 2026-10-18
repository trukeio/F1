package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// adminCacheTTL is how long f1web remembers whether a user administers a
// namespace (ADR-0020, item 8).
const adminCacheTTL = time.Minute

// serverNS is the namespace of the server's ACL, at /-/admin/acl.
const serverNS = "*"

// aclNamespace is the namespace whose ACL a request is about.
func aclNamespace(r *http.Request) string {
	if ns := r.PathValue("ns"); ns != "" {
		return ns
	}
	return serverNS
}

// accessURL is the access page of namespace ns.
func accessURL(ns string) string {
	if ns == serverNS {
		return "/-/admin/acl"
	}
	return "/" + url.PathEscape(ns) + "/settings/access"
}

// perms are the permissions of an ACL entry, in display order.
var perms = []string{"read", "write", "lock", "admin"}

// accessRoutes registers a namespace's access settings (ADR-0020, item 8).
func (s *Server) accessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{ns}/settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+url.PathEscape(r.PathValue("ns"))+"/settings/access", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /{ns}/settings/access", s.recent(s.access))
	mux.HandleFunc("POST /{ns}/settings/access", s.recent(s.setAccess))
	mux.HandleFunc("GET /{ns}/settings/access/remove", s.recent(s.confirmRemoveAccess))
	mux.HandleFunc("POST /{ns}/settings/access/remove", s.recent(s.removeAccess))
	mux.HandleFunc("GET /-/admin/acl", s.recent(s.access))
	mux.HandleFunc("POST /-/admin/acl", s.recent(s.setAccess))
	mux.HandleFunc("GET /-/admin/acl/remove", s.recent(s.confirmRemoveAccess))
	mux.HandleFunc("POST /-/admin/acl/remove", s.recent(s.removeAccess))
}

// recent wraps a page that changes access: it needs a sign-in no older
// than admin_max_age, or the user signs in again first (item 12).
func (s *Server) recent(fn func(w http.ResponseWriter, r *http.Request, rq *request) error) http.HandlerFunc {
	maxAge, _ := s.cfg.adminMaxAge()
	return s.page(func(w http.ResponseWriter, r *http.Request, rq *request) error {
		if time.Since(rq.sess.signedIn) > maxAge {
			next := r.URL.RequestURI()
			if r.Method != http.MethodGet {
				// The form is lost; come back to its page.
				next = r.URL.Path
				if i := strings.Index(next, "/settings/access"); i >= 0 {
					next = next[:i] + "/settings/access"
				} else if strings.HasPrefix(next, "/-/admin/") {
					next = "/-/admin"
				}
			}
			http.Redirect(w, r, "/-/login?reauth=1&next="+urlQueryEscape(next), http.StatusSeeOther)
			return nil
		}
		return fn(w, r, rq)
	})
}

// nsAdmin reports whether the user administers namespace ns: whether F1
// lets them read its ACL. The answer is cached in the session.
func (s *Server) nsAdmin(ctx context.Context, rq *request, ns string) bool {
	sess := rq.sess
	sess.cacheMu.Lock()
	c, ok := sess.admin[ns]
	sess.cacheMu.Unlock()
	if ok && time.Since(c.at) < adminCacheTTL {
		return c.ok
	}
	_, err := rq.api.ACL(ctx, ns)
	switch code := f1client.Code(err); {
	case err == nil:
		c.ok = true
	case code == f1client.CodeForbidden, code == f1client.CodeNotFound:
		c.ok = false
	default:
		return false // not cached: F1 could not answer
	}
	c.at = time.Now()
	sess.cacheMu.Lock()
	if sess.admin == nil {
		sess.admin = map[string]adminCheck{}
	}
	sess.admin[ns] = c
	sess.cacheMu.Unlock()
	return c.ok
}

func (s *Server) forgetAdmin(sess *session, ns string) {
	sess.cacheMu.Lock()
	delete(sess.admin, ns)
	sess.cacheMu.Unlock()
}

// accessPage is a namespace's ACL with a form to change it and a check of
// what applies to a path.
type accessPage struct {
	*repoView        // nil for the server's ACL
	Server    bool   // the ACL of "*"
	Base      string // this page's URL
	Entries   []f1client.ACLEntry
	Perms     []string
	// Form is the entry in the form: empty, picked with Edit, or sent and
	// refused.
	Form f1client.ACLEntry
	// Check is the result of the "check access" box, nil when not asked.
	Check *accessCheck
}

// Has reports whether the form's entry has permission p.
func (p *accessPage) Has(perm string) bool {
	for _, x := range p.Form.Perms {
		if x == perm {
			return true
		}
	}
	return false
}

// accessCheck is what applies to Principals on Path.
type accessCheck struct {
	Principals string // as typed
	Path       string
	Perms      []string            // granted; empty: denied
	Deciding   []f1client.ACLEntry // the entries at the longest matching prefix
}

func (s *Server) access(w http.ResponseWriter, r *http.Request, rq *request) error {
	q := r.URL.Query()
	form := f1client.ACLEntry{Principal: q.Get("principal"), Prefix: q.Get("prefix")}
	return s.showAccess(w, r, rq, http.StatusOK, "", form)
}

// showAccess renders the access page with a message and the form's entry.
func (s *Server) showAccess(w http.ResponseWriter, r *http.Request, rq *request, status int, msg string, form f1client.ACLEntry) error {
	ctx := r.Context()
	ns := aclNamespace(r)
	var v *repoView
	if ns != serverNS {
		var err error
		if v, err = s.loadRepo(ctx, rq, ns, "", "", "settings"); err != nil {
			return err
		}
	}
	entries, err := rq.api.ACL(ctx, ns)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sort.Slice(e.Perms, func(i, j int) bool { return permIndex(e.Perms[i]) < permIndex(e.Perms[j]) })
	}
	p := &accessPage{repoView: v, Server: ns == serverNS, Base: accessURL(ns), Entries: entries, Perms: perms, Form: form}
	if form.Perms == nil {
		// An entry picked with Edit: its current permissions.
		for _, e := range entries {
			if e.Principal == form.Principal && e.Prefix == form.Prefix {
				p.Form.Perms = e.Perms
			}
		}
	}
	q := r.URL.Query()
	if who := strings.TrimSpace(q.Get("check")); who != "" {
		p.Check = checkAccess(entries, who, q.Get("path"))
	}
	title := "Access · " + ns
	if p.Server {
		title = "Server access"
	}
	d := pageData{User: rq.sess.principal, Title: title, Message: msg, Data: p}
	s.render(w, r, status, "access", d)
	return nil
}

// checkAccess applies ADR-0006's rules to the entries: for a path, the
// longest prefix with an entry for any of the principals decides, and the
// entries at that prefix combine. admin implies every permission.
func checkAccess(entries []f1client.ACLEntry, principals, path string) *accessCheck {
	who := map[string]bool{}
	for _, p := range strings.FieldsFunc(principals, func(r rune) bool { return r == ',' || r == ' ' }) {
		who[p] = true
	}
	c := &accessCheck{Principals: principals, Path: path}
	best := -1
	for _, e := range entries {
		if !who[e.Principal] || !strings.HasPrefix(path, e.Prefix) {
			continue
		}
		switch {
		case len(e.Prefix) > best:
			best, c.Deciding = len(e.Prefix), []f1client.ACLEntry{e}
		case len(e.Prefix) == best:
			c.Deciding = append(c.Deciding, e)
		}
	}
	granted := map[string]bool{}
	for _, e := range c.Deciding {
		for _, p := range e.Perms {
			granted[p] = true
		}
	}
	for _, p := range perms {
		if granted[p] || granted["admin"] {
			c.Perms = append(c.Perms, p)
		}
	}
	return c
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return s.showAccess(w, r, rq, http.StatusBadRequest, "The form is too large.", f1client.ACLEntry{})
	}
	ns := aclNamespace(r)
	e := f1client.ACLEntry{Principal: strings.TrimSpace(r.PostFormValue("principal")), Prefix: r.PostFormValue("prefix"),
		Perms: r.PostForm["perm"]}
	sort.Slice(e.Perms, func(i, j int) bool { return permIndex(e.Perms[i]) < permIndex(e.Perms[j]) })
	sent := e
	if len(e.Perms) == 0 {
		e.Perms = []string{"none"}
		sent.Perms = []string{}
	}
	err := rq.api.SetACL(r.Context(), ns, e)
	if msg, ok := refused(err); ok {
		return s.showAccess(w, r, rq, http.StatusBadRequest, msg, sent)
	}
	if err != nil {
		return err
	}
	return s.afterAccessChange(w, r, rq, ns)
}

func permIndex(p string) int {
	for i, x := range perms {
		if x == p {
			return i
		}
	}
	return len(perms)
}

func (s *Server) confirmRemoveAccess(w http.ResponseWriter, r *http.Request, rq *request) error {
	ns := aclNamespace(r)
	var v *repoView
	if ns != serverNS {
		var err error
		if v, err = s.loadRepo(r.Context(), rq, ns, "", "", "settings"); err != nil {
			return err
		}
	}
	q := r.URL.Query()
	principal, prefix := q.Get("principal"), q.Get("prefix")
	where := "the whole namespace"
	if prefix != "" {
		where = "paths starting with " + prefix
	}
	if ns == serverNS {
		where = "the server"
	}
	back := accessURL(ns)
	return s.show(w, r, rq, "confirm", "Remove access", &confirmPage{Repo: v, Admin: ns == serverNS, Tab: "acl",
		Question: "Remove the entry of " + principal + " for " + where + "?",
		Detail:   "Entries for shorter prefixes then apply to these paths, if there are any; otherwise access is denied.",
		Action:   back + "/remove", Button: "Remove", Back: back,
		Fields: map[string]string{"principal": principal, "prefix": prefix}})
}

func (s *Server) removeAccess(w http.ResponseWriter, r *http.Request, rq *request) error {
	if err := parseForm(w, r); err != nil {
		return err
	}
	ns := aclNamespace(r)
	err := rq.api.DeleteACL(r.Context(), ns, r.PostFormValue("principal"), r.PostFormValue("prefix"))
	if msg, ok := refused(err); ok {
		return s.showAccess(w, r, rq, http.StatusConflict, msg, f1client.ACLEntry{})
	}
	if err != nil && f1client.Code(err) != f1client.CodeNotFound {
		return err
	}
	return s.afterAccessChange(w, r, rq, ns)
}

// afterAccessChange returns to the access page, or to the namespace (the
// home page for the server's ACL) if the user no longer administers it.
func (s *Server) afterAccessChange(w http.ResponseWriter, r *http.Request, rq *request, ns string) error {
	s.forgetAdmin(rq.sess, ns)
	to := "/"
	if ns != serverNS {
		to += url.PathEscape(ns)
	}
	if s.nsAdmin(r.Context(), rq, ns) {
		to = accessURL(ns)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}
