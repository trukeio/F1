package test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/oidctest"
	"github.com/trukeio/f1/internal/web"
)

// startWeb runs f1web against e and returns its URL.
func startWeb(t *testing.T, e *env, oidc *web.OIDCConfig) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	cfg := web.DefaultConfig()
	cfg.PublicURL = "http://" + ts.Listener.Addr().String()
	cfg.F1URL = e.url
	cfg.OIDC = oidc
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := web.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL
}

// browser is an HTTP client with cookies that follows redirects.
func browser(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

// fetch sends a request and returns the final status, path and body.
func fetch(t *testing.T, c *http.Client, method, u string, form url.Values, hdr http.Header) (int, string, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL.RequestURI(), string(b)
}

func TestWebTokenSignIn(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.createNS(t, "secret", "")
	e.setACL(t, "proj", "user:alice", "", "read")
	w := startWeb(t, e, nil)
	b := browser(t)

	// Signed out, a page sends the browser to sign in and back.
	status, path, body := fetch(t, b, "GET", w+"/", nil, nil)
	if status != 200 || path != "/login?next=%2F" || !strings.Contains(body, "Sign in with a token") {
		t.Fatalf("signed out: %d %s", status, path)
	}
	if strings.Contains(body, "single sign-on") {
		t.Error("OIDC offered without configuration")
	}

	status, _, body = fetch(t, b, "POST", w+"/login/token", url.Values{"token": {"f1_wrong"}, "next": {"/"}}, nil)
	if status != 401 || !strings.Contains(body, "did not accept") {
		t.Fatalf("wrong token: %d", status)
	}

	// next must stay on this site.
	status, path, body = fetch(t, b, "POST", w+"/login/token", url.Values{"token": {e.tokens["alice"]}, "next": {"//evil.example/"}}, nil)
	if status != 200 || path != "/" {
		t.Fatalf("sign-in: %d %s", status, path)
	}
	if !strings.Contains(body, ">alice<") || !strings.Contains(body, `href="/proj"`) {
		t.Errorf("home page lacks the user or proj:\n%s", body)
	}
	if strings.Contains(body, "secret") {
		t.Error("home page shows a namespace alice cannot read")
	}
	for _, c := range b.Jar.Cookies(mustURL(t, w)) {
		if strings.Contains(c.Value, e.tokens["alice"]) {
			t.Error("the token reached the browser")
		}
	}

	// A sign-out from another site is refused.
	if status, _, _ = fetch(t, b, "POST", w+"/logout", url.Values{}, http.Header{"Sec-Fetch-Site": {"cross-site"}}); status != 403 {
		t.Errorf("cross-site sign-out: %d", status)
	}
	if status, path, _ = fetch(t, b, "POST", w+"/logout", url.Values{}, nil); status != 200 || path != "/login" {
		t.Fatalf("sign-out: %d %s", status, path)
	}
	if _, path, _ = fetch(t, b, "GET", w+"/", nil, nil); !strings.HasPrefix(path, "/login") {
		t.Errorf("still signed in after sign-out: %s", path)
	}
}

func TestWebOIDCSignIn(t *testing.T) {
	p := oidctest.New(t)
	// Tokens live 10 s, less than the refresh margin, so every page
	// refreshes them.
	p.SetClient(oidctest.Client{ID: "f1web", Secret: "s3cret", Subject: "carol", Groups: []string{"designers"},
		Audience: "f1", TTL: 10 * time.Second})
	e := start(t, envOpts{oidc: []auth.OIDCConfig{{Name: "corp", Issuer: p.Issuer(), Audiences: []string{"f1"}}}})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "group:designers", "", "read")
	w := startWeb(t, e, &web.OIDCConfig{Name: "Corp", Issuer: p.Issuer(), ClientID: "f1web", ClientSecret: "s3cret"})
	b := browser(t)

	_, _, body := fetch(t, b, "GET", w+"/login", nil, nil)
	if !strings.Contains(body, "Sign in with Corp") {
		t.Fatal("no OIDC sign-in offered")
	}
	status, path, body := fetch(t, b, "GET", w+"/login/oidc?next=%2F", nil, nil)
	if status != 200 || path != "/" || !strings.Contains(body, ">carol<") || !strings.Contains(body, `href="/proj"`) {
		t.Fatalf("OIDC sign-in: %d %s\n%s", status, path, body)
	}
	if n := p.Issued(); n != 2 {
		t.Errorf("%d tokens issued, want 2 (sign-in, refresh)", n)
	}
	if _, path, _ = fetch(t, b, "GET", w+"/", nil, nil); path != "/" || p.Issued() != 3 {
		t.Errorf("second page: %s, %d tokens issued", path, p.Issued())
	}

	// When the provider stops refreshing, the user signs in again.
	p.RevokeRefreshTokens()
	if _, path, _ = fetch(t, b, "GET", w+"/", nil, nil); !strings.HasPrefix(path, "/login") {
		t.Errorf("revoked refresh token: %s", path)
	}

	// A callback whose state this browser did not start is refused.
	if status, _, _ = fetch(t, b, "GET", w+"/auth/callback?code=code-1&state=forged", nil, nil); status != 400 {
		t.Errorf("forged callback: %d", status)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
