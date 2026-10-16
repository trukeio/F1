# ADR-0004: Repository portability and the admin socket

- Status: accepted
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

Phase 2b implements freezing, archiving, importing, attaching and detaching repositories, and the admin socket through which `f1d repo ...` drives a running server. The plan describes the procedures (`doc/plan.md`, "Repository directory"), but leaves their exact semantics open.

## Decision

### Freeze

1. **Freezing a repository:**
   - New writes fail at once with `ErrFrozen` (commits, ref updates, group commits, and uploads that have not started).
   - Writes already in progress, including long uploads, are waited for.
   - Then the open pack is sealed, `cache.db` is checkpointed so that it is a single consistent file, and a store hold stops consolidation and GC.
   - Freezing a frozen repository is a no-op.
   - The frozen state is not persisted: a restarted server starts unfrozen. Closing a repository unfreezes it.

### Archive

2. **What an archive captures.**
   - The archive holds packs from deletion for its whole duration.
   - It reads `f1repo.toml`, `refs`, `refs.log` and `locks` while holding the ref lock, so `refs` and `refs.log` agree. Then it lists the live packs (indexed in `cache.db`, not pending deletion).
   - Writes continue throughout; packs sealed after the listing are not included.
   - The imported archive has exactly the refs read at the start.
3. **ZIP layout.**
   - All entries are below `<namespace>/`. Text files are deflated; `packs/*.pack` are stored.
   - ZIP64 is used automatically above 4 GiB, as Go's `archive/zip` does.
   - The archive contains no `cache.db`, `lock` or `tmp/`.

### Import

4. **Import takes only repository state.** That is `f1repo.toml`, `refs`, `refs.log`, `locks` and `packs/*.pack`. Everything else in a ZIP or a copied directory (`cache.db*`, `lock`, `tmp/`, temporary files) is skipped and reported.
   - Imports reject archives with more than one root directory, absolute paths, `..`, symbolic links, or no `f1repo.toml`/`refs`.
   - `f1repo.toml` is parsed before anything is extracted, so an unsupported format is refused with a message naming the required version.
5. **Import goes through a staging directory.**
   - The import is extracted or copied into `repos/.import-<random>/`, which the startup scan ignores.
   - Then `--new-uuid` is applied if given, the repository is opened once (`cache.db` rebuilt, `refs.log` recovery applied), and `fsck` runs.
   - Only if `fsck` finds no errors is the directory renamed to `repos/<name>`.
   - The name defaults to the archive root or source directory name, and an existing name is refused.
   - The staging directory is removed in every case.

### Namespaces and registration

6. **Namespace names** are 1–63 characters of lowercase letters, digits, `.`, `_` and `-`. They start and end with a letter or digit and contain no `..`. They double as S3 bucket names and directory names.
7. **Startup.**
   - At startup the server registers every valid repository directory in `repos/` that is not registered.
   - It logs registered repositories that have no directory, and repositories that share a UUID.
   - A repository that fails to open is logged and left closed; the server still starts.

### Admin socket

8. **The admin socket** is HTTP with JSON bodies on `<data_dir>/admin.sock`, mode 0600. It is authenticated by file permissions only. A socket address holds about 104 bytes, so when the path is longer, the server and the admin commands bind and connect through a symlink to the data directory in a new private temporary directory, removed right after. The socket itself stays in the data directory.
   - Endpoints:
     - `GET /repos`
     - `POST /repos/{ns}/freeze|unfreeze|attach|detach`
     - `POST /repos/{ns}/archive` with `{"path": "<absolute path>"}`; the server writes the file atomically.
   - The socket also marks the data directory as owned. A server that finds a live socket refuses to start; a stale socket from a crash is removed.
9. **Online and offline commands.** With a running server, `f1d repo` commands go through the socket.

   | Command | Without a running server |
   |---|---|
   | `freeze`, `unfreeze` | Error: the repositories of a stopped server can be copied directly |
   | `archive` | Opens the repository directly |
   | `attach`, `detach` | Edit `server.db` |
   | `import` | Stages and registers; with a server running, it asks the server to attach |
   | `rebuild-cache`, `fsck` | Offline only: they need the repository lock and refuse while a server has the repository open |

### Scope change

10. **ACL export and import move to phase 3.** ACLs and principals do not exist before phase 3, so `export-acl`/`import-acl` are built together with them.

## Consequences

- Copying a frozen repository with any tool is safe, including `cache.db`. Copies of a running, unfrozen repository may get an inconsistent `cache.db`. Import rebuilds it, and opening a copied repository directory repairs it.
- A freeze can take as long as the longest upload in progress. An operator who needs an immediate copy uses `archive`, which never blocks writes.
- One broken repository cannot stop a server, but it is unavailable until repaired; the log says why.

## Alternatives considered

- **Aborting in-flight uploads on freeze.** Freezing would be faster, but clients would see failures for writes they started before the freeze. Archive covers the "copy now" case without that cost.
- **Persisting the frozen state.** A crash during a manual copy would then leave the repository frozen after restart, which is surprising and gains nothing, since a stopped server already makes copying safe.
- **A custom line protocol on the admin socket.** HTTP keeps the server and client simple and reuses the standard library.
