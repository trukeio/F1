// Package auth authenticates callers (local tokens, OIDC bearer tokens from
// the enterprise identity provider) and authorizes them with path ACLs.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/serverdb"
)

// Caller is an authenticated principal with its groups.
type Caller struct {
	Principal string   // "user:<name>"
	Groups    []string // "group:<name>"
	Issuer    string   // "local" or the OIDC provider name; internal
}

// Principals returns the caller's principal followed by its groups: the
// names ACL entries can match.
func (c *Caller) Principals() []string {
	return append([]string{c.Principal}, c.Groups...)
}

// ErrUnknownToken is returned by a verifier for tokens it does not handle;
// the next verifier is tried.
var ErrUnknownToken = errors.New("auth: token not handled by this verifier")

// ErrUnauthenticated is returned for missing or invalid credentials. Its
// message is safe to show to clients; the reason is logged separately.
var ErrUnauthenticated = errors.New("invalid or missing credentials")

// Verifier turns a bearer token into a caller.
type Verifier interface {
	Verify(ctx context.Context, token string) (*Caller, error)
}

// Chain tries verifiers in order (D17) and records every failure as a
// security event in the log.
type Chain struct {
	Verifiers []Verifier
	Log       *slog.Logger
}

// Authenticate verifies a bearer token. The error is ErrUnauthenticated
// for the client; the detailed reason is logged.
func (c *Chain) Authenticate(ctx context.Context, token, remote string) (*Caller, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	var lastErr error = ErrUnknownToken
	for _, v := range c.Verifiers {
		caller, err := v.Verify(ctx, token)
		if errors.Is(err, ErrUnknownToken) {
			continue
		}
		if err != nil {
			lastErr = err
			break
		}
		return caller, nil
	}
	if c.Log != nil {
		c.Log.Warn("security: authentication failed", "remote", remote, "reason", lastErr.Error())
	}
	return nil, ErrUnauthenticated
}

// LocalTokens verifies the server's own opaque tokens ("f1_...").
type LocalTokens struct {
	DB *serverdb.DB
	// GroupTTL: tokens of external principals carry the groups of their
	// last OIDC token for at most this long, as S3 keys do (ADR-0020).
	GroupTTL time.Duration
}

// Verify implements Verifier.
func (l *LocalTokens) Verify(ctx context.Context, token string) (*Caller, error) {
	if !strings.HasPrefix(token, serverdb.TokenPrefix) {
		return nil, ErrUnknownToken
	}
	p, err := l.DB.VerifyToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("local token: %w", err)
	}
	if p.Disabled {
		return nil, fmt.Errorf("local token: principal %s is disabled", p.Name)
	}
	return callerFor(ctx, l.DB, p, l.GroupTTL)
}

// callerFor builds the caller for a principal authenticated without a
// token that carries groups (local tokens, S3 keys): local users get their
// stored groups, external principals the groups of their last token use
// if it is recent enough (maxAge; 0 for none).
func callerFor(ctx context.Context, db *serverdb.DB, p serverdb.Principal, maxAge time.Duration) (*Caller, error) {
	c := &Caller{Principal: p.Name, Issuer: p.Issuer}
	var err error
	if p.Issuer == serverdb.LocalIssuer {
		c.Groups, err = db.LocalGroups(ctx, p.Name)
	} else if maxAge > 0 {
		c.Groups, err = db.LastGroups(ctx, p.Name, maxAge)
	}
	return c, err
}

// S3KeyCaller returns the caller for an S3 access key's principal; the
// secret is checked by the SigV4 layer. External principals carry the
// groups of their last token use for at most groupTTL (ADR-0005).
func S3KeyCaller(ctx context.Context, db *serverdb.DB, k serverdb.S3Key, groupTTL time.Duration) (*Caller, error) {
	p, err := db.LookupPrincipal(ctx, k.Principal)
	if err != nil {
		return nil, err
	}
	if p.Disabled {
		return nil, fmt.Errorf("principal %s is disabled", p.Name)
	}
	return callerFor(ctx, db, p, groupTTL)
}
