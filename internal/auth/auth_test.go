package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/trukeio/f1/internal/oidctest"
	"github.com/trukeio/f1/internal/serverdb"
)

var ctx = context.Background()

func newDB(t *testing.T) *serverdb.DB {
	t.Helper()
	db, err := serverdb.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func grants(t *testing.T, db *serverdb.DB, c *Caller, ns string) *Grants {
	t.Helper()
	g, err := (&Authorizer{DB: db}).For(ctx, c, ns)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func set(t *testing.T, db *serverdb.DB, ns, principal, prefix string, perms ...string) {
	t.Helper()
	if err := db.SetACL(ctx, serverdb.ACLEntry{Namespace: ns, Principal: principal, Prefix: prefix, Perms: perms}); err != nil {
		t.Fatal(err)
	}
}

func TestACL(t *testing.T) {
	db := newDB(t)
	alice := &Caller{Principal: "user:alice", Groups: []string{"group:eng"}}
	set(t, db, "ns", "group:eng", "", "read")
	set(t, db, "ns", "user:alice", "src/", "read", "write")
	set(t, db, "ns", "group:eng", "secret/")                 // deny
	set(t, db, "ns", "user:alice", "secret/public/", "read") // re-allow below a deny
	set(t, db, "ns", "group:eng", "src/vendor/", "read")     // longer prefix narrows write away
	set(t, db, "other", "user:alice", "", "admin")
	g := grants(t, db, alice, "ns")
	tests := []struct {
		path, perm string
		want       bool
	}{
		{"README", "read", true},
		{"README", "write", false},
		{"src/main.go", "write", true},
		{"src/vendor/x.go", "read", true},
		{"src/vendor/x.go", "write", false}, // longest prefix wins
		{"secret/key", "read", false},
		{"secret/public/doc", "read", true},
		{"secrets", "read", true}, // plain string prefix: "secret/" does not match "secrets"
		{"src/main.go", "lock", false},
	}
	for _, tt := range tests {
		if got := g.Allow(tt.path, tt.perm); got != tt.want {
			t.Errorf("Allow(%s, %s) = %v, want %v", tt.path, tt.perm, got, tt.want)
		}
	}
	if g.Whole("read") {
		t.Error("Whole(read) with a denied prefix")
	}
	if !g.Any("write") || g.Any("admin") {
		t.Error("Any is wrong")
	}
	if !g.Under("secret/", "read") || g.Under("secret/x", "read") {
		t.Error("Under is wrong")
	}
	if o := grants(t, db, alice, "other"); !o.Whole("read") || !o.Allow("x", "lock") {
		t.Error("admin does not imply every permission")
	}
	bob := &Caller{Principal: "user:bob"}
	if b := grants(t, db, bob, "ns"); b.Allow("README", "read") || b.Any("read") {
		t.Error("default is not deny")
	}
	set(t, db, serverdb.ServerNamespace, "user:bob", "", "admin")
	if b := grants(t, db, bob, "ns"); !b.Whole("write") || !b.ServerAdmin() {
		t.Error("server admin is not admin everywhere")
	}
	// Whole read granted at "" with no narrowing entries.
	carol := &Caller{Principal: "user:carol"}
	set(t, db, "ns", "user:carol", "", "read")
	set(t, db, "ns", "user:carol", "docs/", "read", "write")
	if c := grants(t, db, carol, "ns"); !c.Whole("read") || c.Whole("write") {
		t.Error("Whole is wrong")
	}
}

func TestLocalTokens(t *testing.T) {
	db := newDB(t)
	if err := db.AddUser(ctx, "user:alice", []string{"group:eng", "group:ops"}); err != nil {
		t.Fatal(err)
	}
	_, secret, err := db.CreateToken(ctx, "user:alice", "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	chain := &Chain{Verifiers: []Verifier{&LocalTokens{DB: db}}}
	c, err := chain.Authenticate(ctx, secret, "test")
	if err != nil || c.Principal != "user:alice" || strings.Join(c.Groups, ",") != "group:eng,group:ops" {
		t.Fatalf("caller = %+v, %v", c, err)
	}
	for _, bad := range []string{"", "f1_nope", "garbage", secret + "x"} {
		if _, err := chain.Authenticate(ctx, bad, "test"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("token %q accepted", bad)
		}
	}
	db.SetDisabled(ctx, "user:alice", true)
	if _, err := chain.Authenticate(ctx, secret, "test"); err == nil {
		t.Error("disabled principal accepted")
	}
	db.SetDisabled(ctx, "user:alice", false)
	toks, _ := db.Tokens(ctx, "user:alice")
	db.RevokeToken(ctx, toks[0].ID)
	if _, err := chain.Authenticate(ctx, secret, "test"); err == nil {
		t.Error("revoked token accepted")
	}
	_, short, _ := db.CreateToken(ctx, "user:alice", "", time.Nanosecond)
	time.Sleep(1100 * time.Millisecond)
	if _, err := chain.Authenticate(ctx, short, "test"); err == nil {
		t.Error("expired token accepted")
	}
}

type provider struct{ *oidctest.Provider }

func newProvider(t *testing.T) provider { return provider{oidctest.New(t)} }

func (p provider) claims(extra map[string]any) map[string]any {
	return p.Claims("alice", "f1", []string{"eng", "ops"}, extra)
}

func (p provider) token(t *testing.T, kid string, alg jose.SignatureAlgorithm, claims map[string]any) string {
	return p.Token(t, kid, alg, claims)
}

func TestOIDC(t *testing.T) {
	db := newDB(t)
	p := newProvider(t)
	v, err := NewOIDC(OIDCConfig{Name: "corp", Issuer: p.Issuer(), Audiences: []string{"f1"}, GroupClaims: []string{"groups", "roles"}}, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	chain := &Chain{Verifiers: []Verifier{&LocalTokens{DB: db}, v}}
	auth := func(tok string) (*Caller, error) { return chain.Authenticate(ctx, tok, "test") }

	c, err := auth(p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"roles": "admins"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.Principal != "user:alice" || strings.Join(c.Groups, ",") != "group:eng,group:ops,group:admins" || c.Issuer != "corp" {
		t.Fatalf("caller = %+v", c)
	}
	// Just-in-time principal with its groups recorded for S3 keys.
	if g, _ := db.LastGroups(ctx, "user:alice", time.Hour); len(g) != 3 {
		t.Fatalf("recorded groups = %v", g)
	}

	hmacKey := []byte("0123456789abcdef0123456789abcdef")
	hmacSigner, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: hmacKey}, (&jose.SignerOptions{}).WithHeader("kid", "rsa1"))
	hmacTok, _ := jwt.Signed(hmacSigner).Claims(p.claims(nil)).Serialize()
	header := `{"alg":"none","kid":"rsa1"}`
	noneTok := b64(header) + "." + b64(mustJSON(p.claims(nil))) + "."

	rejected := map[string]string{
		"wrong audience":     p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"aud": "g1"})),
		"wrong issuer":       p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"iss": "https://evil.example"})),
		"expired":            p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()})),
		"not yet valid":      p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"nbf": time.Now().Add(5 * time.Minute).Unix()})),
		"no exp":             p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"exp": nil})),
		"unknown kid":        p.token(t, "rsa-unknown", jose.RS256, p.claims(nil)),
		"hmac":               hmacTok,
		"alg none":           noneTok,
		"no principal claim": p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"sub": nil})),
	}
	for name, tok := range rejected {
		if _, err := auth(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: accepted", name)
		}
	}
	// Expiry within the 60 s leeway is accepted.
	if _, err := auth(p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()}))); err != nil {
		t.Errorf("expiry within leeway: %v", err)
	}

	// Key rotation: a new key is picked up without restart, ES256 and EdDSA work.
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.AddKey("ec1", ec, jose.ES256)
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	p.AddKey("ed1", ed, jose.EdDSA)
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-time.Minute) // past the refresh rate limit
	v.mu.Unlock()
	if _, err := auth(p.token(t, "ec1", jose.ES256, p.claims(nil))); err != nil {
		t.Errorf("rotated ES256 key: %v", err)
	}
	if _, err := auth(p.token(t, "ed1", jose.EdDSA, p.claims(nil))); err != nil {
		t.Errorf("EdDSA key: %v", err)
	}

	// A principal name owned by a local user is refused.
	if err := db.AddUser(ctx, "user:bob", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth(p.token(t, "rsa1", jose.RS256, p.claims(map[string]any{"sub": "bob"}))); err == nil {
		t.Error("external token for a local user's name accepted")
	}
	// And a local user cannot take an external principal's name.
	if err := db.AddUser(ctx, "user:alice", nil); !errors.Is(err, serverdb.ErrPrincipalTaken) {
		t.Errorf("AddUser over an external principal: %v", err)
	}
}

// Unknown kids refresh the JWKS at most once per interval.
func TestOIDCRefreshRateLimit(t *testing.T) {
	db := newDB(t)
	p := newProvider(t)
	v, _ := NewOIDC(OIDCConfig{Name: "corp", Issuer: p.Issuer(), Audiences: []string{"f1"}}, db, nil)
	chain := &Chain{Verifiers: []Verifier{v}}
	if _, err := chain.Authenticate(ctx, p.token(t, "rsa1", jose.RS256, p.claims(nil)), ""); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		chain.Authenticate(ctx, p.token(t, "nope", jose.RS256, p.claims(nil)), "")
	}
	if hits := p.KeyFetches(); hits != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", p.KeyFetches())
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
