package test

import (
	"archive/zip"
	"bytes"
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
	"github.com/trukeio/f1/pkg/f1client"
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
	// Older browsers send only Origin.
	if status, _, _ = fetch(t, b, "POST", w+"/-/logout", url.Values{}, http.Header{"Origin": {"https://evil.example"}}); status != 403 {
		t.Errorf("sign-out from another origin: %d", status)
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

	// Signing in again for administration makes the provider ask again
	// (ADR-0020, item 12).
	if _, _, body = fetch(t, b, "GET", w+"/-/login?reauth=1&next=%2Fproj%2Fsettings%2Faccess", nil, nil); !strings.Contains(body, "next=%2fproj%2fsettings%2faccess&amp;reauth=1") {
		t.Errorf("re-authentication link:\n%s", body)
	}
	resp, err := noRedirect(b).Get(w + "/-/login/oidc?next=%2F&reauth=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if q := mustURL(t, resp.Header.Get("Location")).Query(); q.Get("max_age") != "0" || q.Get("prompt") != "login" {
		t.Errorf("re-authentication request: %s", resp.Header.Get("Location"))
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
	e.createNS(t, "code", "git")
	e.setACL(t, "code", "user:alice", "", "read,write")
	w := startWeb(t, e, nil, func(c *web.Config) { c.MaxRender = 1024; c.S3URL = "https://s3.example" })

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
	// W3: each entry's last commit, the shortcuts' data, highlighting.
	if !strings.Contains(body, `href="/proj/commit/`+first+`" title="put x&lt;b&gt;.txt"`) {
		t.Error("the root listing lacks the last commit of an entry")
	}
	if !strings.Contains(body, `data-finder="/proj/find/main"`) || !strings.Contains(body, `data-permalink="/proj/tree/`+c+`"`) {
		t.Error("the root page lacks the shortcuts' data")
	}
	page("/proj/blob/main/src/main.go", `<span class="kn">package</span>`, `data-permalink="/proj/blob/`+c+`/src/main.go"`)
	page("/proj/find/main", `<script id="finder-data" type="application/json" data-base="/proj/blob/main">`,
		`"src/main.go"`, `"x\u003cb\u003e.txt"`)
	page("/-/highlight.css", ".hl .k {", "@media (prefers-color-scheme: dark)")
	page("/proj/tree/main/docs", "guide.md", `<strong>docs</strong>`)
	page("/proj/tree/tags%2Fv1/src", "main.go", "tag:")
	redirect("/proj/tree/main/src/main.go", "/proj/blob/main/src/main.go")
	redirect("/proj/blob/main/docs", "/proj/tree/main/docs")

	// Files.
	page("/proj/blob/main/src/main.go", "5 lines", `<span class="s">&#34;new line&#34;</span>`, `href="/proj/blob/`+c+`/src/main.go"`, `id="L4"`)
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
	page("/proj/commit/"+c, `class="del"`, `<span class="s">&#34;old line&#34;</span>`, `class="add"`, `<span class="s">&#34;new line&#34;</span>`, "1 addition", "1 deletion")
	page("/proj/compare/feature...main", "src/main.go", `<span class="s">&#34;new line&#34;</span>`)
	redirect("/proj/compare?base=v1&head=main", "/proj/compare/v1...main")

	// Refs and locks.
	page("/proj/branches", `href="/proj/tree/feature"`, `href="/proj/compare/main...feature"`, "default")
	page("/proj/tags", `href="/proj/tree/tags%2Fv1"`)
	page("/proj/locks", "docs/guide.md", "user:alice")

	// The Code menu: clone commands for the namespace's protocols, and a
	// ZIP of the files at the ref shown.
	page("/proj", `value="f1 clone `+e.url+` proj"`, `value="aws --endpoint-url https://s3.example s3 sync s3://proj proj"`,
		`href="/proj/archive/main.zip"`)
	if strings.Contains(body, "git clone") {
		t.Error("the Code menu of a namespace without Git offers git clone")
	}
	page("/proj/tree/feature", `value="f1 clone --ref feature `+e.url+` proj"`, `href="/proj/archive/feature.zip"`)
	put(t, e.alice, "code", "a.txt", "a", "new")
	page("/code", `value="git clone `+e.url+`/git/code.git"`)
	if body = page("/proj/tree/main/src"); strings.Contains(body, "Download ZIP") {
		t.Error("a subdirectory has a Code menu")
	}
	zipFiles := func(b *http.Client, path, wantName string) map[string]string {
		t.Helper()
		resp, err := b.Get(w + path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" ||
			resp.Header.Get("Content-Disposition") != "attachment; filename="+wantName+".zip" {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, resp.Header)
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		files := map[string]string{}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			files[f.Name] = string(b)
		}
		return files
	}
	files := zipFiles(b, "/proj/archive/main.zip", "proj-main")
	if len(files) != 7 || files["proj-main/src/main.go"] != main2 || files["proj-main/README.md"] != readme ||
		files["proj-main/logo.png"] != png {
		t.Errorf("ZIP of main: %d files: %v", len(files), files["proj-main/src/main.go"])
	}
	if files = zipFiles(b, "/proj/archive/tags%2Fv1.zip", "proj-tags-v1"); files["proj-tags-v1/src/main.go"] != main1 {
		t.Errorf("ZIP of tags/v1: %v", files)
	}
	if status, _, _ := fetch(t, b, "GET", w+"/proj/archive/main", nil, nil); status != 404 {
		t.Errorf("archive without .zip: %d", status)
	}

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
	if files := zipFiles(bb, "/proj/archive/main.zip", "proj-main"); len(files) != 1 || files["proj-main/docs/guide.md"] != "# Guide\n" {
		t.Errorf("bob's ZIP: %v", files)
	}
	page2 := func(path string) int { status, _, _ := fetch(t, bb, "GET", w+path, nil, nil); return status }
	if s := page2("/proj/commits/main/docs/guide.md"); s != 200 {
		t.Errorf("bob's file history: %d", s)
	}
}

// Users manage their own tokens and S3 keys in f1web (ADR-0020, A2).
func TestWebSettings(t *testing.T) {
	e := start(t, envOpts{})
	w := startWeb(t, e, nil)
	b := browser(t)
	if status, _, _ := fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {e.tokens["alice"]}, "next": {"/"}}, nil); status != 200 {
		t.Fatalf("sign-in: %d", status)
	}

	status, _, body := fetch(t, b, "GET", w+"/-/settings", nil, nil)
	if status != 200 || !strings.Contains(body, "user:alice") || !strings.Contains(body, "group:eng") {
		t.Fatalf("profile: %d\n%s", status, body)
	}

	// A new token is shown once, and works.
	status, path, body := fetch(t, b, "POST", w+"/-/settings/tokens", url.Values{"description": {"laptop"}, "days": {"30"}}, nil)
	i := strings.Index(body, `value="f1_`)
	if status != 200 || path != "/-/settings/tokens" || i < 0 {
		t.Fatalf("create token: %d %s\n%s", status, path, body)
	}
	secret := body[i+len(`value="`):]
	secret = secret[:strings.IndexByte(secret, '"')]
	if _, err := f1client.New(e.url, secret).Me(ctx); err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, _, body = fetch(t, b, "GET", w+"/-/settings/tokens", nil, nil); strings.Contains(body, secret) || !strings.Contains(body, "laptop") {
		t.Fatalf("token page after creation:\n%s", body)
	}
	toks, _ := e.alice.Tokens(ctx)
	var id string
	for _, x := range toks {
		if x.Description == "laptop" {
			id = x.ID
		}
	}

	// F1's refusal is shown on the form.
	if status, _, body = fetch(t, b, "POST", w+"/-/settings/tokens", url.Values{"days": {"365"}}, nil); status != 400 || !strings.Contains(body, "F1 refused this") {
		t.Errorf("token beyond the maximum: %d\n%s", status, body)
	}
	big := url.Values{"days": {"30"}, "description": {strings.Repeat("x", 70<<10)}}
	if status, _, _ = fetch(t, b, "POST", w+"/-/settings/tokens", big, nil); status != 400 {
		t.Errorf("oversized form: %d", status)
	}
	// Forms from other sites are refused.
	if status, _, _ = fetch(t, b, "POST", w+"/-/settings/tokens", url.Values{"days": {"30"}}, http.Header{"Sec-Fetch-Site": {"cross-site"}}); status != 403 {
		t.Errorf("cross-site token creation: %d", status)
	}

	// Revoking asks first.
	status, _, body = fetch(t, b, "GET", w+"/-/settings/tokens/"+id+"/revoke", nil, nil)
	if status != 200 || !strings.Contains(body, `action="/-/settings/tokens/`+id+`/revoke"`) {
		t.Fatalf("confirm revoke: %d\n%s", status, body)
	}
	if status, path, _ = fetch(t, b, "POST", w+"/-/settings/tokens/"+id+"/revoke", url.Values{}, nil); status != 200 || path != "/-/settings/tokens" {
		t.Fatalf("revoke: %d %s", status, path)
	}
	if _, err := f1client.New(e.url, secret).Me(ctx); f1client.Code(err) != f1client.CodeUnauthenticated {
		t.Errorf("revoked token: %v", err)
	}

	// S3 keys: access key and secret shown once; revoked.
	status, _, body = fetch(t, b, "POST", w+"/-/settings/s3keys", url.Values{}, nil)
	keys, _ := e.alice.S3Keys(ctx)
	if status != 200 || len(keys) != 2 || !strings.Contains(body, "Secret key") {
		t.Fatalf("create S3 key: %d, %d keys\n%s", status, len(keys), body)
	}
	var newKey string
	for _, k := range keys {
		if k.AccessKey != e.s3keys["alice"].AccessKey {
			newKey = k.AccessKey
		}
	}
	if !strings.Contains(body, newKey) {
		t.Errorf("the new access key is not shown")
	}
	if status, _, _ = fetch(t, b, "POST", w+"/-/settings/s3keys/"+newKey+"/revoke", url.Values{}, nil); status != 200 {
		t.Fatalf("revoke S3 key: %d", status)
	}
	if keys, _ = e.alice.S3Keys(ctx); len(keys) != 1 {
		t.Errorf("S3 keys after revoking: %+v", keys)
	}
}

