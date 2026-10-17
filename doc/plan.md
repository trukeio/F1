# F1 implementation plan

F1 is a single storage system that replaces S3-style object storage, SVN-style repositories with large files, and Git repositories. It has one storage model and three access protocols: a native HTTP API, an S3-compatible API, and the Git smart-HTTP protocol (with Git LFS).

This document is the specification and work plan for implementing F1 in Go. It is written for Claude Code.

- Repository: `github.com/trukeio/F1`. Go module path: `github.com/trukeio/f1` (see D15).
- License: MIT.
- Version 1 deployment target: one server with local disk.

## How to use this document

- Implement the phases in order. Each phase ends with its acceptance criteria passing in CI.
- The decisions in "Fixed decisions" are settled. Do not change them without writing an ADR in `doc/adr/` and asking the user.
- The on-disk format constants (chunking parameters, hash, object encoding, object types and fields, prolly tree boundary rules, pack format) are permanent once phase 1 is merged. Changing them breaks deduplication and object identity.
- Record any decision this document leaves open as an ADR before implementing it.
- Add a dependency only if it is in "Dependencies" or the user approves it.

## Goals

1. One content-addressed storage model with deduplication across files and versions within a repository.
1a. Each repository (namespace) is a self-contained subdirectory. Copying that directory, or a ZIP of it, to another F1 server gives a working repository with full history.
2. Per-namespace policies that cover the three use cases:
   - S3 style: no or limited history, last-writer-wins.
   - SVN style: full history, large binaries, locks, sparse checkout.
   - Git style: full history, branches, client-side merge.
3. A small native API.
4. S3 compatibility good enough for the AWS CLI, AWS SDKs (Go v2, Python boto3), rclone and MinIO `mc`.
5. Git compatibility: `git clone`, `fetch`, `push` and partial clone work against F1. Commit, tag and tree SHAs round-trip exactly, including non-canonical trees (see "Object mapping"). Objects that `git fsck` reports as errors (not warnings), such as trees with duplicate entries, may be rejected on push.
6. Git LFS compatibility, including the LFS locking API.
7. Path-based ACLs and file locks.

## Non-goals (version 1)

- Multi-node server clustering and Postgres. Version 1 runs on one node with SQLite.
- Hosting F1 on Windows. The server and the admin CLI run on Unix systems (Linux, macOS, BSD); the repository lock uses `flock`. Clients (Git, S3 tools, the native client) may run anywhere.
- S3 or other remote storage as the blob backend. Version 1 stores packs on local disk. The `Backend` interface stays, so a remote backend can be added later.
- OIDC login. Planned for a later version (see "Identity and authorization").
- SVN wire protocol. SVN users migrate to the native client, Git, or S3. Evaluated and deferred indefinitely (ADR-0017).
- FUSE or virtual filesystem client.
- Web UI in the server. A separate viewer, `f1web`, is a client of the native API (ADR-0018).
- Encryption beyond what the storage backend provides.
- Git SHA-256 repositories.
- S3 features: bucket policies, ACLs, lifecycle rules, object lock, SSE, tagging, website hosting, select, replication.

## Known limitations (version 1)

- Storage efficiency for source code. The minimum chunk (16 KiB) is larger than most source files, and F1 stores no deltas. Every changed version of a small file is stored whole (zstd-compressed). A code repository with long history takes 3–6 times the space of a Git delta pack (measured in ADR-0013). ADR-0013 proposes delta-encoded pack records, made during GC repacking, for a later version; it is a format addition awaiting a decision.
- Git fetch deltifies trees and blobs of at most 1 MiB against the previous version of the same path (ADR-0012), but sends no thin packs against the client's objects, so incremental fetches can transfer more than from a Git server.
- Write durability costs about six fsyncs per commit (pack, pack directory, `cache.db`, `refs.log`, `refs`, repository directory). Throughput targets assume NVMe or SSDs with power-loss protection; consumer SSDs with slow fsync will be lower.

## Fixed decisions

| # | Decision | Value |
|---|---|---|
| D1 | Object hash | BLAKE3, 32 bytes |
| D2 | Chunking | FastCDC, min 16 KiB, avg 64 KiB, max 256 KiB, fixed gear table committed in source |
| D3 | Chunk hash input | Uncompressed chunk bytes |
| D4 | Chunk compression | zstd, per chunk, stored only if it saves at least 5 % |
| D5 | Object encoding | Canonical CBOR (deterministic encoding, RFC 8949 §4.2), with a 1-byte type tag. Object hash = BLAKE3(type tag ‖ CBOR bytes) |
| D6 | Physical storage | Each repository has its own directory with its own immutable pack files (target 64 MiB). No sharing of packs or objects between repositories. Version 1 implements only the local filesystem backend |
| D7 | Metadata | Two levels. Per repository: a ref file (source of truth) and a SQLite cache (`cache.db`) that can be deleted and rebuilt from the packs. Server level: one SQLite database for identity, tokens, S3 keys, ACLs and the repository registry. Pure Go SQLite (`modernc.org/sqlite`) |
| D8 | Concurrency primitive | Compare-and-swap on refs, serialized per repository in the server process. All other repository data is immutable and append-only |
| D9 | Storage invariant | An object is stored only if all objects it references are already stored. Single exception: the parent links of the oldest retained commits in a namespace with truncating retention (see "Retention"); `fsck` accepts those and nothing else |
| D10 | Merge | Client-side only. The server never merges file content. A write may opt in to being rebased by the server when no path it changes was changed since its parent (ADR-0014) |
| D11 | Git protocol | Smart HTTP. Protocol v2 for fetch. v0/v1 for push |
| D12 | Git namespaces | Must use `layout = tree` and `retention = all` |
| D13 | Path ACLs | Every hash-addressed access requires read permission on the whole namespace: Git, LFS, the native object endpoints (`/ns/{ns}/objects/...`) and `POST /ns/{ns}/commits`, and therefore the native client. Path-level read ACLs apply only to path-addressed native endpoints (`files`, `tree`, `log`, `diff`) and the S3 API; `log` and `diff` results are filtered to readable paths. Write ACLs apply to all protocols |
| D14 | Server config format | TOML |
| D15 | Module path | `github.com/trukeio/f1` (lowercase). The GitHub repository may be named `F1`; GitHub resolves names case-insensitively. Uppercase in a module path forces case-escaping in the module proxy (`!f1`) and exact-case imports, so it is avoided |
| D16 | License | MIT. `LICENSE` file at the root; no per-file headers |
| D17 | Authentication | Pluggable token verifiers behind one interface: local tokens (version 1) and OIDC bearer tokens from the enterprise identity provider, verified against its JWKS (version 1). F1 trusts the same provider as G1, not G1 itself, so both accept the same users and groups (ADR-0005). OIDC login in F1's own clients later reuses the same verifier |
| D18 | Repository portability | A repository directory contains everything needed to serve the repository except identities and ACLs. A copy taken by the documented procedure (see "Repository directory") is always consistent. Deduplication between repositories is given up for this |
| D19 | Size transparency | No API, protocol, command or user-visible behavior depends on file size. A 1-byte file and a 100 GiB file are written, read, versioned, locked, merged and exported the same way. Size affects only the internal chunk-list layout (direct list or FileIndex). Every read and write path streams; server and client memory use is bounded independently of file size |
| D20 | Prolly tree boundaries | Name-only boundary hash, average 64 entries per node, min 16, max 512 entries, max 128 KiB encoded node size; same rule for interior nodes with the level mixed into the hash (see "Objects") |
| D21 | Durable external identities | Git SHA-1, SHA-256 and MD5 of each File are stored durably in packs as DigestRecord objects, written at ingest. `cache.db` only indexes them. Git SHA-1s of trees, commits and tags are recomputable from metadata alone, never by re-reading file content |

## Repository directory

### Layout

```
<data_dir>/
  server.db                 identities, tokens, S3 keys, ACLs, repository registry
  repos/
    <namespace>/
      f1repo.toml           namespace config, format version, repository UUID
      refs                  current refs (source of truth)
      refs.log              append-only log of every ref update (reflog)
      locks                 current locks
      lock                  empty file; flock held by whoever has the repository open
      packs/
        <ulid>.pack         immutable pack files
      cache.db              rebuildable cache: object index, digests, git map, commit graph, upload state
      tmp/                  in-progress uploads and unsealed packs; never copied
```

### Files

