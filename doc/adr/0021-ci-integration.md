# ADR-0021: Continuous integration: events, webhooks and commit statuses

- Status: proposed
- Date: 2026-10-19
- Deciders: Rolf Veen

## Context

F1 hosts Git-style namespaces whose users want continuous integration: build and test every push, and see the result next to the commit, as on GitHub. A use case that drives this ADR is firmware for microcontroller boards (Raspberry Pi Pico, ESP32), tested on emulators (Espressif's QEMU, Renode, Wokwi) and on real boards: a board farm, where a runner host flashes boards over SWD/JTAG, reads results from their serial ports and power-cycles them through switchable USB hubs or relays.

A CI system has three parts: an event source (a push), a coordinator (job definitions, queues, matching jobs to runners, logs) and runners. Building the coordinator and runners into F1 would make it a second product: job syntax, scheduling, log storage, artifacts, secrets, sandboxing. F1 is a storage system (`doc/plan.md`, "Goals"). Mature tools already do the rest, including the hardware part: Buildbot, Woodpecker, Forgejo's `act_runner` for coordination; Labgrid, LAVA and Espressif's `pytest-embedded` for boards and emulators.

What these tools need from a repository host is small:

- **To learn about changes.** Either F1 calls them (webhooks), or they ask F1. A board farm usually sits in a lab behind NAT, where it cannot receive webhooks, so asking must be cheap and immediate (long polling), not a `git ls-remote` loop.
- **To report results** on a commit, so that people see them in `f1web` and clients can query them.

What exists today:

- Every ref change in every protocol (native, S3, Git, group commits) goes through `UpdateRef` and is appended to the repository's `refs.log` as a group of lines `<time> <tx> <i>/<n> <principal> <refname> <old> <new> <reason>`, fsynced, before it becomes visible (ADR-0003). `refs.log` is append-only and never rotated.
- `server.db` holds identity and ACLs; the repository directory holds only repository state, and import and attach take nothing else (ADR-0004, item 4).
- ACL permissions are `read`, `write`, `lock` and `admin`; access by commit hash needs whole-namespace read (D13, ADR-0006).
- `f1web` changes state through HTML forms with CSRF protection and recent sign-in for settings (ADR-0020).
- Git commit IDs of F1 commits come from `git_map` in `cache.db`, computed on demand (ADR-0008).

## Decision

### Scope

1. **F1 provides the integration points, not a CI engine.** It gains an event feed (item 3), webhooks (items 4–9) and commit statuses (items 10–14). It does not run jobs, store logs or artifacts, or define a job syntax. Coordinators, runners and board farms are external.

### Events

2. **One event per `refs.log` group.** An event is a complete group: the refs it changed, with their old and new values, the principal, the time, the reason and the group ID (`tx`). A multi-ref (atomic) push is one event. Creation and deletion are an empty old or new value. Events are read from `refs.log`, which is the commit point, so an event exists if and only if the change became visible, and nothing new is written in the commit path.

   ```json
   {
     "event": "ref.update",
     "namespace": "firmware",
     "tx": "01JAB…",
     "time": "2026-10-19T10:15:02Z",
     "principal": "user:alice",
     "reason": "push",
     "refs": [
       {"ref": "heads/main", "old": "<f1 hash>", "new": "<f1 hash>",
        "old_git": "<sha1>", "new_git": "<sha1>"}
     ],
     "cursor": "…"
   }
   ```

   - `old_git` and `new_git` are present in namespaces with `git = true`, since runners check out by Git commit ID. They are computed and memoized as for a fetch.
   - Events carry no paths, file names or commit messages: the receiver reads those with its own credentials, so the ACLs decide what it sees.

3. **Event feed.** `GET /api/v1/ns/{ns}/events?after=<cursor>&wait=<seconds>&limit=<n>` returns `{"events": [...], "cursor": "…"}`.
   - Needs whole-namespace read.
   - Without `after`, it returns no events and the current cursor, so a new client starts at the present.
   - With `wait` (at most 60 s), it holds the request until there is an event or the time is up (long polling). The server wakes waiting requests when it appends to `refs.log`; it does not poll the file.
   - The cursor is opaque. It holds the repository UUID, a byte offset in `refs.log` and the `tx` found there. If the UUID or the `tx` at that offset do not match (the repository was replaced by import, attach or `--new-uuid`), the answer is `410 Gone` with the current cursor; the client then does a full sync of its own.
   - `limit` defaults to 100 and is at most 1000. A client that is behind gets the backlog in order, page by page.
   - This is what a runner behind NAT uses. A board farm needs only outbound HTTPS to F1.

### Webhooks

4. **Per-namespace hooks**, managed by the namespace's administrators (`admin` at prefix `""`) and server administrators, with the same 404/403 rules as ACLs (ADR-0020, item 6).

   | Endpoint | Does |
   |---|---|
   | `GET /api/v1/ns/{ns}/hooks` | the namespace's hooks, without secrets |
   | `POST /api/v1/ns/{ns}/hooks` | create one: `{"url", "refs", "active", "secret"?}` |
   | `GET`, `PATCH`, `DELETE /api/v1/ns/{ns}/hooks/{id}` | read, change, remove one |
   | `POST /api/v1/ns/{ns}/hooks/{id}/ping` | send a `ping` event now |
   | `GET /api/v1/ns/{ns}/hooks/{id}/deliveries` | the last 50 deliveries: event, time, HTTP status, duration, error |
   | `POST /api/v1/ns/{ns}/hooks/{id}/deliveries/{delivery}/redeliver` | send that event again |

   - `refs` is a list of globs on full ref names (`heads/*`, `tags/v*`); empty means all. An event is delivered if any of its refs match, with only the matching refs in it.
   - `url` is `https://`, or `http://` only to hosts allowed by item 8.
   - If `secret` is omitted, F1 generates 32 random bytes. The secret is in the response to the POST only (as tokens, ADR-0020, item 9). It can be replaced with `PATCH`, never read.
   - Hooks are stored in `server.db` (a new migration), like ACLs: they are server configuration about a namespace, not repository state, and do not travel with a copied repository.

5. **Request.** `POST` of the event as JSON, with headers:
   - `F1-Event: ref.update` (or `ping`),
   - `F1-Delivery: <ulid>`, unique per delivery attempt chain, kept on retries so receivers can deduplicate,
   - `F1-Signature-256: sha256=<hex>`, HMAC-SHA256 of the body with the hook's secret, as GitHub's `X-Hub-Signature-256`, so existing receivers need only a header name changed,
   - `User-Agent: F1-Hookshot/<version>`.

   A 2xx answer within 10 seconds is success; anything else, a redirect included, is a failure. Response bodies are read up to 4 kB, for the deliveries list, and discarded.

6. **Delivery: at least once, in order, from a cursor.** Each hook has a cursor in `server.db`, of the same form as in item 3. One dispatcher goroutine per hook reads events after its cursor, sends them one at a time, and advances the cursor after each success.
   - Retries back off exponentially from 10 s to 1 h. A hook that has failed for `webhook_max_failing` (configuration, default 3 days) is set inactive, with its last error; the namespace's settings page shows it, and the log gets a warning. Reactivating it starts at the present (as a new feed client), and the skipped events can be read through the feed.
   - When a hook is behind by more than one event, consecutive events that change only the same single ref are merged into one, with the old value of the first and the new value of the last. This keeps autocommit namespaces, which can commit hundreds of times per second (ADR-0012), from flooding receivers. The merged event has the `tx` and `cursor` of the last one.
   - A cursor that no longer matches (as in item 3) moves to the present, logged as a warning.
   - On startup, dispatchers resume from their cursors, so events from while the server was down are delivered.

7. **Concurrency.** At most `webhook_concurrency` deliveries run at once (configuration, default 16) across all hooks. A slow receiver delays only its own hook.

8. **Outbound address rules.** Namespace administrators are not server administrators, and a hook makes the server send requests where they say. By default, F1 refuses to connect to loopback, link-local, private (RFC 1918, ULA), CGNAT and multicast addresses. The check is on the address the connection is made to, after DNS resolution (in the dialer's `Control` function), so DNS rebinding does not bypass it. `webhook_allow` (configuration, a list of CIDRs and host names) allows exceptions, for a coordinator on the local network; `http://` is allowed only to those. Refused deliveries fail with the reason in the deliveries list.

9. **Security events.** Creating, changing and deleting hooks is logged as `security: admin change`, as in ADR-0020, item 7. Secrets are never logged or shown again.

### Commit statuses

10. **A status** is `{context, state, description, target_url}` on a commit in a namespace, set by a principal at a time.
    - `context` names the check (`ci/build`, `hil/pico`, `hil/esp32-s3`): 1–100 printable characters.
    - `state` is `pending`, `success`, `failure` or `error`.
    - `description` is up to 200 characters; `target_url` is an `https://` or `http://` URL up to 2000 characters, typically the job's log. Both are optional.
    - Setting a context again replaces it; the last 10 states per context are kept, for the commit page's history. A commit holds at most 100 contexts.

11. **API.**

    | Endpoint | Does |
    |---|---|
    | `POST /api/v1/ns/{ns}/statuses/{commit}` | set one context |
    | `GET /api/v1/ns/{ns}/statuses/{commit}` | the latest state of every context, and the combined state |
    | `GET /api/v1/ns/{ns}/statuses/{commit}?context=<c>` | one context with its history |

    - `{commit}` is an F1 commit hash, a Git commit ID (40 hex digits, in namespaces with `git = true`, resolved through `git_map`) or a ref name, resolved at the time of the call. The answer always gives both hashes where they exist. A commit that does not exist in the namespace is 404.
    - The combined state is `failure` if any context is `failure` or `error`, otherwise `pending` if any is `pending`, otherwise `success`, or absent with no contexts.

12. **Permissions.** Reading statuses needs whole-namespace read, as other reads by commit hash do (D13). Setting them needs whole-namespace read and the new permission `status` at prefix `""`. `write` and `admin` at prefix `""` imply `status`. A CI bot is a user with `read` and `status`: it can report results without being able to change files.

13. **Storage.** Statuses are in `server.db`, keyed by namespace and F1 commit hash, for the same reason as hooks (item 4). Commits that GC removes (retention, ADR-0011) lose their statuses in the same GC run. Deleting a namespace removes its hooks and statuses.

14. **Statuses are informational.** They do not block ref updates. Protected branches that require passing checks are a separate decision, not taken here.

### Clients and pages

15. **`pkg/f1client`** gains methods for the feed (with a helper that follows the cursor and handles 410), hooks and statuses. The `f1` CLI gains `f1 status <commit>` and `f1 status set <commit> <context> <state> [--description] [--url]`, so a CI script can report a result without curl.

16. **`f1web`**, following ADR-0020 (forms without JavaScript, CSRF protection, recent sign-in for settings):
    - The combined state as an icon (pending, success, failure) next to commits in history pages, on the branches page and in the header of the tree view; each context with its description and link on the commit page.
    - `/{ns}/settings/hooks`: list, create (secret shown once), edit, delete, ping, the last deliveries with their status, and redeliver. Inactive hooks are marked, with their last error.

### Documentation

17. **The manual gains a chapter on CI with F1**:
    - receiving webhooks and checking signatures; following the feed from behind NAT; reporting statuses with `f1 status set`;
    - an example runner script of a few dozen lines that follows the feed, checks out the new commit with Git, runs a command and reports the result;
    - hardware-in-the-loop notes for Pico and ESP32: flashing over SWD/JTAG instead of BOOTSEL or strap pins, stable device names with udev rules on serial numbers, power-cycling hung boards, one job per board, emulators on every push and boards for the default branch, and where Labgrid, LAVA and `pytest-embedded` fit.

### Phases

18. Each phase passes `make ci`.
    - **C0:** this ADR.
    - **C1:** commit statuses: `server.db` migration, the `status` permission, API (items 10–13), `pkg/f1client`, `f1 status`; statuses in `f1web`'s pages.
    - **C2:** event feed (items 2–3): reading `refs.log` from a cursor, long polling, 410 on a replaced repository; `pkg/f1client` helper.
    - **C3:** webhooks (items 4–9): `server.db` migration, dispatchers, signatures, retries and merging, outbound address rules, deliveries; `/{ns}/settings/hooks`. Tests with an `httptest` receiver: ordering, retry after failure, at-least-once across a server restart, merging under autocommit load, refused private addresses, DNS names that resolve to them.
    - **C4:** the manual chapter (item 17) with the example runner, tested in the smoke test against a local server.

    Not included: running jobs, logs, artifacts and secrets (external tools); protected branches that require checks (item 14); server-wide hooks on namespace `*` for all namespaces; skipping CI by push options or commit message (the receiver decides); events other than ref updates (locks, ACL changes).

## Consequences

- CI tools, board farms and scripts can build every change of any protocol, including S3 and native writes, and report back; `f1web` shows the results.
- A board farm in a lab needs only outbound HTTPS and a token with `read` and `status`.
- The commit path is unchanged: events come from `refs.log`, which is already written and fsynced. Delivery adds reads of `refs.log` tails and outbound HTTP, in the background.
- The server makes outbound requests for the first time. The address rules (item 8) limit them, but administrators who allow internal addresses widen what namespace administrators can reach.
- Hook secrets must be stored in a form that can sign, so they are in `server.db` as written; anyone who can read `server.db` can forge deliveries. `server.db` already holds credential hashes and is protected accordingly.
- ACLs gain a fifth permission, `status`. Existing `write` and `admin` entries gain it implicitly. `f1d acl`, the ACL API and the access page accept it.
- `server.db` gains tables for hooks, hook cursors, deliveries and statuses. No change to the repository directory, the on-disk format, or golden vectors.
- Hooks and statuses do not travel with a copied or archived repository (ADR-0004); after an import, they are set up again.

## Alternatives considered

- **A CI engine in F1** (job syntax, scheduler, runners, logs). The largest share of the work, and it duplicates mature tools; F1 would still need everything in this ADR to talk to its own runners.
- **A Git `post-receive`-style hook script on the server.** Covers Git pushes only, not native and S3 writes, runs code on the server host, and is invisible to namespace administrators.
- **An outbox table written in the commit path.** A second durable write per ref update, which must agree with `refs.log` after a crash. `refs.log` is already the durable, ordered record of every change, so the feed and the dispatchers read it.
- **Polling refs with `git ls-remote` or `GET /refs`.** Works without F1 changes, but loses intermediate updates, costs a request per interval per repository, and reacts slowly.
- **Statuses stored in the repository** (as a ref of notes, or in `refs.log`). They would travel with the repository, but setting a status would be a commit, and CI tools would need write access. Statuses are about commits, not part of them.
- **Paths and messages in events.** Convenient for receivers that filter by path, but a receiver is a third party and a hook's events would expose them regardless of its ACLs. The receiver can read the diff with its own token.
- **Requiring `write` to set statuses**, as GitHub's `repo:status` scope effectively does with classic tokens. A CI bot that can only report results is safer, and the new permission is one value in the existing `perms` list.
