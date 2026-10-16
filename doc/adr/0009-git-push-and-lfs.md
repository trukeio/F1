# ADR-0009: Git push and LFS

- Status: accepted
- Date: 2026-10-13
- Deciders: Rolf Veen

## Context

Phase 6 serves Git push (receive-pack) and the Git LFS batch and locking APIs (doc/plan.md, "Object mapping", "Ref and lock checks on push", "Git LFS"). This ADR records the implementation decisions, which complete ADR-0008.

## Decision

### Packs

1. **F1 parses packs itself; go-git is not used.** A pushed pack is handled in five steps:
   - It is spooled to the repository's `tmp/`, and its trailer checksum is verified.
   - It is indexed in one sequential pass.
   - Non-delta blobs stream straight into Ingest.
   - Commits, trees and tags (at most 64 MiB each) are written to an object spool, a second file in `tmp/`, for the import. Memory holds only each object's type, offset and length: golang/go's 370,000 trees would otherwise take gigabytes. Pack entries are kept by value, and found by offset with a binary search.
   - Deltas (ofs-delta, and ref-delta including thin-pack bases from the repository) are applied afterwards. The delta result streams into Ingest as well, and bases are read with random access: from the spool for non-blobs, or through F1's ranged reads for blobs.
   No object is ever loaded whole unless it is a commit, tree or tag being resolved or imported, so memory stays bounded for blobs of any size (D19), and grows with a push's object count by a few hundred bytes per object, not by its size. Pushing golang/go (68,110 commits, 684,820 objects) peaks at 699 MB of server memory, against 2.6 GB when commits and trees were kept in memory.
2. **SHA-1 collision detection.** Every pushed object's id is computed with SHA-1DC (`github.com/pjbgf/sha1cd`), and a detected collision attack fails the push.
   - A Git object cannot carry the published SHAttered collision: its `blob <n>` header shifts the colliding blocks.
   - The test therefore checks the detector, which every object passes through, on the SHAttered blocks themselves (`testdata/shattered-1-prefix.bin`, rebuilt from the sha1cd test vectors).

### Import

3. **Order of conversion.**
   - Blobs are converted while the pack is read.
   - Trees, commits and tags are then imported in dependency order, iteratively (histories are deep).
   - Objects referenced but not in the pack must already be in the repository, resolved through `git_map` and DigestRecords. This is the connectivity check.
   - A failed import reports `unpack <reason>` and updates no ref. Objects already stored stay unreferenced until GC removes them.
4. **Existing Git ids must be resolvable.** The advertisement exports every ref first, so every object a client can reference is recorded.

### Ref updates

5. **Compare-and-swap.** Each command's old id must equal the Git id of the ref's current value. Otherwise it fails with `fetch first`; concurrent S3 or native writes produce the same answer.
   - Only `refs/heads/*` and `refs/tags/*` can be pushed, and branches must point to commits.
   - With `atomic`, all updates go into one multi-ref update or none.
6. **Access.**
   - A push needs whole-namespace read (D13) and write permission somewhere in the namespace.
   - Branch updates need write permission on every changed path (the `CheckPath` hook of `UpdateRefs`).
   - Tags and deletions need write permission on the whole namespace.
   - Locks are enforced in `UpdateRefs`. Rejections reach the client as readable reasons, for example `locked: a.txt is locked by user:alice`.
7. **S3 attributes are carried forward on the pushed branch head only.** The new head's tree reuses the old head's entries, with `ct`, `meta`, `etag`, `mtime` and `cks`, wherever the Git blob id and mode are unchanged. Unchanged subtrees are reused whole.
   - This gives an F1 commit equal to the pushed one except for attributes, with the same Git id (checked).
   - Intermediate pushed commits keep plain entries; S3 reads only the head.
   - Lock and ACL checks then see only real changes.

### Git LFS

8. **Pointer blobs.**
   - A pushed blob of at most 1024 bytes in the exact pointer syntax becomes an `lfs` entry pointing to the uploaded content, which is found by its SHA-256 through DigestRecords.
   - If the object was not uploaded, or its size differs, the push is refused with a message to run `git lfs push --all`.
   - Pointer blobs are also stored as Files, so a partial clone can fetch them by id. This removes the limitation in ADR-0008 (decision 14).
9. **Native and S3 writes over LFS files.** A native or S3 write to a path that holds an `lfs` entry produces a new `lfs` entry: the new content, its canonical pointer, and the old exec flag. This happens in the core commit path, so every protocol behaves the same.
10. **Batch API.**
    - Only the `basic` transfer adapter and SHA-256 are supported.
    - Upload and download URLs point to `info/lfs/objects/<oid>` on the same server, and actions carry the request's `Authorization` header.
    - Uploads are verified against the oid and acknowledged once durable. Uploads need write permission; downloads need whole-namespace read (D13).
11. **Locking API.** LFS locks are F1 locks on the requested ref, or the default ref when none is given. The owner name is the principal. Unlocking needs the lock permission, and `--force` needs admin on the path.

### Protocol and performance

12. **Receive-pack protocol.**
    - Protocol v0/v1 only; Git has no v2 push.
    - Capabilities: `report-status`, `side-band-64k`, `atomic`, `delete-refs`, `ofs-delta`, `quiet` and `push-options`. Push options are read and ignored.
13. **Export cost.** Exporting a commit costs the size of the directories it changed, as creating it does in Git. Blob ids are memoized for the duration of a request. A writer that makes very many commits in one large directory therefore makes the first fetch after them slow; the cost is proportional to the work Git itself would have done.

## Consequences

Every fixture pushes into an empty namespace and clones back with identical ids. A commit that lists a parent twice (Git allows it; golang/go has such commits) is stored with the parent once and its exact bytes in `git_raw_hint` (doc/format.md, "Commit"); before this was fixed, such a push failed. Non-canonical trees round-trip, and trees with duplicate names are refused. The LFS workflow works against F1: push with automatic upload, `git lfs pull`, lock, unlock and `git lfs locks`. LFS files read as real content through the native API and S3, and S3 overwrites are seen by Git LFS. Concurrent pushes and S3 writes lose no update.

## Alternatives considered

- **go-git's packfile parser.** It resolves deltas in memory, so it fails the bounded-memory requirement for large blobs.
- **Rejecting thin packs.** Git sends thin packs by default for incremental pushes, so this would break normal use.
- **Carrying attributes forward on every pushed commit.** It costs a tree rewrite per commit for no S3-visible gain; S3 reads branch heads.