// Namespace administrators manage access in f1web (ADR-0020, A3).
func TestWebAccess(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "user:alice", "", "admin")
	e.setACL(t, "proj", "user:bob", "", "read")
	w := startWeb(t, e, nil)
	signIn := func(user string) *http.Client {
		b := browser(t)
		if status, _, _ := fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {e.tokens[user]}, "next": {"/"}}, nil); status != 200 {
			t.Fatalf("sign-in %s: %d", user, status)
		}
		return b
	}
	alice, bob := signIn("alice"), signIn("bob")

	// Only administrators get the Settings tab and page.
	if _, _, body := fetch(t, alice, "GET", w+"/proj", nil, nil); !strings.Contains(body, `href="/proj/settings/access"`) {
		t.Error("alice has no Settings tab")
	}
	if _, _, body := fetch(t, bob, "GET", w+"/proj", nil, nil); strings.Contains(body, "/settings/access") {
		t.Error("bob has a Settings tab")
	}
	if status, _, _ := fetch(t, bob, "GET", w+"/proj/settings/access", nil, nil); status != 403 {
		t.Errorf("bob's access page: %d", status)
	}

	// Add, deny, list.
	status, path, _ := fetch(t, alice, "POST", w+"/proj/settings/access", url.Values{"principal": {"group:eng"}, "prefix": {"docs/"}, "perm": {"write", "read"}}, nil)
	if status != 200 || path != "/proj/settings/access" {
		t.Fatalf("add entry: %d %s", status, path)
	}
	if _, _, body := fetch(t, alice, "POST", w+"/proj/settings/access", url.Values{"principal": {"user:bob"}, "prefix": {"docs/hr/"}}, nil); !strings.Contains(body, "denied") {
		t.Errorf("deny entry not shown:\n%s", body)
	}
	acl, _ := e.admin.ACL(ctx, "proj")
	var got []string
	for _, x := range acl {
		got = append(got, x.Principal+" "+x.Prefix+" "+strings.Join(x.Perms, ","))
	}
	if want := "user:alice  admin|user:bob  read|group:eng docs/ read,write|user:bob docs/hr/ "; strings.Join(got, "|") != want {
		t.Errorf("ACL = %q, want %q", strings.Join(got, "|"), want)
	}
	status, _, body := fetch(t, alice, "POST", w+"/proj/settings/access", url.Values{"principal": {"bob"}, "perm": {"read"}}, nil)
	if status != 400 || !strings.Contains(body, "F1 refused this") || !strings.Contains(body, `value="bob"`) {
		t.Errorf("invalid principal: %d\n%s", status, body)
	}

	// Edit fills in the form; check access applies the rules.
	if _, _, body = fetch(t, alice, "GET", w+"/proj/settings/access?principal=group:eng&prefix=docs/", nil, nil); !strings.Contains(body, `value="write" checked`) {
		t.Error("Edit does not fill in the permissions")
	}
	q := url.Values{"check": {"user:bob, group:eng"}, "path": {"docs/a.md"}}
	if _, _, body = fetch(t, alice, "GET", w+"/proj/settings/access?"+q.Encode(), nil, nil); !strings.Contains(body, `<span class="tag">write</span>`) || !strings.Contains(body, "<code>docs/</code>") {
		t.Errorf("check docs/a.md:\n%s", body)
	}
	q.Set("path", "docs/hr/pay.md")
	if _, _, body = fetch(t, alice, "GET", w+"/proj/settings/access?"+q.Encode(), nil, nil); strings.Contains(body, "Allowed:") || !strings.Contains(body, "prefix <code>docs/hr/</code>: <code>user:bob</code>") {
		t.Errorf("check docs/hr/pay.md:\n%s", body)
	}

	// Remove, after confirming; cross-site changes are refused.
	status, _, body = fetch(t, alice, "GET", w+"/proj/settings/access/remove?principal=group:eng&prefix=docs/", nil, nil)
	if status != 200 || !strings.Contains(body, `name="principal" value="group:eng"`) {
		t.Fatalf("confirm removal: %d\n%s", status, body)
	}
	form := url.Values{"principal": {"group:eng"}, "prefix": {"docs/"}}
	if status, _, _ = fetch(t, alice, "POST", w+"/proj/settings/access/remove", form, http.Header{"Sec-Fetch-Site": {"cross-site"}}); status != 403 {
		t.Errorf("cross-site removal: %d", status)
	}
	if status, path, _ = fetch(t, alice, "POST", w+"/proj/settings/access/remove", form, nil); status != 200 || path != "/proj/settings/access" {
		t.Fatalf("remove: %d %s", status, path)
	}
	if acl, _ = e.admin.ACL(ctx, "proj"); len(acl) != 3 {
		t.Errorf("ACL after removal: %+v", acl)
	}
	// Removing her own administration sends alice back to the namespace.
	if _, path, _ = fetch(t, alice, "POST", w+"/proj/settings/access/remove", url.Values{"principal": {"user:alice"}, "prefix": {""}}, nil); path != "/proj" {
		t.Errorf("after removing her own entry: %s", path)
	}
}

