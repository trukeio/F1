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

// startWeb runs f1web against e and returns its URL. mod, if not nil,
// changes the configuration.
func startWeb(t *testing.T, e *env, oidc *web.OIDCConfig, mod ...func(*web.Config)) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	cfg := web.DefaultConfig()
	cfg.PublicURL = "http://" + ts.Listener.Addr().String()
	cfg.F1URL = e.url
	cfg.OIDC = oidc
	for _, m := range mod {
		m(&cfg)
	}
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
	if status != 200 || path != "/-/login?next=%2F" || !strings.Contains(body, "Sign in with a token") {
		t.Fatalf("signed out: %d %s", status, path)
	}
	if strings.Contains(body, "single sign-on") {
		t.Error("OIDC offered without configuration")
	}

	status, _, body = fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {"f1_wrong"}, "next": {"/"}}, nil)
	if status != 401 || !strings.Contains(body, "did not accept") {
		t.Fatalf("wrong token: %d", status)
	}

	// next must stay on this site.
	status, path, body = fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {e.tokens["alice"]}, "next": {"//evil.example/"}}, nil)
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
	if status, _, _ = fetch(t, b, "POST", w+"/-/logout", url.Values{}, http.Header{"Sec-Fetch-Site": {"cross-site"}}); status != 403 {
		t.Errorf("cross-site sign-out: %d", status)
	}
	if status, path, _ = fetch(t, b, "POST", w+"/-/logout", url.Values{}, nil); status != 200 || path != "/-/login" {
		t.Fatalf("sign-out: %d %s", status, path)
	}
	if _, path, _ = fetch(t, b, "GET", w+"/", nil, nil); !strings.HasPrefix(path, "/-/login") {
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

	_, _, body := fetch(t, b, "GET", w+"/-/login", nil, nil)
	if !strings.Contains(body, "Sign in with Corp") {
		t.Fatal("no OIDC sign-in offered")
	}
	status, path, body := fetch(t, b, "GET", w+"/-/login/oidc?next=%2F", nil, nil)
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
	if _, path, _ = fetch(t, b, "GET", w+"/", nil, nil); !strings.HasPrefix(path, "/-/login") {
		t.Errorf("revoked refresh token: %s", path)
	}

	// A callback whose state this browser did not start is refused.
	if status, _, _ = fetch(t, b, "GET", w+"/-/auth/callback?code=code-1&state=forged", nil, nil); status != 400 {
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

// signIn signs b in to f1web at w with an F1 token.
func signIn(t *testing.T, b *http.Client, w, token string) {
	t.Helper()
	if status, path, _ := fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {token}, "next": {"/"}}, nil); status != 200 || path != "/" {
		t.Fatalf("sign-in: %d %s", status, path)
	}
}

