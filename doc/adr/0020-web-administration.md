# ADR-0020: Web administration in f1web

- Status: accepted
- Date: 2026-10-18
- Deciders: Rolf Veen

## Context

`TODO.md` asks for a web application to administer F1: manage users and ACLs, and let users get their own tokens and S3 keys, in a browser instead of with `f1d` and the API. It leaves open whether it is part of `f1web` or a separate server, which administrator rights it needs and how it gets them while calling the API as the signed-in user (ADR-0018, item 3), and what CSRF protection its changes need.

What exists today:

- `f1d user`, `token`, `s3key` and `acl` write `server.db` directly, on the server host. The native API has only `GET /me`, the caller's own S3 keys (`/me/s3keys`) and namespace creation (`POST /ns`, server administrators).
- Server administrators have `admin` on namespace `*` with prefix `""`. A namespace administrator has `admin` at prefix `""` of that namespace (ADR-0006).
- Local users authenticate with `f1_` tokens; OIDC users with the provider's access tokens, and F1 records them in `principals` with the groups of their last token (ADR-0005).
- `f1web` signs users in (OIDC, or a pasted `f1_` token), keeps sessions in memory and calls the native API as the user. It changes nothing yet; its only POSTs (sign-in, sign-out) already pass Go's `http.CrossOriginProtection`.
- Repository operations (freeze, attach, gc, backup, import) run on the host through `admin.sock`.

Several of the decisions below were discussed and rejected first: moving identity to a shared library or service with G1, and making F1 OIDC-only. F1 keeps its local users and its own `server.db`.

## Decision

### Architecture

1. **Part of `f1web`.** Administration is a set of pages in the same server, behind the same sign-in and sessions. A separate server would repeat sign-in, sessions, layout and configuration, and need a second OIDC client.
2. **Calls are made as the user (ADR-0018, item 3, unchanged).** `f1web` gains no credential of its own. Every change is an API call with the signed-in user's token, and F1 decides whether the caller may make it. `f1web` hides what the user may not do, but never relies on hiding.
3. **New native API endpoints**, so the CLI and other clients gain them too. `f1d`'s identity commands keep working on the host, for bootstrapping and recovery.

### Native API

4. **Self-service**, for any authenticated caller, about the caller only:

   | Endpoint | Does |
   |---|---|
   | `GET /api/v1/me/tokens` | the caller's tokens, without secrets |
   | `POST /api/v1/me/tokens` | create one: `{"description", "expires"}`; the secret is in the response only |
   | `DELETE /api/v1/me/tokens/{id}` | revoke one |
   | `GET`, `POST`, `DELETE /api/v1/me/s3keys…` | as today |

   - `expires` is required and at most `token_max_ttl` (configuration, default 90 days). Tokens without expiry stay possible only with `f1d token create`.
   - OIDC principals may create tokens too, for Git and scripts. A token of an OIDC principal carries the groups of its last OIDC token, for at most the same TTL as its S3 keys (ADR-0005). Today `LocalTokens.Verify` gives such a token no groups.

5. **Server administration**, for server administrators only (otherwise 403):

   | Endpoint | Does |
   |---|---|
   | `GET /api/v1/admin/principals` | local users and OIDC principals seen, with issuer, groups, disabled, created |
   | `POST /api/v1/admin/users` | add a local user: `{"name", "groups"}` |
   | `PUT /api/v1/admin/users/{name}/groups` | replace a local user's groups |
   | `PUT /api/v1/admin/principals/{principal}/disabled` | `{"disabled": true\|false}`; works for OIDC principals too |
   | `GET /api/v1/admin/tokens[?principal=]` | every token, without secrets |
   | `POST /api/v1/admin/tokens` | create a token for a principal; `expires` may be omitted |
   | `DELETE /api/v1/admin/tokens/{id}` | revoke any token |
   | `GET /api/v1/admin/s3keys[?principal=]`, `DELETE /api/v1/admin/s3keys/{key}` | list and revoke any S3 key |

6. **ACLs**, for administrators of the namespace (`admin` at prefix `""`) and server administrators. Namespace `*` is the server's and needs a server administrator.

   | Endpoint | Does |
   |---|---|
   | `GET /api/v1/ns/{ns}/acl` | the namespace's entries |
   | `PUT /api/v1/ns/{ns}/acl` | set one entry: `{"principal", "prefix", "perms"}`; `perms` is a list; `[]` or `["none"]` denies |
   | `DELETE /api/v1/ns/{ns}/acl?principal=&prefix=` | remove one entry |

   - Principals are written in full (`user:alice`, `group:eng`). Unknown principals are accepted, since OIDC users and groups appear only when they first sign in; listings mark each entry `known` or not, and the page warns.
   - Others get 404 for a namespace they cannot see and 403 for one they can see but not administer, as for other operations.
   - A namespace administrator can grant `admin` in that namespace, but nothing on `*`.

7. **Guards.**
   - No change may leave the server without an enabled server administrator: removing or narrowing the last `admin` entry on `*` with prefix `""`, disabling the last user who has it, or taking them out of the group that gives it, gives 409. Groups count when they have at least one enabled local member, or are among the recorded groups of an enabled OIDC user. A server that has no countable administrator already (for example, administrators in an OIDC group whose members have not used F1 yet) does not block other changes.
   - A caller cannot disable themselves.
   - Every change through these endpoints is logged as a security event (`security: admin change`), with the caller, the operation and its target, as authentication failures are today. Secrets are never logged.