// Changing access needs a recent sign-in (ADR-0020, item 12).
func TestWebAccessRecentSignIn(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "user:alice", "", "admin")
	w := startWeb(t, e, nil, func(c *web.Config) { c.AdminMaxAge = "1ns" })
	b := browser(t)
	fetch(t, b, "POST", w+"/-/login/token", url.Values{"token": {e.tokens["alice"]}, "next": {"/"}}, nil)

	status, path, body := fetch(t, b, "GET", w+"/proj/settings/access", nil, nil)
	if status != 200 || path != "/-/login?reauth=1&next=%2Fproj%2Fsettings%2Faccess" || !strings.Contains(body, "Sign in again") {
		t.Fatalf("stale sign-in: %d %s", status, path)
	}
	status, path, _ = fetch(t, b, "POST", w+"/proj/settings/access", url.Values{"principal": {"user:bob"}, "perm": {"read"}}, nil)
	if !strings.HasPrefix(path, "/-/login?reauth=1&next=%2Fproj%2Fsettings%2Faccess") {
		t.Fatalf("stale sign-in, POST: %d %s", status, path)
	}
	if acl, _ := e.admin.ACL(ctx, "proj"); len(acl) != 1 {
		t.Errorf("a change went through with a stale sign-in: %+v", acl)
	}
}

