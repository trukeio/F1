// Package oidctest is an in-process OIDC provider for tests: discovery, a
// JWKS with rotatable keys, token signing, and the authorization code flow
// with PKCE and refresh tokens for sign-in tests.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	client  Client
	codes   map[string]grant // authorization codes, each good once
	refresh map[string]grant // refresh tokens, rotated at each use
	nextID  int
	issued  int // access tokens issued by the token endpoint
}

// Client is the registered client and the user who signs in at the
// authorization endpoint (see SetClient).
type Client struct {
	ID, Secret string
	// Subject, Groups and Audience go into issued access tokens, which
	// expire after TTL.
	Subject  string
	Groups   []string
	Audience string
	TTL      time.Duration
}

// grant is what a code or refresh token stands for.
type grant struct {
	redirect, challenge string // codes only
}

// New starts a provider with one RS256 key, "rsa1".
func New(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{keys: map[string]crypto.Signer{}, codes: map[string]grant{}, refresh: map[string]grant{}}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys",
			"authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token"})
	})
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
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

// SetClient registers the client of the authorization code flow and the
// user who signs in.
func (p *Provider) SetClient(c Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = c
}

// RevokeRefreshTokens makes every issued refresh token invalid.
func (p *Provider) RevokeRefreshTokens() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh = map[string]grant{}
}

// Issued returns how many access tokens the token endpoint issued.
func (p *Provider) Issued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.issued
}

// authorize signs the configured user in at once and redirects back with
// a code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.mu.Lock()
	defer p.mu.Unlock()
	if q.Get("response_type") != "code" || q.Get("client_id") != p.client.ID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	code := p.newID("code")
	p.codes[code] = grant{redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge")}
	u := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	http.Redirect(w, r, u, http.StatusFound)
}

// token serves the authorization_code and refresh_token grants for a
// confidential client (client_secret_basic).
func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	id, secret, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	secret, _ = url.QueryUnescape(secret)
	if !ok || id != p.client.ID || secret != p.client.Secret {
		refuse("invalid_client")
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		g, ok := p.codes[r.FormValue("code")]
		delete(p.codes, r.FormValue("code"))
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || g.redirect != r.FormValue("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			refuse("invalid_grant")
			return
		}
	case "refresh_token":
		if _, ok := p.refresh[r.FormValue("refresh_token")]; !ok {
			refuse("invalid_grant")
			return
		}
		delete(p.refresh, r.FormValue("refresh_token"))
	default:
		refuse("unsupported_grant_type")
		return
	}
	c := p.client
	exp := time.Now().Add(c.TTL)
	claims := map[string]any{"iss": p.srv.URL, "aud": c.Audience, "sub": c.Subject, "groups": c.Groups,
		"iat": time.Now().Unix(), "exp": exp.Unix()}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.keys["rsa1"]}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "rsa1"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt := p.newID("refresh")
	p.refresh[rt] = grant{}
	p.issued++
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer",
		"expires_in": int64(c.TTL / time.Second), "refresh_token": rt})
}

// newID returns a unique opaque value; p.mu must be held.
func (p *Provider) newID(kind string) string {
	p.nextID++
	return fmt.Sprintf("%s-%d", kind, p.nextID)
}
