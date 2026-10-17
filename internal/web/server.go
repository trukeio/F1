package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

//go:embed templates static
var assets embed.FS

// refreshMargin renews an access token this long before it expires.
const refreshMargin = 30 * time.Second

// Server is the f1web HTTP handler.
type Server struct {
	cfg     Config
	log     *slog.Logger
	sess    *sessions
	oidc    *oidcClient // nil: no OIDC sign-in
	f1      *http.Client
	pages   map[string]*template.Template
	handler http.Handler
}

// New returns a server for a validated configuration.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	idle, err := cfg.idle()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, sess: newSessions(idle), f1: &http.Client{}}
	if cfg.OIDC != nil {
		s.oidc = &oidcClient{cfg: *cfg.OIDC, redirect: cfg.PublicURL + "/-/auth/callback",
			http: &http.Client{Timeout: 30 * time.Second}}
	}
	if s.pages, err = parsePages(); err != nil {
		return nil, err
	}
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /-/static/", http.StripPrefix("/-/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /-/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /-/login", s.loginPage)
	mux.HandleFunc("GET /-/login/oidc", s.loginOIDC)
	mux.HandleFunc("GET /-/auth/callback", s.callback)
	mux.HandleFunc("POST /-/login/token", s.loginToken)
	mux.HandleFunc("POST /-/logout", s.logout)
	mux.HandleFunc("GET /{$}", s.page(s.home))
	s.routes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, http.StatusNotFound, "error", pageData{Title: "Not found", Message: "This page does not exist."})
	})
	// POSTs from other sites are refused, so a form on another site
	// cannot sign a user in or out.
	s.handler = headers(http.NewCrossOriginProtection().Handler(mux))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// headers sets security headers on every response.
func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// Run serves until ctx is done. ready, if not nil, is called with the
// listener address once it accepts connections.
func Run(ctx context.Context, cfg Config, log *slog.Logger, ready func(net.Addr)) error {
	s, err := New(cfg, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				srv.Shutdown(ctx)
				return
			case <-t.C:
				s.sess.sweep()
			}
		}
	}()
	log.Info("f1web listening", "addr", ln.Addr().String(), "f1", cfg.F1URL)
	if ready != nil {
		ready(ln.Addr())
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// request is what a page handler gets for a signed-in user.
type request struct {
	sess *session
	api  *f1client.Client
}

// page wraps a handler that needs a signed-in user. Without one, the
// browser is sent to sign in and then back.
func (s *Server) page(fn func(w http.ResponseWriter, r *http.Request, rq *request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			s.toLogin(w, r)
			return
		}
		api, err := s.client(r.Context(), sess)
		if err == nil {
			err = fn(w, r, &request{sess: sess, api: api})
		}
		if err != nil {
			s.fail(w, r, sess, err)
		}
	}
}

func (s *Server) toLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/-/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
}

// errSignedOut means the session's token can no longer be used.
var errSignedOut = errors.New("signed out")

// fail shows an error page for err.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, sess *session, err error) {
	switch code := f1client.Code(err); {
	case errors.Is(err, errSignedOut), code == f1client.CodeUnauthenticated:
		s.sess.delete(sess.id)
		s.toLogin(w, r)
	case errors.Is(err, errNotFound), code == f1client.CodeNotFound:
		s.render(w, r, http.StatusNotFound, "error", pageData{User: sess.principal, Title: "Not found",
			Message: "This page does not exist, or you have no access to it."})
	case code == f1client.CodeForbidden:
		s.render(w, r, http.StatusForbidden, "error", pageData{User: sess.principal, Title: "No access",
			Message: "You have no access to this page."})
	default:
		s.log.Error("page failed", "path", r.URL.Path, "principal", sess.principal, "err", err)
		s.render(w, r, http.StatusBadGateway, "error", pageData{User: sess.principal, Title: "Error",
			Message: "The F1 server could not answer. Try again later."})
	}
}

// client returns an API client acting as the session's user, refreshing
// its access token first if it is about to expire.
func (s *Server) client(ctx context.Context, sess *session) (*f1client.Client, error) {
	token, refresh, expiry := sess.tokens()
	if !expiry.IsZero() && time.Until(expiry) < refreshMargin {
		if refresh == "" || s.oidc == nil {
			return nil, errSignedOut
		}
		var err error
		if token, err = s.refresh(ctx, sess); err != nil {
			return nil, err
		}
	}
	return &f1client.Client{BaseURL: s.cfg.F1URL, Token: token, HTTP: s.f1}, nil
}

// refresh renews the session's access token. Concurrent requests wait for
// one refresh.
func (s *Server) refresh(ctx context.Context, sess *session) (string, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.expiry.IsZero() || time.Until(sess.expiry) >= refreshMargin {
		return sess.token, nil // another request refreshed it
	}
	t, err := s.oidc.refresh(ctx, sess.refresh)
	if err != nil {
		s.log.Info("token refresh failed", "principal", sess.principal, "err", err)
		return "", errSignedOut
	}
	sess.token, sess.expiry = t.AccessToken, t.expiry(time.Now())
	if t.RefreshToken != "" {
		sess.refresh = t.RefreshToken
	}
	return sess.token, nil
}

// pageData is what every template gets.
type pageData struct {
	User    string // principal; "" when not signed in
	Title   string
	Message string
	Data    any // page-specific
}

// UserName is the principal without its "user:" prefix.
func (d pageData) UserName() string { return strings.TrimPrefix(d.User, "user:") }

// parsePages parses each page template together with the layout and the
// shared parts.
func parsePages() (map[string]*template.Template, error) {
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" || name == "parts" {
			continue
		}
		t, err := template.New(name).Funcs(templateFuncs()).ParseFS(assets, "templates/layout.html", "templates/parts.html", n)
		if err != nil {
			return nil, err
		}
		pages[name] = t
	}
	return pages, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, d pageData) {
	t := s.pages[name]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", d); err != nil {
		s.log.Error("template failed", "page", name, "path", r.URL.Path, "err", err)
	}
}
