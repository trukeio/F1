# ADR-0018: Web viewer as a separate client of the native API

- Status: accepted
- Date: 2026-10-17
- Deciders: Rolf Veen

## Context

`TODO.md` asks for a web interface to an F1 installation: browse namespaces, files and history, and view file contents, looking and working much like GitHub. It leaves open whether it runs inside the F1 server or as a separate client of its APIs, how users sign in, and what it can change beyond viewing.

Constraints:

- A web UI is a version 1 non-goal of the server (`doc/plan.md`, "Non-goals").
- The native API authenticates only with `Authorization: Bearer` (D17, ADR-0005). Users of the enterprise provider get tokens with the authorization code flow with PKCE; OIDC login in F1's own clients was left for later.
- ACLs are evaluated per caller, and path-level read ACLs filter `tree`, `log` and `diff` (D13, ADR-0006). Unknown and inaccessible namespaces both give 404.
- D19: no behaviour depends on file size, and every read path streams.
- New dependencies need the user's approval (`doc/plan.md`, "Dependencies").

## Decision

### Architecture

1. **A separate server, `f1web`, in this repository and module.** It is built as `cmd/f1web` from `internal/web`, and talks to F1 only through `pkg/f1client` over HTTP. `f1d` gains no HTML, sessions or cookies.
2. **Imports.** `internal/web` imports no package under `internal/` except `internal/client/textmerge`, for line diffs. A test enforces this.
3. **Calls are made as the user.** Every API call carries the signed-in user's token, so F1 applies ACLs, D13 and the 404 rule unchanged. `f1web` holds no credential of its own that could read more than the user can.
4. **Rendering on the server** with `html/template`, CSS and a little JavaScript embedded with `embed`. No single-page application and no JavaScript build step.

### Sign-in

5. **OIDC, authorization code flow with PKCE.** `f1web` is a confidential client of the provider that F1 and G1 trust (ADR-0005). Its access tokens must carry audience `f1`, which the provider is configured to add (a Keycloak audience mapper, or an `api://f1` scope in Entra ID). Token exchange (RFC 8693) can replace this later.
6. **Local users** sign in by pasting an `f1_` token.
7. **Sessions are kept on the server, in memory.** The browser gets only a random session ID in an `HttpOnly`, `Secure`, `SameSite=Lax` cookie. Access and refresh tokens never reach the browser. `f1web` refreshes access tokens itself. A restart signs everyone out.

### Pages

8. **URLs follow GitHub's.** Refs are one path segment, with `/` written as `%2F`, as in the native API (ADR-0006, item 11).

   | URL | Page |
   |---|---|
   | `/` | namespaces the user can see |
   | `/{ns}` | the default ref's root directory and its README |
   | `/{ns}/tree/{ref}/{path}` | a directory, with a ref picker |
   | `/{ns}/blob/{ref}/{path}` | a file: text with line numbers, images inline, otherwise a download link |
   | `/{ns}/raw/{ref}/{path}` | the file content, streamed, with `Range` and ETags passed through |
   | `/{ns}/commits/{ref}[/{path}]` | history, paged |
   | `/{ns}/commit/{hash}` | a commit and its diff |
   | `/{ns}/compare/{a}...{b}` | the diff between two refs or commits |
   | `/{ns}/branches`, `/{ns}/tags`, `/{ns}/locks` | refs and locks |

9. **Permalinks.** A "copy permalink" action rewrites the URL with the commit hash. Pages addressed by hash can use the immutable `private` caching of ADR-0016.
10. **Namespace kinds.** A `flat` namespace is shown as a paged key browser. Pages that need history are hidden when the namespace keeps none.
11. **Partial access.** A user without whole-namespace read cannot use `log` without a path (D13). The history page then asks for a path, and the commit page shows only what the user can read.
12. **Size.** Raw downloads always stream. Rendered views (highlighted text, Markdown, diffs) stop at a configured size and offer the raw file instead. This limits what `f1web` renders, not what F1 serves, so D19 holds for the API.

### Native API

13. **`log` gets `skip`.** `GET /ns/{ns}/log/{ref}?skip=N` leaves out the first N commits of the walk. A commit cannot serve as a cursor, because the walk merges branches by committer time and its position depends on the whole queue. `skip` is exact and costs a walk of `skip + limit` commits, which suits pages of a UI. The Go client gets `LogPage`.
14. **One commit by hash** is `log/{hash}?limit=1`, which already works: refs resolve full commit hashes. No new endpoint is needed.

### Dependencies

15. **Approved for `internal/web` only:**
    - `github.com/yuin/goldmark`: Markdown, for READMEs and `.md` files. Raw HTML in Markdown is not rendered.
    - `github.com/alecthomas/chroma/v2`: syntax highlighting on the server.

### Phases

16. The viewer is built in phases, each passing `make ci`:
    - **W0:** this ADR and the API change of item 13.
    - **W1:** `cmd/f1web` skeleton, configuration, sign-in and sessions.
    - **W2:** the read-only pages of item 8.
    - **W3:** syntax highlighting, a "last commit" column in directory listings (one path `log` per entry, using ADR-0015's filters), keyboard shortcuts.

    Changes beyond viewing (editing a file with `F1-Parent`, branches, locks) need CSRF protection and are a separate decision. Blame, search and anonymous browsing are deferred. Anonymous browsing depends on anonymous reads in F1, which are still open in `TODO.md`.

## Consequences

- The server's scope and attack surface do not grow. HTML, sessions and CSRF live in a process that can be restarted, deployed and scaled on its own, and pointed at any F1 server.
- Each page costs one or a few API calls, each with a fixed cost of about 200 µs (`doc/benchmarks.md`, "Web site files"), which is small for a UI.
- Gaps the viewer finds are closed in the native API, so the CLI and other clients gain them too.
- Deployments that use the viewer need one more OIDC client registration and an audience mapping at the provider.
- Two new dependencies, used only by `f1web`; `f1d` and `f1` do not link them.
- No change to the on-disk format or to golden vectors.

## Alternatives considered

- **Inside `f1d`.** It saves one network hop and one process. But the API would have to accept session cookies, which then need CSRF protection on every endpoint, and HTML rendering and sessions would run in the storage server. It would also contradict the version 1 non-goal.
- **A single-page application calling the API from the browser.** Bearer tokens would live in the browser, exposed to any script injection, and the API would need CORS. It also needs a JavaScript build toolchain the project does not have.
- **A separate repository.** It would lose the shared types and CI, and the reuse of `textmerge`, for no gain while one person maintains both.
- **A commit cursor for `log` (`after=<hash>`).** Not exact for histories with merges; see item 13.
- **Highlighting in the browser (highlight.js).** It avoids a Go dependency but needs a vendored script and gives a page that renders twice. Highlighting on the server keeps the pages static.