// Server administrators manage users, credentials, the server's ACL and
// namespaces in f1web (ADR-0020, A4).
func TestWebAdmin(t *testing.T) {
	e := start(t, envOpts{})
	w := startWeb(t, e, nil)
	admin, alice := browser(t), browser(t)
	signIn(t, admin, w, e.tokens["admin"])
	signIn(t, alice, w, e.tokens["alice"])

	// Only server administrators get the link and the pages.
	if _, _, body := fetch(t, admin, "GET", w+"/", nil, nil); !strings.Contains(body, `href="/-/admin"`) {
		t.Error("no Administration link for the administrator")
	}
	if _, _, body := fetch(t, alice, "GET", w+"/", nil, nil); strings.Contains(body, `href="/-/admin"`) {
		t.Error("Administration link for alice")
	}
	for _, p := range []string{"/-/admin/users", "/-/admin/tokens", "/-/admin/s3keys", "/-/admin/acl", "/-/admin/namespaces", "/-/admin/users/bob"} {
		if status, _, _ := fetch(t, alice, "GET", w+p, nil, nil); status != 403 {
			t.Errorf("alice GET %s: %d", p, status)
		}
	}

	// Users.
	status, _, body := fetch(t, admin, "GET", w+"/-/admin/users", nil, nil)
	if status != 200 || !strings.Contains(body, "user:alice") || !strings.Contains(body, `server admin</span>`) {
		t.Fatalf("users: %d\n%s", status, body)
	}
	status, path, _ := fetch(t, admin, "POST", w+"/-/admin/users", url.Values{"name": {"dave"}, "groups": {"eng, ops"}}, nil)
	if status != 200 || path != "/-/admin/users/dave" {
		t.Fatalf("add dave: %d %s", status, path)
	}
	if status, _, body = fetch(t, admin, "POST", w+"/-/admin/users", url.Values{"name": {"dave"}}, nil); status != 400 || !strings.Contains(body, "the name is taken") {
		t.Errorf("add dave twice: %d", status)
	}
	if _, path, _ = fetch(t, admin, "POST", w+"/-/admin/users/dave/groups", url.Values{"groups": {"eng"}}, nil); path != "/-/admin/users/dave" {
		t.Errorf("set groups: %s", path)
	}
	if _, _, body = fetch(t, admin, "GET", w+"/-/admin/users/dave", nil, nil); !strings.Contains(body, `value="group:eng"`) {
		t.Errorf("dave's groups:\n%s", body)
	}

	// A token for dave, shown once; revoked from his page.
	_, _, body = fetch(t, admin, "POST", w+"/-/admin/users/dave/tokens", url.Values{"description": {"ci"}, "days": {"0"}}, nil)
	i := strings.Index(body, `value="f1_`)
	if i < 0 {
		t.Fatalf("no new token:\n%s", body)
	}
	secret := body[i+len(`value="`):]
	secret = secret[:strings.IndexByte(secret, '"')]
	dave := f1client.New(e.url, secret)
	if me, err := dave.Me(ctx); err != nil || me.Principal != "user:dave" {
		t.Fatalf("dave's token: %+v %v", me, err)
	}
	toks, _ := e.admin.AdminTokens(ctx, "dave")
	if len(toks) != 1 || toks[0].Expires != nil {
		t.Fatalf("dave's tokens: %+v", toks)
	}
	if _, _, body = fetch(t, admin, "GET", w+"/-/admin/tokens", nil, nil); !strings.Contains(body, toks[0].ID) {
		t.Error("the token is not on the tokens page")
	}
	_, _, body = fetch(t, admin, "GET", w+"/-/admin/tokens/"+toks[0].ID+"/revoke?back=/-/admin/users/dave", nil, nil)
	if !strings.Contains(body, `name="back" value="/-/admin/users/dave"`) {
		t.Fatalf("confirm revoke:\n%s", body)
	}
	if _, path, _ = fetch(t, admin, "POST", w+"/-/admin/tokens/"+toks[0].ID+"/revoke", url.Values{"back": {"/-/admin/users/dave"}}, nil); path != "/-/admin/users/dave" {
		t.Errorf("after revoking: %s", path)
	}
	if _, err := dave.Me(ctx); f1client.Code(err) != f1client.CodeUnauthenticated {
		t.Errorf("revoked token: %v", err)
	}
	// back must stay on the administration pages.
	_, _, body = fetch(t, admin, "GET", w+"/-/admin/s3keys/"+e.s3keys["bob"].AccessKey+"/revoke?back=//evil.example/", nil, nil)
	if !strings.Contains(body, `name="back" value="/-/admin/s3keys"`) {
		t.Errorf("foreign back kept:\n%s", body)
	}
	fetch(t, admin, "POST", w+"/-/admin/s3keys/"+e.s3keys["bob"].AccessKey+"/revoke", url.Values{}, nil)
	if keys, _ := e.admin.AdminS3Keys(ctx, "bob"); len(keys) != 0 {
		t.Errorf("bob's S3 keys: %+v", keys)
	}

	// Disable and enable; the administrator cannot disable themselves.
	if _, _, body = fetch(t, admin, "GET", w+"/-/admin/users/bob/disable", nil, nil); !strings.Contains(body, `action="/-/admin/users/bob/disable"`) {
		t.Fatalf("confirm disable:\n%s", body)
	}
	fetch(t, admin, "POST", w+"/-/admin/users/bob/disable", url.Values{}, nil)
	if _, err := e.bob.Me(ctx); f1client.Code(err) != f1client.CodeUnauthenticated {
		t.Errorf("disabled bob: %v", err)
	}
	fetch(t, admin, "POST", w+"/-/admin/users/bob/enable", url.Values{}, nil)
	if _, err := e.bob.Me(ctx); err != nil {
		t.Errorf("enabled bob: %v", err)
	}
	if status, _, body = fetch(t, admin, "POST", w+"/-/admin/users/admin/disable", url.Values{}, nil); status != 400 || !strings.Contains(body, "cannot disable yourself") {
		t.Errorf("disable yourself: %d", status)
	}

	// The server's ACL: the last administrator stays.
	if _, _, body = fetch(t, admin, "GET", w+"/-/admin/acl", nil, nil); !strings.Contains(body, "Server access") || !strings.Contains(body, "user:admin") {
		t.Fatalf("server access:\n%s", body)
	}
	status, _, body = fetch(t, admin, "POST", w+"/-/admin/acl/remove", url.Values{"principal": {"user:admin"}, "prefix": {""}}, nil)
	if status != 409 || !strings.Contains(body, "no enabled administrator") {
		t.Errorf("remove the last administrator: %d\n%s", status, body)
	}

	// Namespaces.
	if _, path, _ = fetch(t, admin, "POST", w+"/-/admin/namespaces", url.Values{"name": {"code"}, "preset": {"git"}}, nil); path != "/code" {
		t.Fatalf("create namespace: %s", path)
	}
	if status, _, body = fetch(t, admin, "POST", w+"/-/admin/namespaces", url.Values{"name": {"code"}}, nil); status != 400 || !strings.Contains(body, "the name is taken") {
		t.Errorf("create it twice: %d", status)
	}
	if ns, _ := e.admin.Namespaces(ctx); len(ns) != 1 || !ns[0].Config.Git {
		t.Errorf("namespaces: %+v", ns)
	}
}
