// Package oidctest is an in-process OIDC provider for tests: discovery, a
// JWKS with rotatable keys, and token signing.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Provider is a test OIDC provider.
type Provider struct {
	srv  *httptest.Server
	mu   sync.Mutex
	keys map[string]crypto.Signer
	pub  jose.JSONWebKeySet
	hits int
}

// New starts a provider with one RS256 key, "rsa1".
func New(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{keys: map[string]crypto.Signer{}}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.hits++
		json.NewEncoder(w).Encode(p.pub)
	})
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.AddKey("rsa1", k, jose.RS256)
	return p
}

// Issuer returns the issuer URL.
func (p *Provider) Issuer() string { return p.srv.URL }

// KeyFetches returns how often the JWKS was fetched.
func (p *Provider) KeyFetches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

// AddKey publishes a new signing key (key rotation).
func (p *Provider) AddKey(kid string, priv crypto.Signer, alg jose.SignatureAlgorithm) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[kid] = priv
	var pub any
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		pub = &k.PublicKey
	case *ecdsa.PrivateKey:
		pub = &k.PublicKey
	case ed25519.PrivateKey:
		pub = k.Public()
	}
	p.pub.Keys = append(p.pub.Keys, jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: string(alg), Use: "sig"})
}

// Claims returns valid claims for subject sub with audience aud and the
// given groups, merged with extra (a nil value removes a claim).
func (p *Provider) Claims(sub, aud string, groups []string, extra map[string]any) map[string]any {
	c := map[string]any{"iss": p.srv.URL, "aud": aud, "sub": sub, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(), "groups": groups}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// Token signs claims with key kid. An unpublished kid signs with a fresh
// key the provider never published.
func (p *Provider) Token(t testing.TB, kid string, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	p.mu.Lock()
	key := p.keys[kid]
	p.mu.Unlock()
	if key == nil {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
