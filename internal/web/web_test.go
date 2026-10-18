package web

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/client/textmerge"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/proj/tree/main/docs":   "/proj/tree/main/docs",
		"/proj?x=1":              "/proj?x=1",
		"//evil.example/":        "/",
		"/\\evil.example":        "/",
		"/\t/evil.example":       "/",
		"/\n/evil.example":       "/",
		"/a\\b":                  "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"proj/relative/path.txt": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	good := func() Config {
		c := DefaultConfig()
		c.PublicURL, c.F1URL = "https://f1web.example/", "http://127.0.0.1:8080"
		return c
	}
	c := good()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://f1web.example" || !c.secure() {
		t.Errorf("public URL %q, secure %v", c.PublicURL, c.secure())
	}
	if c.CloneURL != c.F1URL || c.S3URL != "" {
		t.Errorf("clone URL %q, S3 URL %q", c.CloneURL, c.S3URL)
	}
	c.OIDC = &OIDCConfig{Issuer: "https://login.example/realms/acme", ClientID: "f1web"}
	if err := c.Validate(); err != nil || c.OIDC.Name == "" || len(c.OIDC.Scopes) != 1 {
		t.Errorf("OIDC defaults: %+v, %v", c.OIDC, err)
	}
	for name, change := range map[string]func(*Config){
		"no public URL":   func(c *Config) { c.PublicURL = "" },
		"bad F1 URL":      func(c *Config) { c.F1URL = "f1.example:8080" },
		"bad clone URL":   func(c *Config) { c.CloneURL = "git@f1.example" },
		"bad S3 URL":      func(c *Config) { c.S3URL = "s3.example" },
		"bad idle":        func(c *Config) { c.SessionIdle = "-1h" },
		"bad log format":  func(c *Config) { c.Log.Format = "xml" },
		"no client ID":    func(c *Config) { c.OIDC = &OIDCConfig{Issuer: "https://login.example"} },
		"no way to login": func(c *Config) { c.TokenLogin = false },
	} {
		c := good()
		change(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSessionsExpire(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := newSessions(time.Hour)
	st.now = func() time.Time { return now }
	s := st.create("user:alice", "tok", "", time.Time{})
	now = now.Add(59 * time.Minute)
	if st.get(s.id) == nil {
		t.Fatal("session expired early")
	}
	now = now.Add(59 * time.Minute) // used 59 minutes ago: still alive
	if st.get(s.id) == nil {
		t.Fatal("use did not extend the session")
	}
	now = now.Add(61 * time.Minute)
	if st.get(s.id) != nil {
		t.Fatal("idle session still valid")
	}
	if st.get("unknown") != nil {
		t.Fatal("unknown session found")
	}

	st.addPending("state1", pendingLogin{verifier: "v"})
	if p, ok := st.takePending("state1"); !ok || p.verifier != "v" {
		t.Fatal("pending login lost")
	}
	if _, ok := st.takePending("state1"); ok {
		t.Fatal("a state was used twice")
	}
	st.addPending("state2", pendingLogin{})
	now = now.Add(pendingTTL + time.Second)
	if _, ok := st.takePending("state2"); ok {
		t.Fatal("stale pending login accepted")
	}
	st.addPending("state3", pendingLogin{})
	st.create("user:bob", "tok", "", time.Time{})
	now = now.Add(2 * time.Hour)
	st.sweep()
	if len(st.byID) != 0 || len(st.pending) != 0 {
		t.Errorf("sweep left %d sessions, %d pending logins", len(st.byID), len(st.pending))
	}
}

func TestPKCEChallenge(t *testing.T) {
	// RFC 7636, appendix B.
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge = %s", got)
	}
}

// TestImports enforces ADR-0018, item 2: f1web reaches F1 only through
// the native API.
func TestImports(t *testing.T) {
	allowed := map[string]bool{
		"github.com/trukeio/f1/pkg/f1client":              true,
		"github.com/trukeio/f1/internal/client/textmerge": true,
		"github.com/trukeio/f1/internal/web":              true,
	}
	files, _ := filepath.Glob("*.go")
	cmd, _ := filepath.Glob("../../cmd/f1web/*.go")
	for _, f := range append(files, cmd...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.HasPrefix(p, "github.com/trukeio/f1/") && !allowed[p] {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig("../../deploy/f1web.toml")
	if err != nil || c.OIDC == nil || c.OIDC.ClientID != "f1web" || !c.secure() {
		t.Fatalf("deploy/f1web.toml: %+v, %v", c, err)
	}
}

func TestParseUnified(t *testing.T) {
	ctx := context.Background()
	a := "one\ntwo\n-- three\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n"
	var buf bytes.Buffer
	if err := textmerge.Unified(ctx, textmerge.Bytes([]byte(a)), textmerge.Bytes([]byte(b)), "a", "b", &buf); err != nil {
		t.Fatal(err)
	}
	var fd fileDiff
	if err := parseUnified(&buf, &fd); err != nil {
		t.Fatal(err)
	}
	if fd.Adds != 1 || fd.Dels != 1 || len(fd.Hunks) != 2 {
		t.Fatalf("adds %d, dels %d, hunks %d:\n%s", fd.Adds, fd.Dels, len(fd.Hunks), buf.String())
	}
	// The deleted line starts with "--": it is a line, not a header.
	del := fd.Hunks[0].Lines[2]
	if del.Op != "del" || del.Text != "-- three" || del.Old != 3 || del.New != 0 {
		t.Errorf("deleted line = %+v", del)
	}
	add := fd.Hunks[1].Lines[len(fd.Hunks[1].Lines)-1]
	if add.Op != "add" || add.Text != "eleven" || add.New != 10 {
		t.Errorf("added line = %+v", add)
	}
	ctxLine := fd.Hunks[1].Lines[0]
	if ctxLine.Op != "ctx" || ctxLine.Old != 8 || ctxLine.New != 7 {
		t.Errorf("context line = %+v", ctxLine)
	}
}

func TestMarkdownLinks(t *testing.T) {
	src := "[guide](guide.md#setup) [up](../README.md) [root](/LICENSE) [out](../../x) [web](https://example.com/) [here](#top)\n\n" +
		"![logo](img/logo.png) <script>alert(1)</script> [js](javascript:alert(1))\n"
	html, err := renderMarkdown([]byte(src), "proj", "tags/v1", "docs/index.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`href="/proj/blob/tags%2Fv1/docs/guide.md#setup"`,
		`href="/proj/blob/tags%2Fv1/README.md"`,
		`href="/proj/blob/tags%2Fv1/LICENSE"`,
		`href="/proj/tree/tags%2Fv1"`,
		`href="https://example.com/"`,
		`href="#top"`,
		`src="/proj/raw/tags%2Fv1/docs/img/logo.png"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("missing %s in\n%s", want, html)
		}
	}
	if strings.Contains(string(html), "<script") || strings.Contains(string(html), "javascript:") {
		t.Errorf("unsafe output:\n%s", html)
	}
}

func TestRawType(t *testing.T) {
	for _, c := range []struct{ stored, name, want string }{
		{"image/png", "x", "image/png"},
		{"application/octet-stream", "a.png", "image/png"},
		{"", "a.svg", "image/svg+xml"},
		{"text/html; charset=utf-8", "a.bin", "text/html"},
		{"", "a.unknown-ext", "application/octet-stream"},
	} {
		if got := rawType(c.stored, c.name); got != c.want {
			t.Errorf("rawType(%q, %q) = %q, want %q", c.stored, c.name, got, c.want)
		}
	}
}

func TestHighlight(t *testing.T) {
	for _, src := range []string{"package main", "package main\n\n/* a\nb */\nvar x = 1 < 2\n", "a\n\n\n"} {
		text := strings.TrimSuffix(src, "\n")
		hl := highlight("main.go", text)
		if want := len(strings.Split(text, "\n")); len(hl) != want {
			t.Errorf("%q: %d lines, want %d", src, len(hl), want)
		}
	}
	hl := highlight("main.go", "/* a\nb */\nx := `<script>`")
	if !strings.Contains(string(hl[1]), `class="cm"`) {
		t.Errorf("the second line of a comment is not a comment: %s", hl[1])
	}
	if strings.Contains(string(hl[2]), "<script>") {
		t.Errorf("unescaped: %s", hl[2])
	}
	if highlight("notes.unknown-extension", "x") != nil {
		t.Error("highlighted a file no lexer knows")
	}
	if got := codeLines("a.unknown-extension", "<b>\nc"); len(got) != 2 || got[0] != "&lt;b&gt;" {
		t.Errorf("plain lines = %q", got)
	}
	css := highlightCSS()
	if !strings.Contains(css, ".hl .k {") || strings.Contains(css, "background") {
		t.Errorf("CSS:\n%s", css)
	}
}
