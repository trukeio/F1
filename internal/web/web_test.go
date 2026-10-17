package web

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	c.OIDC = &OIDCConfig{Issuer: "https://login.example/realms/acme", ClientID: "f1web"}
	if err := c.Validate(); err != nil || c.OIDC.Name == "" || len(c.OIDC.Scopes) != 1 {
		t.Errorf("OIDC defaults: %+v, %v", c.OIDC, err)
	}
	for name, change := range map[string]func(*Config){
		"no public URL":   func(c *Config) { c.PublicURL = "" },
		"bad F1 URL":      func(c *Config) { c.F1URL = "f1.example:8080" },
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