### Pages

8. **URLs.** Settings use GitHub's places; f1web's own pages stay under `/-/`.

   | URL | Page | Who sees it |
   |---|---|---|
   | `/-/settings` | your principal and groups | everyone signed in |
   | `/-/settings/tokens` | your tokens: create, revoke | everyone signed in |
   | `/-/settings/s3keys` | your S3 keys: create, revoke | everyone signed in |
   | `/{ns}/settings/access` | the namespace's ACL, sorted by prefix, then principal | namespace and server administrators |
   | `/-/admin` | overview: users, OIDC principals, server administrators | server administrators |
   | `/-/admin/users`, `/-/admin/users/{name}` | add users, groups, disable, their tokens and S3 keys | server administrators |
   | `/-/admin/tokens`, `/-/admin/s3keys` | all credentials, revoke | server administrators |
   | `/-/admin/acl` | the ACL of `*` | server administrators |
   | `/-/admin/namespaces` | create a namespace from a preset | server administrators |

   - The header gets a user menu with Settings, and Administration for server administrators. A namespace's tabs get Settings for its administrators. `f1web` learns this from `GET /admin/principals` (403 or not) and `GET /ns/{ns}/acl`, cached in the session for a minute.
   - The access page also has a "check access" box: a principal and a path give the permission that applies and the entry that decides it. It is computed in `f1web` from the entries, with the matching rules of ADR-0006, so for a group it ignores entries of other groups the user may also have.

9. **Secrets are shown once.** A new token or S3 secret is shown on the page that answers the POST (no redirect), with `Cache-Control: no-store` and a copy button. It is never stored in the session.

10. **Forms work without JavaScript.** Each change is a POST of an HTML form, answered with a redirect to the page (except item 9). Destructive actions (revoke, disable, remove an entry) go through a confirmation page, not a JavaScript dialog.

### Protection

11. **CSRF.** Every unsafe request (`POST`, `PUT`, `DELETE`) to `f1web` passes Go's `http.CrossOriginProtection` (Go 1.25), as sign-in and sign-out already do: it rejects cross-origin requests by `Sec-Fetch-Site`, or `Origin` against `Host` in older browsers. `SameSite=Lax` stays. No new dependency, and no tokens in forms.
12. **Recent sign-in for administration.** Changes under `/-/admin` and `/{ns}/settings` require a sign-in no older than `admin_max_age` (configuration, default 1 hour). An older session is sent to sign in again: for OIDC with `max_age=0` (the provider asks again, with MFA if it requires it), for local users by pasting the token again. Self-service settings do not need it.
13. **Rate and size.** Admin and settings forms are small; f1web limits their bodies to 64 kB.

### Phases

14. Each phase passes `make ci`.
    - **A0:** this ADR.
    - **A1:** API for self-service tokens (item 4), groups for OIDC principals' tokens, `token_max_ttl`; `pkg/f1client` methods. Tests of `f1web`'s CSRF protection (item 11).
    - **A2:** `/-/settings` pages (items 8–10).
    - **A3:** ACL API (item 6), guards (item 7), security events; the namespace access page with "check access"; recent sign-in (item 12).
    - **A4:** server administration API (item 5) and the `/-/admin` pages; manual: a chapter on administering F1 in a browser, `f1web` configuration.

    Not included: repository operations (freeze, gc, backup, import, archive), which stay on the host; editing a namespace's configuration after creation; SCIM or provisioning from the provider; an audit log stored in `server.db` (the security events go to the log, which the SIEM collects).

## Consequences

- Users get their own tokens and S3 keys without an administrator, and namespace owners manage their own ACLs. Server administrators need the host only for repository operations.
- The API gains about fifteen endpoints, all usable from the CLI and `pkg/f1client`; `f1d`'s commands are unchanged.
- `f1web` starts changing state, so it gets CSRF protection and recent sign-in; the read-only pages are unaffected.
- Tokens of OIDC principals gain groups, which changes what existing tokens of such principals can do: with `group:` ACL entries they can now do more. The release notes say so.
- The last-administrator guard can refuse a legitimate change while the server is being reorganised; `f1d acl` on the host is not guarded and stays the way out.
- No change to the on-disk format, `server.db`'s schema or golden vectors.

## Alternatives considered

- **A separate administration server.** Clearer separation, but a second sign-in, session store, OIDC client and layout for a few pages. The attack surface is the same, because the rights are checked by F1 in both cases.
- **A privileged service credential for f1web.** Simpler endpoints (f1web would write as itself), but a compromised f1web could then do anything, and F1's logs would show f1web instead of the person. Rejected for the same reason as in ADR-0018.
- **Administration through `admin.sock`.** It is local to the host and authenticated by file permissions, so a web server would have to run beside `f1d` and act with full rights.
- **Synchronizer CSRF tokens in every form.** They work in browsers that send neither `Sec-Fetch-Site` nor `Origin`, which F1's users do not run, and need state per session and a field in every form. `http.CrossOriginProtection` is the standard library's answer and needs neither.
- **Moving identity out of F1** (a shared library or service with G1, or OIDC only). Discussed before this ADR and rejected: G1 stays OIDC-only for the legal weight of its signatures and MFA, and F1 keeps its local users.