// noRedirect is a browser that reports redirects instead of following
// them.
func noRedirect(b *http.Client) *http.Client {
	c := *b
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// TestWebPages walks the read-only pages of ADR-0018, item 8.
func TestWebPages(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "svn")
	e.createNS(t, "secret", "")
	e.setACL(t, "proj", "user:alice", "", "read,write,lock")
	e.setACL(t, "proj", "user:bob", "docs/", "read")
	w := startWeb(t, e, nil, func(c *web.Config) { c.MaxRender = 1024 })

	readme := "# Project\n\nSee the [guide](docs/guide.md).\n\n![logo](logo.png)\n"
	main1 := "package main\n\nfunc main() {\n\tprintln(\"old line\")\n}\n"
	main2 := "package main\n\nfunc main() {\n\tprintln(\"new line\")\n}\n"
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	c := put(t, e.alice, "proj", "README.md", readme, "new").Commit
	c = put(t, e.alice, "proj", "docs/guide.md", "# Guide\n", c).Commit
	c = put(t, e.alice, "proj", "src/main.go", main1, c).Commit
	c = put(t, e.alice, "proj", "logo.png", png, c).Commit
	c = put(t, e.alice, "proj", "big.txt", strings.Repeat("x", 2000), c).Commit
	c = put(t, e.alice, "proj", "evil.html", "<script>alert(1)</script>", c).Commit
	c = put(t, e.alice, "proj", "x<b>.txt", "angle", c).Commit
	first := c
	c = put(t, e.alice, "proj", "src/main.go", main2, c).Commit
	if err := e.alice.UpdateRef(ctx, "proj", "heads/feature", "", first); err != nil {
		t.Fatal(err)
	}
	if err := e.alice.UpdateRef(ctx, "proj", "tags/v1", "", first); err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.Lock(ctx, "proj", "main", "docs/guide.md", 0); err != nil {
		t.Fatal(err)
	}

	b := browser(t)
	signIn(t, b, w, e.tokens["alice"])
	page := func(path string, want ...string) string {
		t.Helper()
		status, _, body := fetch(t, b, "GET", w+path, nil, nil)
		if status != 200 {
			t.Fatalf("%s: %d\n%s", path, status, body)
		}
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", path, s)
			}
		}
		return body
	}
	redirect := func(path, want string) {
		t.Helper()
		resp, err := noRedirect(b).Get(w + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != want {
			t.Errorf("%s: %d to %q, want %q", path, resp.StatusCode, loc, want)
		}
	}

	// The root: listing, last commit, README with rewritten links.
	body := page("/proj",
		`href="/proj/tree/main/docs"`, `href="/proj/tree/main/src"`,
		`<h1 id="project">Project</h1>`, `href="/proj/blob/main/docs/guide.md"`, `src="/proj/raw/main/logo.png"`,
		`href="/proj/commit/`+c+`"`, "x&lt;b&gt;.txt")
	if strings.Index(body, "/proj/tree/main/src") > strings.Index(body, "/proj/blob/main/README.md") {
		t.Error("directories are not listed first")
	}
	page("/proj/tree/main/docs", "guide.md", `<strong>docs</strong>`)
	page("/proj/tree/tags%2Fv1/src", "main.go", "tag:")
	redirect("/proj/tree/main/src/main.go", "/proj/blob/main/src/main.go")
	redirect("/proj/blob/main/docs", "/proj/tree/main/docs")

	// Files.
	page("/proj/blob/main/src/main.go", "5 lines", `println(&#34;new line&#34;)`, `href="/proj/blob/`+c+`/src/main.go"`, `id="L4"`)
	page("/proj/blob/main/big.txt", "too large to display")
	page("/proj/blob/main/logo.png", `<img src="/proj/raw/`+c+`/logo.png"`)
	page("/proj/blob/main/README.md", `<h1 id="project">`, `href="?plain=1"`)
	page("/proj/blob/main/README.md?plain=1", "# Project", "Preview")

	// Raw files are inert: HTML as text, under a sandbox.
	resp, err := b.Get(w + "/proj/raw/main/evil.html")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(raw) != "<script>alert(1)</script>" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("raw HTML: %d %q %v", resp.StatusCode, raw, resp.Header)
	}
	req, _ := http.NewRequest("GET", w+"/proj/raw/main/big.txt", nil)
	req.Header.Set("Range", "bytes=10-19")
	if resp, err = b.Do(req); err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(raw) != "xxxxxxxxxx" || resp.Header.Get("Content-Range") != "bytes 10-19/2000" {
		t.Errorf("raw range: %d %q", resp.StatusCode, raw)
	}
	if resp, err = b.Get(w + "/proj/raw/main/logo.png"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Content-Disposition") != "" {
		t.Errorf("raw image: %v", resp.Header)
	}

	// History, a commit, a comparison.
	page("/proj/commits/main", "put src/main.go", "put README.md", `href="/proj/commit/`+first+`"`)
	page("/proj/commits/main/src/main.go", "History of")
	redirect("/proj/commit/main", "/proj/commit/"+c)
	page("/proj/commit/"+c, `class="del"`, `println(&#34;old line&#34;)`, `class="add"`, `println(&#34;new line&#34;)`, "1 addition", "1 deletion")
	page("/proj/compare/feature...main", "src/main.go", `println(&#34;new line&#34;)`)
	redirect("/proj/compare?base=v1&head=main", "/proj/compare/v1...main")

	// Refs and locks.
	page("/proj/branches", `href="/proj/tree/feature"`, `href="/proj/compare/main...feature"`, "default")
	page("/proj/tags", `href="/proj/tree/tags%2Fv1"`)
	page("/proj/locks", "docs/guide.md", "user:alice")

	// Unknown and unreadable namespaces look the same.
	for _, p := range []string{"/nope", "/secret", "/secret/tree/main"} {
		if status, _, _ := fetch(t, b, "GET", w+p, nil, nil); status != 404 {
			t.Errorf("%s: %d", p, status)
		}
	}

	// bob reads only docs/: no whole history, no other files.
	bb := browser(t)
	signIn(t, bb, w, e.tokens["bob"])
	_, _, body = fetch(t, bb, "GET", w+"/proj/commits/main", nil, nil)
	if !strings.Contains(body, "only parts of this namespace") {
		t.Error("bob's history page lacks the partial-access note")
	}
	_, _, body = fetch(t, bb, "GET", w+"/proj", nil, nil)
	if !strings.Contains(body, "/proj/tree/main/docs") || strings.Contains(body, "README.md") {
		t.Error("bob's listing is not filtered")
	}
	if status, _, _ := fetch(t, bb, "GET", w+"/proj/blob/main/src/main.go", nil, nil); status == 200 {
		t.Error("bob reads src/main.go")
	}
	page2 := func(path string) int { status, _, _ := fetch(t, bb, "GET", w+path, nil, nil); return status }
	if s := page2("/proj/commits/main/docs/guide.md"); s != 200 {
		t.Errorf("bob's file history: %d", s)
	}
}