- `f1repo.toml`: the namespace configuration from "Namespace", plus `format = 1` and `uuid`. Human-editable while the repository is detached.
- `refs`: one line per ref, `<hash-hex> <refname>`, sorted. Replaced atomically on each update: write `refs.tmp`, fsync, rename, fsync directory.
- `refs.log`: one line per ref update, `<time> <tx> <i>/<n> <principal> <refname> <old> <new> <reason>`; a multi-ref update is one group of `n` lines sharing `tx`, written in one write. Appended and fsynced before `refs` is replaced. Used for audit and for recovery after a crash between the two steps: a torn trailing group is removed, and a complete last group not yet in `refs` is applied (ADR-0003).
- `locks`: same atomic-replace scheme as `refs`. Text, one lock per line. Paths are stored as bytes, so in `locks` and `refs.log` every byte outside printable ASCII, and every space, `%` and newline, is percent-encoded (`%XX`).
- `lock`: an empty file. The server takes an exclusive `flock` on it when it opens the repository and holds it until detach or shutdown. Offline commands (`fsck --dir`, `rebuild-cache`, offline `archive`) take the same lock and refuse to run if it is held. Not part of the repository state; excluded from archives.
- `packs/`: as in "Pack store". A pack is complete and readable on its own (footer index plus checksum).
- `cache.db`: object index (hash → pack, offset), digest index, Git map, commit graph (ADR-0015), multipart upload state, GC state. Everything in it is either derivable from `packs/` and `refs` or disposable. If it is missing, corrupt, or its recorded pack list differs from `packs/`, the server rebuilds it when it opens the repository. Index rebuild reads pack footers, then the payloads of DigestRecord objects (small; located through the footer's type field). The Git map for trees, commits and tags is recomputed in the background from metadata and DigestRecords, without reading file content (D21).
- `tmp/`: in-progress multipart uploads and unsealed pack buffers. Not part of the repository state. Deleted on detach.

Identities and ACLs stay in `server.db`, because they refer to principals that exist across repositories. `f1d repo export-acl <ns>` and `f1d repo import-acl <ns> <file>` move ACLs as a TOML file when needed. Principals unknown on the target server are reported and skipped.

### Write ordering

The ordering rules that make plain file copies safe:

1. An object is written to a pack, the pack is sealed, fsynced and indexed before any ref can point to it (or to anything that references it).
2. `refs.log` is appended, then `refs` is replaced.
3. GC never deletes a pack while a hold is active on the repository (see below), and deletes packs only after the rewritten objects are in new sealed packs.

Consequence: any copy that takes `refs` first and then `packs/` contains every object reachable from the copied refs, provided GC does not delete packs during the copy.

### Copy and archive procedures

| Procedure | Server state | Writes during copy |
|---|---|---|
| Manual copy, offline | Server stopped | n/a |
| Manual copy, online | `f1d repo freeze <ns>`: repository read-only, GC held, packs flushed. Then copy the directory with any tool (cp, rsync, zip). Then `f1d repo unfreeze <ns>` | Rejected with a clear error (S3 `503 SlowDown` with message, Git push error, native `423`) |
| Online archive | `f1d repo archive <ns> <file.zip>` | Allowed. The command holds GC, reads `refs`, `refs.log`, `locks` and `f1repo.toml`, lists `packs/`, writes the ZIP, then releases the hold. Packs sealed after the listing are not included and are not needed |
| Native API | `GET /api/v1/ns/{ns}/archive` (admin), streams the same ZIP | Allowed |

ZIP details: ZIP64 enabled (Go `archive/zip` handles it), pack files stored without compression (they are already zstd), text files deflated, `cache.db`, `lock` and `tmp/` excluded. The archive root is the repository directory name.

### Restore and attach

- `f1d repo import <file.zip|dir> [--name <ns>]`: copies or extracts into `repos/<ns>/`, registers the namespace, rebuilds `cache.db`, runs `fsck`. Refuses if the name exists.
- A directory placed into `repos/` while the server is stopped is detected at start and registered (log message). While running, `f1d repo attach <ns>` registers it.
- `f1d repo detach <ns>`: unregisters the repository, flushes, deletes `tmp/`, leaves the directory in a consistent state for moving.
- Duplicate UUIDs (a repository copied under a second name on the same server) are allowed but logged. `f1d repo import --new-uuid` assigns a new one.
- `format` higher than the server supports: refuse to open, with a message naming the required version.

## Architecture overview

```
           native HTTP API     S3 API      Git smart HTTP + LFS
                  \               |               /
                   \              |              /
     server.db ---- auth, ACL, repository registry
                                  |
                    +------- repo (core) -------+
                    |  read path, write txn,     |
                    |  commit, diff, locks       |
                    +-------------+--------------+
                                  |   one instance per repository directory
                 +----------------+----------------+
                 |                |                |
          object layer       refs, refs.log,     cache.db
   (File, Tree, Commit, Tag)    locks          (index, digests,
                 |                              git map, uploads)
            pack store
                 |
            packs/ (local fs)
```

## Data model

### Namespace

A namespace is a repository. It has a name, a configuration and a set of refs.

Namespace configuration:

```toml
[namespace.design-data]
layout     = "tree"        # "tree" (hierarchical paths) or "flat" (S3 keys)
retention  = "all"         # "all" | "last:N" (commits) | "days:N" | "none"
autocommit = false         # true: every Put creates a commit (group-committed)
branches   = true
locks      = "exclusive"   # "off" | "exclusive" | "required:<glob>,<glob>"
git        = true          # expose via Git protocol (requires layout=tree, retention=all)
default_ref = "main"
```

Presets:
- S3 style: `layout=flat, retention=none, autocommit=true, branches=false, git=false`.
- SVN style: `layout=tree, retention=all, locks=required:*.sch,*.brd,*.docx, git=false`.
- Git style: `layout=tree, retention=all, git=true`.

Git export of existing objects depends only on stored data, never on current configuration.

### Layout: tree versus flat

- `tree`: paths are hierarchical. `a/b` requires `a` to be a directory. Same rules as Git. Paths must not be empty, start or end with `/`, contain `//`, or have `.` or `..` components; such keys are rejected by every protocol (S3: `400 InvalidArgument` with a message suggesting `layout = flat`). This includes S3 "folder marker" keys ending in `/`.
- `flat`: keys are opaque strings (S3 semantics). `a` and `a/b` may both exist. Stored as one sorted map keyed by the full key.
- Listing order is byte order of the full path in both layouts. In `tree` layout, iteration that recurses into directories merges children by comparing a directory as `name/` (the Git sort rule), so `a.txt` (0x2E) is listed before `a/b` (0x2F).

Both layouts use the same Tree object. In `flat` layout the root tree contains only file entries, whose names may contain `/`.

### Objects

All objects are immutable, CBOR-encoded, and addressed by hash.

**Chunk** (type 0x01): raw bytes. Not CBOR.

**File** (type 0x02): `[size, level, refs]` with `refs = [ [hash, size] ... ]`, at most 4096 refs. Level 0: refs are chunks (size = uncompressed chunk length). Level n > 0: refs are FileIndex objects of level n − 1 (size = bytes covered).

**FileIndex** (type 0x03): `[level, refs]`, at most 4096 refs, same level meaning. A file with more than 4096 chunks groups its refs into FileIndex objects at content-defined boundaries (ADR-0002), so an edit rewrites only the FileIndex objects around it. Up to 4 levels.

Sizes are part of the format so that a read at any byte offset (HTTP `Range`, `Seek`, Git delta base reads) locates its chunk by prefix sums over the chunk list, without fetching or decompressing earlier chunks.

The File hash depends on the chunk list, not on the content alone. The same content uploaded with different chunk boundaries (for example S3 multipart) can yield a different File hash. This only causes false "changed" signals, never false "unchanged" ones. Content digests that external protocols need are stored separately (see "Digests").

**Tree** (type 0x04): a prolly tree (content-defined B-tree), so that large directories and flat namespaces with millions of keys can be updated in O(log n).

Leaf node:
```
entries: [ {name: bytes, kind: uint8, target: hash, attrs: map?} ... ]  # sorted by name, byte order
```
Interior node:
```
children: [ {first_name: bytes, node: hash, count: uint64} ... ]
```
Node boundaries are content-defined (D20). Nodes are built left to right at each level:
- Leaf level: split after an entry when the node has at least 16 entries and `BLAKE3(entry_name)` has its low 6 bits equal to zero (average 64 entries). Force a split when the node reaches 512 entries or 128 KiB of encoded entries.
- Interior levels: the same rule applied to children, with boundary hash `BLAKE3(uint8(level) ‖ first_name)`, level 1 being the lowest interior level.
- A level with fewer than 16 entries is a single node, which is then the root.

The boundary hash uses only the name, never the target or attrs, so changing a file's content or metadata never moves node boundaries. Forced splits are rare (probability about e^-7.7 per node) and keep nodes bounded when entries carry large attrs. The target node size is a few KiB to a few tens of KiB, so a single-key update writes O(log n) small nodes.

Entry kinds:

| Kind | Meaning | Git mode |
|---|---|---|
| 1 file | regular file | 100644 |
| 2 exec | executable file | 100755 |
| 3 symlink | target stored as File | 120000 |
| 4 dir | subtree (tree layout only) | 040000 |
| 5 gitlink | submodule; `attrs.git_sha1` holds the commit | 160000 |
| 6 lfs | content stored as File; `attrs.lfs_pointer` holds exact pointer bytes | 100644 or 100755 (`attrs.exec`) with blob = pointer bytes |

Entry `attrs` (optional, CBOR map with string keys), used by the S3 API:
- `ct`: content type
- `meta`: map of user metadata (`x-amz-meta-*`)
- `etag`: S3 ETag string
- `mtime`: write time (Unix ns)
- `cks`: S3 checksum values sent by the client (`crc32`, `crc32c`, `sha256`, ...)

Entry `attrs` used by the Git mapping (these do affect Git export):
- `exec`: executable flag for `lfs` entries
- `git_sha1`: submodule commit for `gitlink` entries
- `lfs_pointer`: exact pointer bytes for `lfs` entries
- `git_raw`: hash of a GitRaw object holding the exact Git tree bytes for a `dir` entry whose Git tree is not canonical (see "Object mapping")

The S3 attrs (`ct`, `meta`, `etag`, `mtime`, `cks`) do not affect Git export. Two F1 trees that differ only in S3 attrs map to the same Git tree.

Hashes inside attrs (`git_raw`) are references for D9 and GC like any other.

**Commit** (type 0x05):
```
tree:      hash
parents:   [hash...]
author:    {name: bytes, email: bytes, time: int64, tz: string}   # tz as "+0200"
committer: {name, email, time, tz}
message:   bytes                     # raw, not normalized
git_extra: [ {key: bytes, value: bytes} ... ]   # extra Git headers in original order (gpgsig, mergetag, encoding, ...)
git_raw_hint: bytes?                 # only if exact re-encoding is otherwise impossible (see Git mapping)
git_tree_raw: hash?                  # GitRaw object for the root tree, only if the root Git tree is not canonical
```

**Tag** (type 0x06): annotated tag, same field approach as Commit (object, type, tag name, tagger, message, git_extra).

**DigestRecord** (type 0x07): durable external digests of one File (D21).
```
file:    hash
sha1git: bytes(20)    # Git blob SHA-1
sha256:  bytes(32)
md5:     bytes(16)
```
Written in the same pack as, or after, the File it describes. Not referenced by any other object. Several records for one File are harmless (identical values). GC keeps a record exactly when its File is live.

**GitRaw** (type 0x08): the exact bytes of a Git tree object that F1 cannot re-encode canonically (non-standard modes such as `100664`, zero-padded modes, non-standard entry order). Referenced from `attrs.git_raw` of a `dir` entry or from `Commit.git_tree_raw`.

### Refs

Stored in the repository's `refs` file (see "Repository directory"). Names follow Git rules (`heads/main`, `tags/v1`). The native API accepts short names (`main` → `heads/main`).

Update is compare-and-swap on the old hash, serialized by a per-repository mutex in the server. A multi-ref update (Git `atomic` push) checks all old values, then writes one `refs.log` record group and one new `refs` file. A ref update is the only way a commit becomes visible.

### Digests

The source of truth for a File's digests is its DigestRecord in the packs (D21). Table `digests(file_hash, sha1git, sha256, md5)` in `cache.db` indexes them, with secondary indexes on `sha1git` and `sha256` for Git and LFS lookups.
- Single-stream ingest (native Put, S3 PutObject, LFS upload, Git push) computes the digests inline and writes the DigestRecord together with the File, before the write is acknowledged.
- Files assembled from parts (S3 multipart) get their DigestRecord from a background job that reads the content once. Until it exists, a request that needs the digest computes it on demand and writes the record.
- On a `cache.db` rebuild, the table is refilled from DigestRecords. File content is never re-read for this.

`sha1git` is the Git blob SHA-1 (`"blob <size>\0" + content`). `sha256` is used by LFS. `md5` is used for S3 ETags of single-part uploads.

### Locks

Stored in the repository's `locks` file, one line per lock: id, ref, path, owner, created, expires. The owner is a principal name (`user:<name>`).
- Locks are on exact paths (not prefixes).
- `locks = required:<globs>`: commits that modify matching paths are rejected unless the committer holds the lock.
- `locks = exclusive`: taking a lock is optional, but a held lock is enforced: commits by other principals that modify a locked path are rejected.
- `locks = required:<globs>` also enforces held locks on paths outside the globs, as `exclusive` does.
- Enforcement happens in the core commit path, so it applies to all protocols. A glob without `/` matches the file name in any directory; group commits check each request with its own principal (ADR-0006).
- The checked change set is the diff between the old and new value of the ref. For a ref being created, it is the diff between the new commit and its merge base with `default_ref`, or against the empty tree if there is none.

### ACLs

Table `acl(namespace, principal, path_prefix, perms)` in `server.db`, where `principal` is `user:<name>` or `group:<name>` (the same syntax as G1), with perms from `read`, `write`, `lock`, `admin`. The longest matching prefix wins. Default deny. Precise rules (string prefixes, combining a user's and its groups' entries, empty perms deny, `admin` implies all, server administrators via namespace `*`) are in ADR-0006.

Hash-addressed access bypasses paths, so it requires read on the whole namespace (D13). Otherwise a principal with partial read could fetch any object by hash, probe for content through `objects/missing`, or commit a tree that references an object it cannot read and then read it through its own path.

## Storage layer

### Blob backend interface

```go
type Backend interface {
    Put(ctx context.Context, key string, r io.Reader, size int64) error
    Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string, fn func(key string, size int64) error) error
}
```

Implementation in version 1: `fs` (local directory, atomic rename on Put, `fsync` of file and directory before acknowledging). The interface is kept narrow so an S3 backend can be added later without changes elsewhere.

### Pack store

- One pack store per repository. Writes go into an open pack buffer in `tmp/`. A pack is sealed at 64 MiB or after 2 s idle, fsynced, renamed to `packs/<ulid>.pack`, and its index is recorded in `cache.db`.
- Pack file format: header (magic `F1PK`, version), records (`type, hash, flags, length, payload`), footer index (sorted entries of hash, type, offset, stored length, uncompressed length), footer trailer (index offset, BLAKE3 of the whole pack).
- Table `objects(hash, pack_id, offset, length, size, type)` in `cache.db`. Rebuildable from pack footers.
- When one upload produces more than 64 MiB of new chunks, its chunks fill packs sequentially and are sealed as they fill. This is the same pack mechanism for every file, not a separate large-file path.
- Reads: look up `objects`, then a ranged Get on the pack. LRU cache of decoded chunks and objects (size configurable).
- A Put is acknowledged only after its pack is durable and indexed. The write path must not acknowledge objects held in an unsealed buffer. Sealing on request (flush) is allowed.
- Because every acknowledged write seals its pack, most packs are small (one request or one group commit). Pack consolidation keeps their number bounded: when a repository has more than 256 sealed packs smaller than 8 MiB, a background job copies all their objects, without any liveness check, into packs of up to 64 MiB, updates `objects` rows atomically, and deletes the old packs after the read grace period (see "GC algorithm"). Consolidation respects holds and runs under the same coordination as GC sweeps.

### Ingest pipeline

Ingest runs chunking, BLAKE3, zstd, and the digest hashes (MD5, SHA-1, SHA-256) concurrently: chunking feeds per-chunk BLAKE3 and compression workers, and the digest hashes run on their own goroutines over the same byte stream. Run serially on one core, these steps reach only about 150 MB/s, so the pipeline is required to meet the phase 1 throughput target. Memory stays bounded by a fixed number of in-flight chunks.

### Write path invariant (D9)

`PutObject(obj)` checks that every hash `obj` references exists in `objects` (or in the same pending batch). Every existence check (D9 checks, dedup hits during ingest, `objects/missing` answers) goes through one core function that cooperates with GC (see "GC algorithm"): an object condemned by a running sweep is reported as not stored, so the writer stores a fresh copy. Combined with the grace period, this guarantees that no stored object has dangling references.

## Core (`internal/repo`)

Operations:

```go
type Repo interface {
    // Read
    ResolveRef(ctx, ns, ref string) (Hash, error)
    Stat(ctx, ns string, commit Hash, path string) (Entry, error)
    Open(ctx, ns string, commit Hash, path string) (io.ReadSeekCloser, error) // supports ranges
    List(ctx, ns string, commit Hash, prefix string, opts ListOpts) (Iterator, error)   // delimiter, start-after, limit
    Log(ctx, ns, ref string, path string, limit int) ([]CommitInfo, error)
    Diff(ctx, ns string, a, b Hash, prefix string) (Iterator, error)                   // O(changed) via tree hashes
    MergeBase(ctx, ns string, a, b Hash) (Hash, error)

    // Write
    Ingest(ctx, r io.Reader) (FileRef, Digests, error)          // chunk, hash, store
    Commit(ctx, ns, ref string, expectParent Hash, changes []Change, meta CommitMeta) (Hash, error)
    UpdateRef(ctx, ns, ref string, old, new Hash) error           // CAS, checks locks + ACL on the diff

    // Coordination
    Lock(ctx, ns, ref, path string, ttl time.Duration) (Lock, error)
    Unlock(ctx, id LockID, force bool) error
    Locks(ctx, ns, ref, prefix string) ([]Lock, error)
}
```

`Change` = `{path, op: put|delete|rename, entry}`. `Commit` applies changes to the parent tree (prolly-tree update), writes new nodes, writes the commit, and calls `UpdateRef`.

### Group commit for autocommit namespaces

For `autocommit = true`, concurrent Puts to the same ref are batched: a committer goroutine per (namespace, ref) collects the queued changes and those arriving less than 1 ms apart, for up to 20 ms or 1000 changes, creates one commit, and completes all waiting Puts (ADR-0012). Conditional Puts (`If-Match`, `If-None-Match`) are evaluated in order against the tree state inside the batch.

Target: ≥ 500 small Puts per second per namespace on a single node with SSD.

## Native API (`/api/v1`)

JSON for metadata, raw bytes for content. Authentication: bearer token. Refs are one URL segment (`%2F` for `/`); `F1-Parent` takes a commit hash or `new`; `F1-Rebase: true` (or `"rebase": true` in `POST /commits`) applies a write on a moved branch head if none of its paths changed (ADR-0014); errors are JSON `{code, message, current?, owner?, paths?}`; conventions in ADR-0006. Reads by commit hash and object reads are marked immutable for HTTP caches (ADR-0016). The JSON types are those of `pkg/f1client`.

| Method and path | Purpose |
|---|---|
| `GET /ns` | List namespaces |
| `POST /ns` | Create namespace (admin) |
| `GET /ns/{ns}/refs` | List refs |
| `PUT /ns/{ns}/refs/{ref}` | CAS update `{old, new}` |
| `GET /ns/{ns}/files/{ref}/{path}` | Read file. `ref` may be a ref name or commit hash. Supports `Range` |
| `HEAD /ns/{ns}/files/{ref}/{path}` | Stat |
| `PUT /ns/{ns}/files/{ref}/{path}` | Write one file and commit. Header `F1-Parent` (expected commit) is required unless autocommit; `F1-Rebase: true` allows a moved head (ADR-0014) |
| `DELETE /ns/{ns}/files/{ref}/{path}` | Delete one file and commit |
| `GET /ns/{ns}/tree/{ref}/{prefix}?delim=/&after=&limit=` | List |
| `GET /ns/{ns}/log/{ref}?path=&limit=` | History. Without `path`, requires read on the whole namespace; with `path`, read on that path |
| `GET /ns/{ns}/diff/{a}..{b}?prefix=` | Diff, filtered to paths the caller can read |
| `POST /ns/{ns}/objects/missing` | Body: list of hashes. Returns the subset not stored. Whole-namespace read |
| `POST /ns/{ns}/objects` | Upload a batch of objects (pack stream). Whole-namespace read and write |
| `GET /ns/{ns}/objects/{hash}` | Download one object. Whole-namespace read |
| `POST /ns/{ns}/objects/fetch` | Body: list of hashes. Returns pack stream. Whole-namespace read |
| `POST /ns/{ns}/commits` | Create commit from `{ref, parent, changes, message, rebase?}` with already uploaded objects. Whole-namespace read, write on the changed paths. If a referenced object is no longer stored (D9), returns `409` with the list of missing hashes |
| `POST /ns/{ns}/locks`, `GET`, `DELETE /ns/{ns}/locks/{id}` | Locks (`?force=true` needs admin on the path) |
| `GET /ns/{ns}/archive` | Stream the repository as a ZIP (admin) |
| `GET /me`, `POST /me/s3keys`, `GET /me/s3keys`, `DELETE /me/s3keys/{key}` | The caller, and its S3 access keys |

Objects are per repository (D6), so every object endpoint is under `/ns/{ns}`. The pack stream format used by the object endpoints is the pack file format without the requirement of being 64 MiB.

Provide a public Go SDK in `pkg/f1client`.

## S3 compatibility API

Served on its own listener (`[server.s3] listen`, default `127.0.0.1:9000`). Path-style and virtual-host-style addressing (`server.s3.domain` configures the latter). Precise semantics (authentication details, bucket creation and deletion, metadata, conditional requests, checksums, versioning rules) are in ADR-0007.

```toml
[server.s3]
listen        = "127.0.0.1:9000"   # "" disables the S3 API
region        = "us-east-1"
# domain      = "s3.example.com"   # virtual-host addressing
create_bucket = true               # server administrators may create and delete buckets
upload_expiry = "168h"             # open multipart uploads are aborted after this

[[server.s3.alias]]                # expose a branch as a bucket
bucket    = "design-dev"
namespace = "design"
ref       = "heads/dev"
```

### Mapping

- Bucket = namespace with `layout = flat` or `layout = tree`. Reads and writes go to the namespace's `default_ref`.
- Bucket aliases in config may map a bucket name to `(namespace, ref)`, for example to expose a branch.
- Key = path. In `tree` layout, writes that conflict with the hierarchy (`a` exists as file and `a/b` is written) return `409 InvalidRequest` with a clear message. Keys that `tree` layout cannot represent (see "Layout: tree versus flat"), including folder markers ending in `/`, return `400 InvalidArgument`.
- Listings are in byte order of the full key in both layouts (see "Layout: tree versus flat"). Tools such as rclone and `aws s3 sync` depend on this.
- Writes to non-autocommit namespaces create one commit per request (message `s3: PutObject <key>`), with the authenticated principal as author.
- `LastModified` = entry `attrs.mtime`, else the commit time of the commit that contains the entry version (computed via `Log`, cached).

### Operations

| Group | Operations | Phase |
|---|---|---|
| Service | ListBuckets | 4a |
| Bucket | HeadBucket, CreateBucket (server administrators; creates a namespace with the S3 preset; can be disabled), DeleteBucket (server administrators; empty only; the directory is moved aside, not erased), GetBucketLocation | 4a |
| Object | GetObject (Range, If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since), HeadObject, PutObject (incl. conditional `If-None-Match: *` and `If-Match`), DeleteObject, DeleteObjects, CopyObject (same or other bucket; metadata COPY/REPLACE) | 4a |
| Listing | ListObjectsV2 (prefix, delimiter, continuation-token, start-after, max-keys, fetch-owner), ListObjects (v1) | 4a |
| Multipart | CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads, UploadPartCopy | 4b |
| Versioning | GetBucketVersioning (Enabled when retention ≠ none, else Suspended/absent), ListObjectVersions, GetObject/HeadObject/DeleteObject with `versionId` | 4c |
| Everything else | Return `501 NotImplemented` with S3 error XML | 4a |

Versioning mapping: `versionId` = base32 of the commit hash in which that object version was written. A DeleteObject on a versioned bucket creates a commit that removes the entry, reported as a delete marker in ListObjectVersions. History is immutable: deleting a specific version is possible only for the newest version of a key (which restores the one before it, as in S3); older versions expire with the retention policy (ADR-0007).

### Authentication and integrity

- AWS Signature Version 4: `Authorization` header and presigned query parameters.
- Payload modes: `UNSIGNED-PAYLOAD`, signed payload hash, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (signed aws-chunked), `STREAMING-UNSIGNED-PAYLOAD-TRAILER` and `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` (aws-chunked with trailing checksum). Current AWS SDKs use the trailer modes by default.
- Checksums: validate `Content-MD5`, `x-amz-checksum-crc32`, `-crc32c`, `-crc64nvme`, `-sha1`, `-sha256` when sent. Store the values in `attrs.cks` and return them when `x-amz-checksum-mode: ENABLED`.
- Access keys are issued per principal and map to the same identity and ACLs as native tokens.

### ETags

- Single-part: hex MD5 of the content in quotes. Computed during ingest, stored in `attrs.etag`.
- Multipart: MD5 of the concatenated binary part MD5s, plus `-<part count>`. Stored in `attrs.etag`.
- `If-Match` and `If-None-Match` compare against the stored ETag.

### Multipart internals

- Each part is ingested independently (chunked on its own) and stored as a File, and acknowledged once its pack is durable. Upload state lives in tables `uploads` and `upload_parts` in `cache.db` (schema version 2); part data lives in sealed packs. If `cache.db` is rebuilt, open uploads are lost (clients get `NoSuchUpload` and restart), which is acceptable.
- Complete builds one File whose chunk list is the concatenation of the part chunk lists. No data is copied.
- Part Files and their chunks are GC roots while the upload is open. Abort, or expiry after 7 days (configurable), releases them.
- Digests for the completed File are computed by the background job.

### Errors

S3 XML error bodies with standard codes: `NoSuchBucket`, `NoSuchKey`, `NoSuchUpload`, `InvalidPart`, `InvalidPartOrder`, `PreconditionFailed`, `AccessDenied`, `SignatureDoesNotMatch`, `InvalidArgument`, `BadDigest`, `NotImplemented`. A write blocked by a lock returns `403 AccessDenied` with message `path is locked by <owner>`.

## Git compatibility

Served under `/git/{namespace}.git` on the main listener. Authentication: HTTP Basic with username and token as password. Implementation decisions for fetch (own pack writer and pkt-line code, negotiation, on-demand `git_map`) are in ADR-0008.

### Protocol scope

- `GET /info/refs?service=git-upload-pack` and `POST /git-upload-pack`: protocol v2 (`ls-refs`, `fetch`, `object-info`), with fallback to v0/v1 for older clients.
- `GET /info/refs?service=git-receive-pack` and `POST /git-receive-pack`: v0/v1.
- Fetch capabilities: `side-band-64k`, `ofs-delta`, `shallow`, `deepen`, `filter` (`blob:none`, `blob:limit=<n>`, `tree:0`), `thin-pack`, `include-tag`, `allow-reachable-sha1-in-want`.
- Push capabilities: `report-status`, `side-band-64k`, `atomic`, `delete-refs`, `ofs-delta`, `quiet`, `push-options`.
- Partial clone (`filter`) lets clients fetch blobs lazily. With D19, F1 always exposes real content to Git, so partial clone (`--filter=blob:limit=<n>` or `blob:none`) is how Git users avoid downloading large blobs. The choice is made by the client, not by F1.

### Object mapping

Table `git_map(f1_hash, git_sha1, kind)` in `cache.db`, with indexes in both directions. Export is deterministic, so the table is rebuildable by re-exporting from the refs. Blob SHA-1s come from DigestRecords (D21), so the rebuild reads only tree, commit and tag objects, never file content. Version 1 computes missing entries on demand during fetches and memoizes them (ADR-0008); a background rebuild can be added later. Many F1 hashes may map to one Git SHA-1 (trees differing only in S3 attrs).

**Export (F1 → Git)**, computed lazily during fetch and memoized:
- File entry → Git blob. SHA-1 from the DigestRecord (`sha1git`).
- lfs entry → Git blob with the bytes in `attrs.lfs_pointer`.
- Tree → Git tree. If the directory has `git_raw` (or the commit has `git_tree_raw` for the root), use those bytes verbatim after checking that they parse to the same names, kinds and targets. Otherwise flatten the prolly tree, sort entries by Git rules (directories compare as `name/`), and use modes from the entry kind table.
- Commit → Git commit. Serialize tree, parents (mapped), author, committer with original tz, `git_extra` headers in order, raw message. If `git_raw_hint` is present, use it verbatim after checking that it matches the fields.
- Tag → Git tag, same approach.

**Import (Git → F1)**, during push:
- Spool the pack to `tmp/` and index it (offset, type and SHA-1 of every object, after delta resolution).
- Compute every object's SHA-1 with collision detection (SHA-1DC, as Git does) and reject the push if a collision attack is detected.
- Convert in dependency order, because Git packs usually put commits first and blobs last, while D9 requires referenced objects to be stored first: blobs, then trees bottom-up, then commits and tags in topological order.
- Blobs → Ingest as Files, writing DigestRecords.
- Blobs that are LFS pointers (exact pointer syntax) at paths in trees: when building the tree, if the pointer's `oid sha256` has a DigestRecord, create an `lfs` entry with the pointer bytes and the File reference. If the LFS object is missing, reject the push with a message telling the user to run `git lfs push`.
- Trees → F1 trees. Names are stored as bytes. Modes map to kinds; non-standard modes (`100664`, zero-padded modes, and so on) map to the nearest kind. Trees with duplicate names are rejected. If re-encoding the resulting F1 directory does not reproduce the pushed Git tree bytes, store the original bytes as a GitRaw object and reference it from `attrs.git_raw` of the directory's entry, or from `Commit.git_tree_raw` for a root tree.
- S3 attrs carry forward: when a pushed tree entry has the same Git blob SHA-1 and mode as the entry at the same path in the ref's old commit, reuse the old F1 entry with its attrs (`ct`, `meta`, `etag`, `mtime`, `cks`). Otherwise the entry gets no S3 attrs. Unchanged files therefore keep their ETag and LastModified across pushes.
- Commits and tags → F1 objects, preserving every header and the raw message.
- After import, re-export every new commit, tag and tree, and verify that the computed SHA-1 equals the pushed SHA-1. If a commit cannot round-trip, store `git_raw_hint`. If it still fails, reject the push. This guarantees that Git SHAs never change.

**Writes from other protocols into Git namespaces**: native and S3 writes create normal F1 commits. Their Git form is derived deterministically (author and committer = principal, email from identity store, tz `+0000`). Files are always exported as real Git blobs, whatever their size.

**LFS is for compatibility only.** F1 never converts files to LFS pointers. `lfs` entries exist only because a Git client pushed LFS pointers; F1 keeps them so that commit SHAs round-trip. Native and S3 reads of an `lfs` entry return the real content. A native or S3 write to a path that currently holds an `lfs` entry replaces it with a new `lfs` entry (new content, canonical pointer), so the repository stays consistent for the Git users who chose LFS.

**Streaming in the Git layer.** Push packs are spooled to `tmp/` and parsed in a streaming way; blob content goes straight into Ingest. Delta resolution against a large base reads the base through Open with ranged reads, never by loading it whole. Fetch streams blob content from Open into the pack writer. If a `go-git` plumbing component cannot do this with bounded memory, replace that component with F1 code (ADR required).

### Ref and lock checks on push

Each ref update in a push goes through `UpdateRef`, which enforces write ACLs and locks on the diff between old and new commit. With `atomic`, all ref updates are applied in one multi-ref update or none. Implementation decisions for push and LFS (pack parsing, import order, carry-forward on the branch head, LFS pointer handling) are in ADR-0009.

### Pack generation

- Trees and blobs of at most 1 MiB are sent as deltas against the previously sent version of the same path when that halves them (ofs-delta if negotiated, else ref-delta); larger blobs stream whole (ADR-0012).
- Thin packs and `ofs-delta` decoding are required for push.

### Git LFS

Under `/git/{namespace}.git/info/lfs`:
- Batch API (`objects/batch`) with the `basic` transfer adapter. Upload and download URLs point to F1 endpoints that stream into or out of Ingest/Open. `oid` is SHA-256, looked up through the `digests` index of DigestRecords (D21).
- Locking API (`locks`, `locks/verify`, `locks/{id}/unlock`) mapped to F1 locks. `git lfs lock` and `git lfs locks` then work against F1.

## Native client (`cmd/f1`)

For SVN-style use: large binary repositories, sparse checkouts, locks. Code repositories use Git.

Commands: `clone [--sparse <path>...]`, `sparse add|remove <path>`, `status`, `add`, `rm`, `commit -m`, `pull`, `push`, `log`, `diff`, `lock <path>`, `unlock <path>`, `locks`, `checkout <ref>`, `branch`.

Working copy:
- `.f1/` holds config, the current commit, an index (path → File hash, size, mtime, inode), sparse patterns and a local object cache.
- Status uses the index (mtime and size) and re-hashes only changed files.
- Files in `locks = required` patterns are checked out read-only until locked (as `svn:needs-lock`).

Pull and merge:
- Fetch only trees within the sparse patterns, and files on checkout.
- Merge per path: if only one side changed, take it. If both changed, the merge strategy depends on content type, never on size:
  - Text (valid UTF-8, no NUL bytes in the first 8 KiB; overridable per glob in the `[merge]` table of `f1repo.toml`, e.g. `"*.svg" = "text"`): 3-way line merge with conflict markers. The implementation must use linear-space diff (Myers with divide-and-conquer or patience diff on line hashes) so large text files merge with bounded memory.
  - Binary: keep both as `<name>.mine` and `<name>.theirs` and mark a conflict.
- Push = upload missing objects, `POST /ns/{ns}/commits`, which performs the ref CAS. Push asks the server to rebase each commit when others changed only other paths, then updates the working copy as pull does; `--no-rebase` refuses any change on the server (ADR-0014). On a conflict, the client tells the user to pull. If the server answers that objects are missing (they were collected by GC after `objects/missing` reported them present), the client uploads them and retries, up to a fixed number of times.
- The native client needs read permission on the whole namespace (D13). Sparse checkout limits download size; it is not an access-control mechanism.

Implementation decisions (working copy files, path-addressed reads without pristine copies, rebase of local commits path by path, conflict resolution with `add` and `rm`, the two-level diff that bounds merge memory, and the refusal of client-uploaded DigestRecords) are in ADR-0010.

## Retention and garbage collection

### Retention

- `all`: keep every commit.
- `last:N`: per ref, keep the last N commits in first-parent history.
- `days:N`: per ref, keep commits newer than N days.
- `none`: keep only the commit each ref points to.

Truncated history: the oldest retained commit keeps its parent hashes, but the parents are no longer stored. The core treats missing parents of a retained commit as the history boundary. This is the only allowed exception to D9; `fsck` accepts missing commit parents only in namespaces with truncating retention. Git namespaces cannot use truncating retention (D12).

### GC algorithm

Mark and sweep with generations, never reference counting. GC runs per repository and touches only that repository's directory.

0. If a hold is active (freeze, archive, backup), skip this repository.
1. Start a GC epoch E. Record its start time T.
2. Roots: all refs at the moment of marking, retained commits per retention policy, open multipart uploads, active client upload leases, objects written after T minus the grace period (default 24 h).
3. Mark: traverse from roots. Use tree hashes to skip shared subtrees already marked. A DigestRecord is live exactly when its File is live.
4. Condemn: under the repository's write mutex, compute the condemned set = stored objects − marked − touched set (below), and publish it to the core's existence check. From then on, existence checks report condemned objects as not stored, so a writer that needs one stores a fresh copy (with a new index row in a new pack, which the sweep must not overwrite).
5. Sweep: for each pack, compute the live ratio. Delete packs with no live objects. Rewrite packs with live ratio below 50 % into new packs, update the index, then delete the old packs. Merge small packs as in "Pack store".
6. Clean metadata: `git_map` and `digests` rows whose F1 hash is no longer stored.

Touched set: from the start of the epoch until the condemned set is published, the core records every hash that an existence check reports as stored (dedup hits during ingest, D9 checks, `objects/missing` answers). These hashes are treated as live in step 4. This closes the race where a write deduplicates onto an old, unreachable object after marking and before sweeping. The grace period alone does not cover it, because the reused object is old.

Safety rules:
- Objects newer than the grace period are never deleted.
- Old objects reused by concurrent writes are protected by the touched set and the condemned-set check above.
- A pack rewrite updates index rows atomically before old packs are deleted.
- Old packs are deleted only after a second grace period (default 1 h), so in-flight reads complete.
- A `gc --dry-run` mode reports what would be deleted.

Implementation decisions are in ADR-0011. They cover:
- the store as the single point of cooperation with writers, with checks and writes atomic with the epoch's steps;
- pack age from the pack's ULID;
- DigestRecords marked through their Files;
- D9 kept for every stored object, so the references of garbage left in mostly live packs are rescued;
- scheduling with `[server.gc]` and `f1d gc`.
Client upload leases are not needed: the grace period, the touched set and the native client's push retry cover uploads.

## Identity and authorization

### Principals

- Principals and groups in `server.db`. Admin CLI: `f1d user add`, `f1d token create`, `f1d s3key create`, `f1d acl set`.
- Principal names use G1's syntax, so that administrators write ACLs the same way for both systems: `user:<name>` for a caller and `group:<name>` for each of its groups. For an OIDC caller, `<name>` is the value of the configured principal claim (default `sub`); for a local user, the name given to `f1d user add`.
- Each principal also records the issuer that authenticated it (`local` for local users). The issuer is internal: ACLs, locks, `refs.log` and commit authors use only the principal name. A name belongs to one issuer: a local user cannot be created with the name of an existing external principal, and a token from an issuer whose principal name is taken by another issuer is refused (and logged as a security event).
- Principals from the OIDC provider are created on first successful authentication (just-in-time). Their groups come from the token on every request, as in G1: a membership change takes effect when the token is refreshed, so token lifetimes should be short (5 to 15 minutes, the provider's setting).
- Authorization check function used by all protocol layers: `Allow(principal, groups, ns, path, perm)`. ACLs refer to principal and group names, never to issuers or token types.

### Credentials per protocol

| Protocol | Credential |
|---|---|
| Native API | `Authorization: Bearer <token>` |
| Git and LFS | HTTP Basic, password = token (username ignored), or `Authorization: Bearer <token>` |
| S3 | SigV4 access key and secret (local only; SigV4 cannot carry a bearer token) |

An externally authenticated principal can create S3 keys for itself through the native API (`POST /api/v1/me/s3keys`), so users of the enterprise identity provider (the same users as G1's) can use S3 tools. Requests signed with such a key carry the principal's groups as of the key's last use with a token: each authenticated native request refreshes them, and they expire after a configured time (default 24 h) until the next one.

### Token verifiers

```go
type Verifier interface {
    // Verify returns the principal for a token, or ErrUnknownToken if this
    // verifier does not handle tokens of this form (the next verifier is tried).
    Verify(ctx context.Context, token string) (Principal, error)
}
```

Verifiers are tried in configured order. Version 1 implements:

1. **Local tokens.** Opaque random tokens with prefix `f1_`, stored as SHA-256 hashes, with expiry and revocation.
2. **OIDC bearer tokens from the enterprise identity provider** (Keycloak, Microsoft Entra ID, ...), the provider G1 also trusts (G1 plan §10.1, `doc/spec/access.md`). G1 does not issue tokens; F1 and G1 are both relying parties of the same provider, each registered with its own audience. Users obtain tokens with the authorization code flow with PKCE, services with the client credentials grant. Tokens are signed JWTs, verified locally:
   - Discover the provider from its issuer URL (`<issuer>/.well-known/openid-configuration`) and fetch the `jwks_uri` it names; `jwks_url` overrides discovery. Cache keys, refresh on unknown `kid` (rate-limited) and every 10 minutes.
   - Check signature (RS256, ES256, EdDSA), `iss`, `aud` (must contain one of the configured audiences, normally `f1`), `exp`, `nbf` with 60 s clock skew.
   - The principal is `user:<value of principal_claim>` (default `sub`); groups are `group:<value>` for every value of every claim in `group_claims` (default `["groups"]`; a claim may be a string or a list of strings, and roles claims work too). A token without the principal claim is refused.
   - Reject `alg: none` and HMAC algorithms.

```toml
[[auth.oidc]]
name            = "corp"                                       # internal issuer label, used in logs
issuer          = "https://login.example.com/realms/acme"      # the same provider as G1
audiences       = ["f1"]
principal_claim = "sub"
group_claims    = ["groups"]
# jwks_url      = "https://login.example.com/realms/acme/protocol/openid-connect/certs"   # optional; overrides discovery
```

Configuring several providers is allowed; their principal names must not collide (see "Principals").

Later: OIDC login in F1's own clients (authorization code with PKCE in a browser, device flow for the CLI). This reuses the same verifier; only token acquisition is new. Token exchange (RFC 8693) for back-ends acting on behalf of a user, as G1 records it, is a later extension. Opaque tokens are not expected (G1 uses JWTs from the same provider); if ever needed, a third verifier can use OAuth 2.0 token introspection (RFC 7662).

## Operations

- Single binary `f1d` with subcommands: `serve`, `gc` (`--dry-run`, `--grace`, `--list`; through the admin socket, or offline with `--dir`), `fsck`, `user`, `token`, `s3key`, `acl`, `ns`, `repo` (`archive`, `freeze`, `unfreeze`, `import`, `attach`, `detach`, `export-acl`, `import-acl`, `rebuild-cache`), `backup`.
- Admin commands that act on a running server talk to it over a Unix socket (`<data_dir>/admin.sock`, mode 0600; HTTP with JSON bodies, ADR-0004). The socket also prevents a second server on the same data directory. Commands that work offline (`fsck`, `rebuild-cache`, `archive` of a detached repository) open the directory directly and refuse if the server holds it.
- `fsck <ns>`: verify pack checksums, that `cache.db` matches the packs, reachability from refs, and invariant D9. Works on a copied or extracted repository directory without a server (`f1d fsck --dir <path>`).
- Metrics: Prometheus text format on `/metrics`, written directly (`internal/metrics`; metric list in doc/admin.md).
- Structured logging with `log/slog`.
- Graceful shutdown: stop accepting, finish requests, seal and flush packs.
- Backup: `f1d backup <dir>` copies `server.db` (SQLite `VACUUM INTO`), then for each repository holds GC, reads `f1repo.toml`, `refs`, `refs.log` and `locks`, copies the packs not yet in the target, and only then replaces the target's text files and drops packs no longer live (so an interrupted backup stays consistent). `cache.db` is not backed up. Restore: start f1d on a copy of the backup, which is a data directory, or `f1d repo import` per repository (ADR-0012).

## Repository layout

```
cmd/f1d/                 server and admin CLI
cmd/f1/                  native client
internal/hash/           BLAKE3 wrapper, Hash type, encoding
internal/chunk/          FastCDC
internal/object/         File, FileIndex, Tree (prolly), Commit, Tag, DigestRecord, GitRaw; CBOR codec
internal/content/        ingest pipeline (chunk, hash, compress, digests) and random-access file reader
internal/pack/           pack format, writer, reader
internal/backend/        Backend interface; fs
internal/repodir/        repository directory: refs, refs.log, locks files, holds, freeze, archive, import, attach
internal/cache/          per-repository cache.db (index, digests, git map, commit graph, uploads); rebuild
internal/store/          pack store: open pack, sealing, existence check (D9), reads, consolidation
internal/tree/           prolly tree lookup, iteration, incremental update, diff
internal/serverdb/       server.db (principals, tokens, S3 keys, ACLs, registry); migrations
internal/server/         f1d server: listeners, repository manager, admin socket, import
internal/repo/           core operations, group commit, locks, ACL enforcement
internal/digest/         DigestRecords, digest index, background job
internal/policy/         namespace config, retention
internal/gc/             garbage collection, fsck
internal/auth/           principals, verifiers (local, JWT/JWKS), SigV4
internal/api/native/     native HTTP API
internal/metrics/        Prometheus counters, histograms and exposition
internal/oidctest/       in-process OIDC provider for tests
internal/gittest/        test-only importer of Git repositories (uses the production Git importer)
internal/api/s3/         S3 API (SigV4 verification is in internal/auth)
internal/api/git/        Git smart HTTP, pack export/import, object mapping
internal/api/lfs/        Git LFS batch and locking API
internal/client/         working copy, sparse, merge
internal/client/textmerge/  line diff, 3-way merge, unified diff (bounded memory)
pkg/f1client/            public Go SDK for the native API
doc/adr/                 architecture decision records
doc/format.md            on-disk format specification (written in phase 1)
doc/admin.md             administration guide
doc/manual.md            user manual
doc/benchmarks.md        benchmark results and regression procedure
deploy/                  systemd unit, container image, deployment config
scripts/benchcmp/        benchmark comparison (make bench-check)
internal/testutil/       deterministic test data, golden files
testdata/                golden vectors, Git fixture repositories
```

## Dependencies

| Purpose | Package |
|---|---|
| BLAKE3 | `lukechampine.com/blake3` |
| zstd | `github.com/klauspost/compress/zstd` |
| CBOR | `github.com/fxamacker/cbor/v2` (core deterministic encoding mode) |
| SQLite | `modernc.org/sqlite` |
| S3 client for tests only | `github.com/aws/aws-sdk-go-v2` (service/s3), imported only from `_test.go` files and `test/` |
| JWT and JWKS | `github.com/go-jose/go-jose/v4` |
| Git pack and pkt-line parsing | F1's own code, with bounded memory for any blob size (ADR-0008, ADR-0009); go-git is not used |
| SHA-1 with collision detection (Git import) | `github.com/pjbgf/sha1cd` |
| TOML | `github.com/BurntSushi/toml` |
| ULID | `github.com/oklog/ulid/v2` |
| 3-way text merge | F1's own code, with bounded memory for any file size (ADR-0010) |
| Markdown, web viewer only | `github.com/yuin/goldmark` (ADR-0018) |
| Syntax highlighting, web viewer only | `github.com/alecthomas/chroma/v2` (ADR-0018) |

Everything else uses the standard library (`net/http`, `log/slog`, `crypto/*`, `hash/crc32`).

## Phases

Each phase lists deliverables and acceptance criteria. CI runs `go vet`, `staticcheck`, `go test -race ./...`, and the integration suites that apply.

### Phase 0: skeleton

- Module, repository layout, Makefile, CI workflow, `f1d serve` stub with config loading, `slog` setup.
- ADR template and ADR-0001 recording the fixed decisions.

Acceptance: CI green; `f1d serve --config example.toml` starts and stops cleanly.

### Phase 1: chunks, objects, packs

- FastCDC with fixed gear table. BLAKE3 hashing. zstd.
- CBOR codec for all object types with type tags, including DigestRecord and GitRaw. File and FileIndex carry chunk sizes.
- Pack writer and reader, with type and uncompressed length in the footer index. `fs` backend.
- Concurrent ingest pipeline that writes DigestRecords inline (see "Ingest pipeline").
- Prolly tree boundary rules (D20) specified in `doc/format.md` with golden vectors, even though the tree code itself lands in phase 2, because they are part of the permanent format.
- `doc/format.md` describing every byte of the format.

Acceptance:
- Golden test vectors: fixed input files produce fixed chunk boundaries and hashes (stored in `testdata/`).
- Property test: inserting bytes in the middle of a file changes at most 3 chunks around the edit.
- Encoding is deterministic: encode → decode → encode is byte-identical, fuzz-tested.
- Random-offset reads of a multi-chunk File fetch and decompress only the chunks that overlap the requested range.
- Ingest throughput ≥ 300 MB/s single stream on a modern CPU with the `fs` backend, digests included (benchmark, not a CI gate).

### Phase 2: metadata, refs, trees, commits

- Repository directory: `f1repo.toml`, `refs` and `refs.log` with atomic replace and CAS, `locks` file with path escaping, `lock` file with `flock`, `tmp/` handling, crash recovery from `refs.log`.
- `cache.db` with object index and digest index; rebuild from pack footers and DigestRecords. `server.db` with registry and migrations.
- Prolly tree: build, lookup, iterate (byte order of full paths, including the `name/` merge rule for `tree` layout), update, diff.
- Core `Repo` read and write operations, invariant D9 checks through the single existence-check function that GC will use.
- Group commit for autocommit namespaces.
- Pack consolidation of small packs (no liveness needed; see "Pack store").
- `f1d fsck` (pack checksums, `cache.db` against packs, reachability from refs, D9) and `fsck --dir`. Phase 8 extends it with GC and retention checks.

Acceptance:
- Prolly tree property tests: the same set of entries always yields the same root hash regardless of insertion order; diff equals the naive set difference; changing only a target or attrs never changes node boundaries.
- Updating one key in a 1,000,000-entry flat tree writes O(log n) nodes: the test asserts fewer than 10 new nodes and less than 256 KiB of new tree data, with entries carrying typical S3 attrs.
- Iteration order of a `tree` layout namespace containing `a.txt` and `a/b` equals byte order of the full paths.
- Concurrent CAS test: N goroutines updating one ref; exactly one wins per round; no lost updates with group commit.
- Crash test: kill the process during commits (test harness with a subprocess); after restart, `fsck` reports no errors and every acknowledged write is present.
- Cache rebuild test: delete `cache.db` of a populated repository; on open, the index and digests are rebuilt without reading any chunk payload, and every object is readable.
- Pack consolidation test: 10,000 small commits leave at most a bounded number of small packs after consolidation; all content readable; `fsck` clean.
- Offline commands refuse to run on a repository the server holds open.

### Phase 2b: repository portability

- `f1d repo freeze|unfreeze|archive|import|attach|detach|rebuild-cache|list`, GC holds (also respected by pack consolidation), admin socket (ADR-0004).
- ACL export and import move to phase 3, where ACLs are introduced.
- The native API archive endpoint is delivered in phase 3, together with the rest of the native API.

Acceptance:
- Offline copy: stop the server, `cp -r` a repository to a second data directory, start a second server; all refs and content readable; `fsck --dir` clean.
- Frozen copy: freeze, `zip -r` the directory with the system `zip` tool, unfreeze; import the ZIP on a second server; identical refs and content.
- Online archive under load: continuous writes through the core `Repo` API during `f1d repo archive`; the imported archive passes `fsck` and its refs equal the refs read at archive start; no write fails. The same test is repeated with native API writes in phase 3 and S3 writes in phase 4.
- A ZIP above 4 GiB (ZIP64) round-trips.
- A repository copied without `cache.db` opens and rebuilds it.
- An archive with an unsupported `format` value is refused with a clear message.

### Phase 3: native API, identity, ACLs, locks

- Native HTTP API (including the archive endpoint), Go SDK, ACLs, locks and their enforcement in `UpdateRef`.
- `f1d repo export-acl` and `import-acl` (moved from phase 2b).
- Verifier chain with local tokens and OIDC providers (discovery, JWKS). Principal names `user:<name>` and `group:<name>`; just-in-time principals; configurable principal and group claims. `/api/v1/me/s3keys`.
- Namespace presets and config validation (D12), including `tree` layout path rules.

Acceptance:
- API integration tests through `pkg/f1client` against an in-process server.
- OIDC tests with an in-process test provider serving discovery and a JWKS: valid token accepted; wrong `aud`, wrong `iss`, expired, `alg: none`, HMAC-signed and unknown-`kid` tokens rejected; key rotation (new `kid`) picked up without restart; group claims (a string, a list, several claims) drive ACL decisions; a token without the principal claim is refused; a principal name already owned by another issuer (or a local user) is refused.
- ACL tests: read and write denied and allowed by prefix; longest prefix wins.
- Hash-addressed ACL tests (D13): a principal with read on only part of a namespace is denied every `/ns/{ns}/objects/...` endpoint and `POST /ns/{ns}/commits`; `diff` and path-scoped `log` return only readable paths; `log` without `path` is denied.
- Lock tests: commit modifying a required-lock path without the lock is rejected; with the lock it succeeds; under `exclusive`, another principal's commit to a locked path is rejected; creating a branch whose commits touch a locked path is checked against its merge base with `default_ref`; lock expiry works.
- Online archive under load with native API writes (see phase 2b).

### Phase 4: S3 compatibility

- 4a: SigV4 (all payload modes), checksums, buckets, objects, listing, conditional requests, errors.
- 4b: multipart upload, UploadPartCopy, upload expiry.
- 4c: versioning mapping.

Acceptance:
- Integration tests with `aws-sdk-go-v2` against an in-process server covering every operation in the table, including presigned URLs and default SDK checksum behavior.
- AWS CLI (`aws s3 cp/sync/ls`, `aws s3api ...`), rclone (`copy`, `sync`, `check`), MinIO `mc mirror`. Each tool has a script in `test/s3tools/`, run by `TestS3Tools`; CI installs pinned versions with `test/s3tools/install.sh`, and the test skips a missing tool only outside CI.
- A selected subset of the Ceph `s3-tests` suite passes (list the selected tests in `test/s3tests.txt`; the rest are expected failures for unsupported features).
- Multipart of a 512 MiB object with 20 parts uploaded in parallel; downloaded content matches; ETag matches the AWS formula. With `F1_TEST_LARGE=1`: 5 GiB with 100 parts.
- Listing order in a `tree` layout bucket equals byte order of the keys (`a.txt` before `a/b`); `rclone check` and `aws s3 sync` report no differences on a second run.
- `tree` layout rejects folder-marker keys (`dir/`), `a//b`, leading `/`, `.` and `..` with `400 InvalidArgument`; `flat` layout accepts them.
- Online archive under load with S3 writes (see phase 2b).

### Phase 5: Git fetch

- Smart HTTP upload-pack, protocol v2 and v0/v1, export mapping, partial clone filters, shallow.

Acceptance:
- Import fixture repositories into F1 through a test-only importer, then `git clone` them from F1. `git fsck` passes and `git rev-parse` of every ref equals the original.
- Fixtures include: merges, octopus merge, signed commits and tags, non-UTF-8 file names, non-UTF-8 commit message with `encoding` header, executable files, symlinks, submodules, an empty tree, and a commit with unusual tz (`-0030`).
- `git clone --filter=blob:none` works and later checkout fetches blobs on demand.
- Files written via native/S3 API appear in `git clone` with deterministic SHAs (two independent exports produce the same SHA).
- After deleting `cache.db` of a Git namespace, `git ls-remote` and `git clone` work and return the same SHAs; the Git map rebuild reads no chunk payloads (asserted with a counting backend).

### Phase 6: Git push and LFS

- Receive-pack with import, round-trip verification, atomic pushes, lock and ACL enforcement.
- LFS batch API and locking API.

Acceptance:
- Push each fixture repository to an empty F1 namespace; clone it back; all SHAs equal.
- Additional push fixtures: trees with mode `100664`, zero-padded modes and non-standard entry order round-trip with identical SHAs; a tree with duplicate entries is rejected with a readable message; a SHAttered-style colliding blob is rejected.
- A push to a namespace with S3-written files keeps `ct`, `meta`, `etag` and `mtime` of every file the push did not change.
- Push with a commit touching a locked path is rejected with a readable message on the client.
- `git lfs push`, `git lfs pull`, `git lfs lock`, `git lfs unlock`, `git lfs locks` work against F1 (requires `git-lfs` in CI; skipped otherwise).
- An LFS file pushed via Git is readable through the native and S3 APIs as its real content. An S3 overwrite of that path is visible to `git lfs pull` as the new content.
- A 512 MiB file (5 GiB with `F1_TEST_LARGE=1`) written via S3 appears in `git clone` as a normal blob; `git clone --filter=blob:limit=1m` skips it until checkout.
- Concurrent push and S3 writes to the same namespace: no lost updates; losers get a clear non-fast-forward or precondition error.

### Phase 7: native client

- `cmd/f1` with the commands listed above, sparse checkout, read-only checkout of required-lock files, merge.

Acceptance:
- End-to-end scripts: two working copies, concurrent edits, pull, merge, conflicts on text and binary files.
- Sparse clone of one subdirectory of a 5 GB synthetic repository (50 GB with `F1_TEST_LARGE=1`) downloads only that subtree's data (assert bytes transferred).
- Push retry: when GC collects an object between `objects/missing` and `POST /ns/{ns}/commits`, the client re-uploads and the push succeeds.

### Phase 8: retention and GC

- Retention policies, GC with touched set and condemned-set check, pack compaction, multipart and lease roots, `fsck` extensions (retention boundaries, DigestRecord liveness).

Acceptance:
- GC under concurrent write load (writes reuse old chunks during GC) loses no data: `fsck` clean and all acknowledged content readable after GC.
- Deterministic race test: with GC paused between condemn and sweep (test hook), a write that deduplicates onto an unreachable old chunk stores a fresh copy or keeps the old one live; after sweep, the written file is fully readable.
- `fsck` on a `last:N` namespace accepts the missing parents of boundary commits and reports any other missing object.
- GC started during `f1d repo archive` or while frozen skips the repository; the archive stays valid.
- `retention = none` namespace with 1,000,000 overwrites of 1,000 keys: storage after GC is within 2× the live data size.
- Dry-run output equals actual deletions in a second run.

### Phase 9: hardening and performance

- Delta compression in Git pack generation.
- Evaluate a delta-encoded chunk type for storage of small, frequently edited files (see "Known limitations"); ADR required, since it is a format addition.
- Read caches tuned, pack prefetch for sequential reads.
- Metrics, admin docs, `f1d backup`, example deployment (systemd unit, container image).

Acceptance: documented benchmarks for ingest, small Put rate, S3 GET latency, and `git clone` of a large fixture; no regressions above 10 % between releases.

## Test infrastructure

- Unit tests next to code. Fuzz tests for every decoder: CBOR objects, pack files, pkt-line, Git packs, SigV4 header parsing, aws-chunked streams, LFS pointers.
- Integration tests in `test/` start an in-process server on random ports with a temporary directory.
- External tools (git, git-lfs, aws, rclone, mc) are detected at test start. Tests that need a missing tool are skipped with a message, never silently passed.
- Size transparency suite (D19), added from phase 3 on and extended in each later phase: the same test cases run with file sizes 0 B, 1 B, 16 KiB − 1, 16 KiB, 256 KiB + 1, 64 MiB + 1, 1 GiB and 10 GiB (the last two only with `F1_TEST_LARGE=1`). Every protocol path is covered: native Put/Get/Range, S3 PutObject/GetObject/multipart/CopyObject, Git push/fetch/partial clone, LFS, native client checkout/commit/merge, archive and import. Each case checks content, digests and that peak RSS of server and client stays below a fixed bound (default 512 MiB) measured in a subprocess.
- Golden vectors in `testdata/` are regenerated only by an explicit `go test -run Golden -update`, and changing them requires an ADR (format change).
- Large tests (`F1_TEST_LARGE=1`: multi-GiB objects, 50 GB synthetic repositories) need far more disk than standard CI runners have. They run on a dedicated large-disk runner (nightly or before release); default CI runs the scaled-down versions given in each phase. Synthetic repositories may reuse content to save disk only where the test does not measure bytes transferred or stored.

## Open questions for the user

1. ~~G1 token format.~~ **Resolved (2026-10-12, from `../g1`):** G1 issues no tokens. It accepts OIDC bearer tokens (signed JWTs) from the enterprise identity provider, verified against the provider's JWKS found by discovery (G1 `internal/api/auth.go`, plan §10.1). F1 trusts the same provider; no introspection verifier is needed. See D17 and "Token verifiers".
2. ~~G1 claims.~~ **Resolved:** groups come from configurable claims, default `groups` (string or list; roles claims work too), the principal from `sub` by default, and principals are named `user:<sub>` and `group:<name>`. The audience is per service: G1 accepts its own (its example uses `g1`); F1 is registered with the provider under its own audience, `f1` by default.
3. ~~S3 folder markers.~~ **Resolved (2026-10-13, confirmed by the user):** `tree` layout rejects keys ending in `/` and the other keys the hierarchy cannot represent with `400 InvalidArgument`, pointing to `layout = flat`, which accepts them.
4. ~~Path-limited readers.~~ **Resolved (2026-10-13, confirmed by the user):** principals with read access to only part of a namespace use the path-addressed native endpoints and S3; the native client, Git and the object endpoints need whole-namespace read (D13).
