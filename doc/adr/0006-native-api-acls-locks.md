# ADR-0006: Native API, ACL evaluation and lock enforcement

- Status: accepted
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

Phase 3 implements the native API, identities, ACLs and locks. The plan fixes the outline: path ACLs with the longest prefix winning, D13, lock modes, and the endpoint list. It leaves the precise rules open; this ADR records them.

## Decision

### ACLs

1. **Prefix matching.** An entry applies to paths that start with its prefix. This is plain string prefix matching, as in S3 policies: `docs/` is a directory, while `docs` also matches `docs.txt`.
2. **Evaluation.**
   - For a path, the longest applying prefix decides. The caller's own entries and its groups' entries at that prefix are combined.
   - An entry with no permissions denies, overriding shorter prefixes.
   - `admin` implies `read`, `write` and `lock`. Default deny.
3. **Server administrators.** An entry `admin` on namespace `*` with prefix `""` makes a principal a server administrator, which is admin in every namespace. Creating namespaces needs it.
4. **D13 in practice.**
   - "Whole-namespace read" means read granted at prefix `""` and not narrowed at any longer prefix.
   - It is required by `objects/*`, `POST /commits`, ref updates, and `log` without `path`.
   - `diff` and listings are filtered to readable paths. A collapsed listing prefix is shown if anything below it is readable.
   - Tag updates and ref deletions change no file and need whole-namespace write. Branch updates need write on every changed file.
5. **Unknown and inaccessible namespaces look the same.** A caller with no permission in a namespace gets 404, so namespace names do not leak.
6. **ACL principals in the CLI** must be written in full (`user:<name>` or `group:<name>`). A bare name is ambiguous there.

### Locks

7. **What a change set is.** Lock checks run on every file a branch update changes: the diff from the old value. For a new branch, it is the diff from its merge base with the default ref (or the empty tree); the default ref's locks then apply as well. Deletions are changes.
8. **Rules.**
   - Under `exclusive` and `required`, a change to a path locked by another principal is rejected.
   - Under `required`, a change to a matching path also needs a lock held by the committer. A glob without `/` matches the file name in any directory, as svn:needs-lock patterns do; a glob with `/` matches the whole path.
   - The lock check and the CAS check run together under the ref lock.
9. **Group commit.** A batch may mix principals, so the group committer checks each request's paths with its own principal, and the batch commit skips the check.
10. **Lock management.** Locking a path the caller already holds refreshes its expiry. Only the owner unlocks; `force` needs `admin` on the path. Expired locks are ignored and dropped at the next lock write.

### Native API conventions

11. **Refs in URLs.** A ref is one URL path segment. Refs with `/` are percent-encoded as `%2F`; the Go client does it.
12. **`F1-Parent`** carries the expected commit for file writes, or `new` for a ref that must not exist yet. It is required unless the namespace has autocommit, where writes go through the group committer (428 `parent_required` otherwise).
13. **Errors** are JSON `{code, message, current?, owner?}`. CAS conflicts (409 `conflict`) carry the current value. Locked paths (423 `locked`) carry the owner, empty when a lock is required but not held. Frozen repositories return 423 `frozen`, and missing objects 409 `missing_objects`.
14. **Reads.** GET serves `Range`, `If-Match`, `If-None-Match` and HEAD through `http.ServeContent`, with ETag = the File hash. `F1-Commit`, `F1-File`, `F1-Kind` and `F1-Size` describe the result.
15. **Object uploads** are pack streams in dependency order (D9). Payloads are verified against their hashes, and the objects are durable when the response is sent.
16. **JSON types are shared.** The server and the public client package `pkg/f1client` use the same types.

### Identities

17. **Local principals** are `user:<name>` with issuer `local` and stored groups. Local tokens are `f1_` plus 32 random characters, stored as SHA-256, with optional expiry and revocation.
18. **S3 secrets are stored in plain text in `server.db`,** because SigV4 needs them. `server.db` must be protected accordingly (mode 0600, backups encrypted at rest).

## Consequences

- One ACL syntax and one set of rules serves every protocol. Phase 4 (S3) and phase 6 (Git) reuse `auth.Grants` and the `CheckPath` hook of `UpdateRefs`.
- String-prefix ACLs are predictable but need the trailing `/` for directories. The CLI does not add it.
- Path-limited readers cannot use hash-addressed endpoints (plan open question 4).

## Alternatives considered

- **Component-boundary prefixes** (`docs` matching `docs/...` but not `docs.txt`). This is safer for directories, but differs from S3 key prefixes, and S3 is the main user of path ACLs.
- **Per-batch lock checks with the batch author.** Wrong when a batch mixes principals.
- **Hashing S3 secrets.** Not possible with SigV4: the server must compute the same HMAC as the client.
