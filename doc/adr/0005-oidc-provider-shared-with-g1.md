# ADR-0005: Authenticate against the OIDC provider shared with G1

- Status: accepted (amends D17 in ADR-0001)
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

D17 and the plan assumed that G1 issues JWTs that F1 verifies, and left two questions open: G1's token format, and its claims and audience. Reading G1 (`../g1`: `internal/api/auth.go`, `doc/spec/access.md`, plan §10.1, `deploy/`) answered both:

- G1 issues no tokens. It is an OIDC relying party: every caller presents a bearer token from the enterprise identity provider (Keycloak, Microsoft Entra ID, ...). G1 verifies it against the provider's JWKS, found through OIDC discovery, checking signature, issuer, expiry and an audience from a configured list.
- Users get tokens through the authorization code flow with PKCE, services through client credentials, and back-ends acting for a user through token exchange (RFC 8693).
- The principal is `user:<value of a configurable claim, default sub>`, and the groups are `group:<value>` for each value of the configured group claims (default `groups`; string or list). Groups are read from the token on every request; there is no directory synchronisation.
- Each service has its own audience. G1's example configuration uses `g1`.

## Decision

1. **Trust the provider, not G1.** F1 trusts the same OIDC provider as G1. It is registered there as its own client, with audience `f1`, so the same users and groups work in both systems. No introspection verifier is needed.
2. **Configuration.** The provider is configured by issuer URL with discovery (`[[auth.oidc]]`: `issuer`, `audiences`, `principal_claim` default `sub`, `group_claims` default `["groups"]`). An explicit `jwks_url` is an optional override.
3. **Principal names follow G1:** `user:<name>` and `group:<name>`, in ACLs, locks, `refs.log` and commit authors. The authenticating issuer is recorded with the principal but is internal. A principal name belongs to one issuer. Collisions between issuers, or with local users, are refused.
4. **Groups come from the token on each request.** Membership changes take effect when the token is refreshed. For S3 keys, which cannot carry a token, the groups seen at the principal's last token-authenticated request are used, and they expire after a configured time (default 24 h).

## Consequences

- One identity setup serves both G1 and F1. An ACL written for `group:engineering` means the same in both.
- F1 keeps no group directory, as G1 doesn't; revocation of a group membership waits for token expiry (short lifetimes are the provider's setting).
- S3 access with stored keys may use groups that are up to the expiry time old. Removing the S3 key, or the principal's ACL entries, is immediate.
- The plan's `(issuer, subject)` identity becomes a principal name plus a recorded issuer. This is simpler for administrators and the same as G1.

## Alternatives considered

- **Treat G1 as an issuer.** Not possible: G1 issues no tokens.
- **Keep `(issuer, subject)` as the principal identity in ACLs.** This is more general for many providers, but administrators would write ACLs differently in F1 and G1. Collisions are handled by refusing them instead.
- **Synchronise groups from the provider (SCIM).** This gives immediate revocation, but it is not done by G1 either. It can be added later for both.
