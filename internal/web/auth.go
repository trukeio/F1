package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// Cookies. With an https public URL the session cookie takes the
// __Host- prefix, which browsers accept only if Secure, with Path=/ and
// no Domain.
const (
	sessionCookie = "f1web_session"
	loginCookie   = "f1web_login" // binds a pending OIDC login to the browser
)

func (s *Server) sessionCookieName() string {
	if s.cfg.secure() {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

// session returns the request's session, or nil.
func (s *Server) session(r *http.Request) *session {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return nil
	}
	return s.sess.get(c.Value)
}

func (s *Server) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: s.cfg.secure(), SameSite: http.SameSiteLaxMode})
}

// safeNext returns next if it is a path on this site, and "/" otherwise,
// so that a crafted link cannot send a user elsewhere after sign-in.
// Browsers read "\\" as "/" and drop tabs and newlines in URLs, so
// "/\\evil.example" and "/\t/evil.example" would both name another host.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	for _, c := range []byte(next) {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return next
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// loginData is the sign-in page's data.
type loginData struct {
	Next       string
	Reauth     bool   // signing in again for administration
	OIDCName   string // "" without OIDC
	TokenLogin bool
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	reauth := r.URL.Query().Get("reauth") == "1"
	if s.session(r) != nil && !reauth {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	msg := ""
	if reauth {
		msg = "Sign in again to change administration settings: your sign-in is older than " + s.cfg.AdminMaxAge + "."
	}
	s.renderLogin(w, r, http.StatusOK, next, msg)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, next, msg string) {
	d := loginData{Next: next, TokenLogin: s.cfg.TokenLogin, Reauth: r.URL.Query().Get("reauth") == "1"}
	if s.oidc != nil {
		d.OIDCName = s.oidc.cfg.Name
	}
	s.render(w, r, status, "login", pageData{Title: "Sign in", Message: msg, Data: d})
}

// loginOIDC sends the browser to the provider.
func (s *Server) loginOIDC(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.oidc == nil {
		http.Redirect(w, r, "/-/login", http.StatusSeeOther)
		return
	}
	state, verifier := newID(), newID()
	u, err := s.oidc.authURL(r.Context(), state, verifier, r.URL.Query().Get("reauth") == "1")
	if err != nil {
		s.log.Error("oidc sign-in failed", "err", err)
		s.renderLogin(w, r, http.StatusBadGateway, next, "The identity provider cannot be reached. Try again later.")
		return
	}
	s.sess.addPending(state, pendingLogin{verifier: verifier, next: next})
	s.setCookie(w, loginCookie, state, "/-/auth/", int(pendingTTL/time.Second))
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// callback completes an OIDC sign-in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie(loginCookie)
	s.setCookie(w, loginCookie, "", "/-/auth/", -1)
	if s.oidc == nil || err != nil || state == "" || c.Value != state {
		// The login was started in another browser, or long ago.
		s.renderLogin(w, r, http.StatusBadRequest, "/", "Sign-in failed. Try again.")
		return
	}
	p, ok := s.sess.takePending(state)
	if !ok {
		s.renderLogin(w, r, http.StatusBadRequest, "/", "Sign-in took too long. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.log.Info("oidc sign-in refused", "error", e, "description", q.Get("error_description"))
		s.renderLogin(w, r, http.StatusUnauthorized, p.next, "The identity provider refused the sign-in.")
		return
	}
	t, err := s.oidc.exchange(r.Context(), q.Get("code"), p.verifier)
	if err != nil {
		s.log.Error("oidc code exchange failed", "err", err)
		s.renderLogin(w, r, http.StatusBadGateway, p.next, "Sign-in failed. Try again.")
		return
	}
	s.signIn(w, r, t.AccessToken, t.RefreshToken, t.expiry(time.Now()), p.next)
}

// loginToken signs in with a pasted F1 token.
func (s *Server) loginToken(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	token := strings.TrimSpace(r.FormValue("token"))
	if !s.cfg.TokenLogin {
		http.Error(w, "token sign-in is disabled", http.StatusForbidden)
		return
	}
	if token == "" {
		s.renderLogin(w, r, http.StatusBadRequest, next, "Enter a token.")
		return
	}
	s.signIn(w, r, token, "", time.Time{}, next)
}

// signIn asks F1 who the token belongs to, which also verifies it, and
// starts a session.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, token, refresh string, expiry time.Time, next string) {
	api := &f1client.Client{BaseURL: s.cfg.F1URL, Token: token, HTTP: s.f1}
	me, err := api.Me(r.Context())
	if err != nil {
		if code := f1client.Code(err); code == f1client.CodeUnauthenticated || code == f1client.CodeForbidden {
			s.log.Info("sign-in refused by F1", "err", err)
			s.renderLogin(w, r, http.StatusUnauthorized, next, "F1 did not accept the credentials.")
			return
		}
		s.log.Error("sign-in: F1 unreachable", "err", err)
		s.renderLogin(w, r, http.StatusBadGateway, next, "The F1 server cannot be reached. Try again later.")
		return
	}
	// A new session ID at each sign-in prevents session fixation.
	if old := s.session(r); old != nil {
		s.sess.delete(old.id)
	}
	sess := s.sess.create(me.Principal, token, refresh, expiry)
	s.setCookie(w, s.sessionCookieName(), sess.id, "/", 0)
	s.log.Info("signed in", "principal", me.Principal)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := s.session(r); sess != nil {
		s.sess.delete(sess.id)
	}
	s.setCookie(w, s.sessionCookieName(), "", "/", -1)
	http.Redirect(w, r, "/-/login", http.StatusSeeOther)
}

// home lists the namespaces the user can see.
func (s *Server) home(w http.ResponseWriter, r *http.Request, rq *request) error {
	nss, err := rq.api.Namespaces(r.Context())
	if err != nil {
		return err
	}
	s.render(w, r, http.StatusOK, "home", pageData{User: rq.sess.principal, Title: "Namespaces", Data: nss})
	return nil
}
