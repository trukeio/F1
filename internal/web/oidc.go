package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oidcClient signs users in with the authorization code flow with PKCE
// (ADR-0018, item 5). It does not verify ID tokens: f1web trusts no claim
// of its own, and learns the principal by calling F1 with the access
// token, which F1 verifies.
type oidcClient struct {
	cfg      OIDCConfig
	redirect string
	http     *http.Client

	mu        sync.Mutex
	endpoints *oidcEndpoints // nil until discovered
}

type oidcEndpoints struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
}

// tokenResponse is the token endpoint's answer (RFC 6749 §5.1).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// expiry returns when the access token expires; zero if not stated.
func (t tokenResponse) expiry(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// discover returns the provider's endpoints, fetching them on first use
// and again after a failure, so f1web starts while the provider is down.
func (o *oidcClient) discover(ctx context.Context) (*oidcEndpoints, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.endpoints != nil {
		return o.endpoints, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: %s", resp.Status)
	}
	var e oidcEndpoints
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if e.Issuer != o.cfg.Issuer {
		return nil, fmt.Errorf("oidc discovery: issuer is %q, configured %q", e.Issuer, o.cfg.Issuer)
	}
	if e.Authorization == "" || e.Token == "" {
		return nil, errors.New("oidc discovery: authorization or token endpoint missing")
	}
	o.endpoints = &e
	return o.endpoints, nil
}

// pkceChallenge returns the S256 challenge of a verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authURL returns the provider URL that starts a sign-in.
func (o *oidcClient) authURL(ctx context.Context, state, verifier string, reauth bool) (string, error) {
	e, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.cfg.ClientID},
		"redirect_uri":          {o.redirect},
		"scope":                 {strings.Join(o.cfg.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	if reauth {
		// The provider asks the user again (with MFA if it requires it)
		// instead of reusing its own session.
		q.Set("max_age", "0")
		q.Set("prompt", "login")
	}
	sep := "?"
	if strings.Contains(e.Authorization, "?") {
		sep = "&"
	}
	return e.Authorization + sep + q.Encode(), nil
}

// exchange trades an authorization code for tokens.
func (o *oidcClient) exchange(ctx context.Context, code, verifier string) (tokenResponse, error) {
	return o.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {o.redirect},
		"code_verifier": {verifier},
	})
}

// refresh trades a refresh token for new tokens.
func (o *oidcClient) refresh(ctx context.Context, refreshToken string) (tokenResponse, error) {
	return o.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func (o *oidcClient) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	var t tokenResponse
	e, err := o.discover(ctx)
	if err != nil {
		return t, err
	}
	if o.cfg.ClientSecret == "" {
		form.Set("client_id", o.cfg.ClientID) // a public client
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return t, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.cfg.ClientSecret != "" {
		// client_secret_basic: both parts form-encoded (RFC 6749 §2.3.1).
		req.SetBasicAuth(url.QueryEscape(o.cfg.ClientID), url.QueryEscape(o.cfg.ClientSecret))
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return t, fmt.Errorf("oidc token: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return t, fmt.Errorf("oidc token: %s: %w", resp.Status, err)
	}
	if t.Error != "" {
		return t, fmt.Errorf("oidc token: %s: %s", t.Error, t.Description)
	}
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		return t, fmt.Errorf("oidc token: %s, no access token", resp.Status)
	}
	return t, nil
}
