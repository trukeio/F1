package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/trukeio/f1/internal/serverdb"
)

// OIDCConfig configures one OIDC provider (ADR-0005).
type OIDCConfig struct {
	Name           string   `toml:"name"`            // internal issuer label
	Issuer         string   `toml:"issuer"`          // exact "iss"; discovery base URL
	Audiences      []string `toml:"audiences"`       // the token must name one of them
	PrincipalClaim string   `toml:"principal_claim"` // default "sub"
	GroupClaims    []string `toml:"group_claims"`    // default ["groups"]
	JWKSURL        string   `toml:"jwks_url"`        // optional; overrides discovery
}

// Validate checks the configuration and applies defaults.
func (c *OIDCConfig) Validate() error {
	if c.Name == "" || c.Issuer == "" || len(c.Audiences) == 0 {
		return errors.New("auth.oidc: name, issuer and audiences are required")
	}
	if c.Name == serverdb.LocalIssuer {
		return fmt.Errorf("auth.oidc: name %q is reserved", c.Name)
	}
	if c.PrincipalClaim == "" {
		c.PrincipalClaim = "sub"
	}
	if c.GroupClaims == nil {
		c.GroupClaims = []string{"groups"}
	}
	return nil
}

// Allowed signature algorithms: asymmetric only. "none" and HMAC are never
// accepted, so a public key can never be used as an HMAC secret.
var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.EdDSA}

// Clock skew tolerated for exp and nbf.
const leeway = 60 * time.Second

// Key cache timing.
const (
	jwksMaxAge      = 10 * time.Minute // refresh keys at least this often
	jwksMinInterval = 10 * time.Second // refresh on an unknown kid at most this often
)

// OIDC verifies JWT bearer tokens from one provider against its JWKS.
type OIDC struct {
	cfg  OIDCConfig
	db   *serverdb.DB
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	jwksURL   string
	keys      map[string]jose.JSONWebKey
	fetchedAt time.Time
}

// NewOIDC returns a verifier for one provider. Discovery and key fetching
// happen on first use, so the server starts even if the provider is
// briefly unreachable.
func NewOIDC(cfg OIDCConfig, db *serverdb.DB, client *http.Client) (*OIDC, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDC{cfg: cfg, db: db, http: client, now: time.Now, jwksURL: cfg.JWKSURL}, nil
}

// Verify implements Verifier.
func (o *OIDC) Verify(ctx context.Context, token string) (*Caller, error) {
	if strings.Count(token, ".") != 2 || strings.HasPrefix(token, serverdb.TokenPrefix) {
		return nil, ErrUnknownToken
	}
	tok, err := jwt.ParseSigned(token, allowedAlgs)
	if err != nil {
		return nil, fmt.Errorf("oidc %s: %w", o.cfg.Name, err)
	}
	var std jwt.Claims
	if err := json.Unmarshal(unverifiedPayload(token), &std); err != nil || std.Issuer != o.cfg.Issuer {
		// Another provider's token (or garbage): let the next verifier try.
		return nil, ErrUnknownToken
	}
	if len(tok.Headers) != 1 {
		return nil, fmt.Errorf("oidc %s: expected one signature", o.cfg.Name)
	}
	key, err := o.key(ctx, tok.Headers[0].KeyID)
	if err != nil {
		return nil, err
	}
	claims := map[string]any{}
	if err := tok.Claims(key, &std, &claims); err != nil {
		return nil, fmt.Errorf("oidc %s: %w", o.cfg.Name, err)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{Issuer: o.cfg.Issuer, AnyAudience: o.cfg.Audiences, Time: o.now()}, leeway); err != nil {
		return nil, fmt.Errorf("oidc %s: %w", o.cfg.Name, err)
	}
	if std.Expiry == nil {
		return nil, fmt.Errorf("oidc %s: token has no exp claim", o.cfg.Name)
	}
	sub, _ := claims[o.cfg.PrincipalClaim].(string)
	if sub == "" {
		return nil, fmt.Errorf("oidc %s: token has no usable %s claim", o.cfg.Name, o.cfg.PrincipalClaim)
	}
	c := &Caller{Principal: "user:" + sub, Issuer: o.cfg.Name}
	if err := serverdb.ValidPrincipal(c.Principal); err != nil {
		return nil, fmt.Errorf("oidc %s: %w", o.cfg.Name, err)
	}
	seen := map[string]bool{}
	for _, gc := range o.cfg.GroupClaims {
		for _, g := range stringsClaim(claims[gc]) {
			name := "group:" + g
			if serverdb.ValidPrincipal(name) == nil && !seen[name] {
				seen[name] = true
				c.Groups = append(c.Groups, name)
			}
		}
	}
	p, err := o.db.SeenExternal(ctx, c.Principal, o.cfg.Name, c.Groups)
	if err != nil {
		return nil, fmt.Errorf("oidc %s: %w", o.cfg.Name, err)
	}
	if p.Disabled {
		return nil, fmt.Errorf("oidc %s: principal %s is disabled", o.cfg.Name, c.Principal)
	}
	return c, nil
}

// unverifiedPayload decodes the payload of a compact JWS without checking
// it; it is used only to route the token to the right provider by "iss".
func unverifiedPayload(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return b
}

// stringsClaim reads a claim that is a string or a list of strings.
func stringsClaim(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// key returns the verification key for kid, refreshing the JWKS when it is
// old or the kid is unknown (rate-limited).
func (o *OIDC) key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	k, ok := o.keys[kid]
	stale := now.Sub(o.fetchedAt) > jwksMaxAge
	if ok && !stale {
		return k, nil
	}
	if stale || now.Sub(o.fetchedAt) > jwksMinInterval {
		if err := o.refresh(ctx); err != nil && !ok {
			return jose.JSONWebKey{}, err
		}
		k, ok = o.keys[kid]
	}
	if !ok {
		return jose.JSONWebKey{}, fmt.Errorf("oidc %s: unknown key id %q", o.cfg.Name, kid)
	}
	return k, nil
}

func (o *OIDC) refresh(ctx context.Context) error {
	o.fetchedAt = o.now() // also on failure, so a failing provider is not hammered
	if o.jwksURL == "" {
		var disc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := o.getJSON(ctx, strings.TrimSuffix(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
			return fmt.Errorf("oidc %s: discovery: %w", o.cfg.Name, err)
		}
		if disc.Issuer != o.cfg.Issuer || disc.JWKSURI == "" {
			return fmt.Errorf("oidc %s: discovery document names issuer %q and jwks_uri %q", o.cfg.Name, disc.Issuer, disc.JWKSURI)
		}
		o.jwksURL = disc.JWKSURI
	}
	var set jose.JSONWebKeySet
	if err := o.getJSON(ctx, o.jwksURL, &set); err != nil {
		return fmt.Errorf("oidc %s: jwks: %w", o.cfg.Name, err)
	}
	keys := map[string]jose.JSONWebKey{}
	for _, k := range set.Keys {
		if k.IsPublic() && (k.Use == "" || k.Use == "sig") {
			keys[k.KeyID] = k
		}
	}
	o.keys = keys
	return nil
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(v)
}
