# ADR-0003: Repository files, recovery and core semantics

- Status: accepted
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

Phase 2 implements the repository directory, the pack store, `cache.db`, the prolly tree operations and the core commit path. The plan leaves several details open. This ADR records how they were settled.

## Decision

### refs.log and crash recovery

1. **Line format.** A `refs.log` line is `<time> <tx> <i>/<n> <principal> <refname> <old> <new> <reason>`. This extends the plan's format with a group ID and an `i/n` counter, so recovery can tell a complete multi-ref group from a torn one. Fields are percent-escaped as in `locks`, and an empty field is written as `-`.
2. **A group is one write followed by fsync.** On open:
   - An incomplete trailing line, or an incomplete trailing group, is truncated away; it was never applied.
   - If `refs` still holds the old values of the last complete group, the group is applied (rolled forward). `refs.log` is the commit point. Every object it references was sealed before it was written, so rolling forward is always safe.
   - If `refs` matches neither the old nor the new values of the last group, `refs` is kept as the source of truth and a warning is logged.
3. **Bounded read on open.** Opening reads `refs.log` backwards in growing windows, starting at 64 KiB, until it holds the last complete group. Open time does not grow with history.

### Repository lock

4. **flock.** The repository lock is an exclusive non-blocking `flock` on the empty `lock` file. Hosting F1 on Windows is a non-goal (`doc/plan.md`), so Unix-only locking is sufficient. On other platforms `Open` fails with a clear error.

### cache.db and the pack store

5. **cache.db.**
   - `cache.db` runs in WAL mode with `synchronous=NORMAL`, because it is rebuildable. `server.db` uses `synchronous=FULL`.
   - A cache with another schema version (`user_version`), or one that fails to open, is deleted and rebuilt.
   - If an indexed pack is missing or has a different size, the cache is rebuilt in full.
   - Packs on disk that are not indexed, such as a pack sealed just before a crash, are indexed incrementally from their footers and DigestRecords.
6. **One open pack.** Each repository has one open pack, `tmp/<ulid>.pack.open`. Sealing finishes the pack, fsyncs it, renames it into `packs/` (no copy) and fsyncs the directory, then indexes it in one transaction. `UpdateRefs` seals the open pack before appending to `refs.log`, which enforces write ordering rule 1.
7. **Consolidation and deferred deletion.**
   - Consolidation copies the objects whose index rows point to small packs.
   - It moves those rows to the new packs and removes the old pack rows in one transaction.
   - It records the old files in `pending_deletes` with a deadline. Files past the deadline are deleted on the next consolidation run.
   - Packs pending deletion are not re-indexed on open.
   - If a crash loses `pending_deletes` (cache rebuild), the old packs are indexed again (duplicates are harmless) and merged again later.
8. **D9 is checked in `Store.Put`.** Every CBOR object is decoded and its references are checked through `Store.Has`. That is the single existence check GC will cooperate with (phase 8).

### Core semantics

9. **Paths in `tree` layout.**
   - Paths may not be empty, start or end with `/`, contain `//`, `.` or `..` components, or contain NUL.
   - Writing below a file, or a file over a directory, is `ErrPathConflict`.
   - Deleting a file and writing below its path in the same commit is allowed.
10. **Deletes.** Deleting a missing path is a no-op. Deleting a directory path is `ErrIsDir`; only renames move directories.
11. **Directories.** A directory that becomes empty is removed, as in Git. A directory whose content changes loses its `git_raw` attribute.
12. **Changes within one commit.** Renames are resolved against the parent tree. For repeated paths, the last change wins.
13. **Native and S3 commits.** These record times in seconds with timezone `+0000`; the committer defaults to the author.
14. **Group commit.**
    - A batch is committed with the first request's author and principal. Its message is `autocommit: N changes`, or the request's own message for a batch of one.
    - Preconditions are evaluated in order against the batch's state.
    - On a CAS conflict, the accepted requests are re-evaluated against the new head, up to 20 times.
    - If a batch fails for another reason, each change is committed alone, so one bad change fails only its own request.
15. **`Log` with a path** returns commits whose entry at the path (kind and target) differs from the entry in every available parent. A missing parent ends the walk on that branch (truncated history).
16. **`List`.**
    - Results are in byte order of the full path. In `tree` layout a directory sorts as `name/`.
    - With delimiter `/`, a directory below the prefix is reported as one prefix item without being read.

### fsck

17. **fsck severities.**
    - Errors: corrupt or missing data, D9 violations, refs pointing to missing objects.
    - Warnings: derived state that opening the repository repairs, such as a stale `cache.db` or a pending roll-forward.
    - `f1d fsck` exits 1 only on errors. It takes the repository lock and repairs nothing.

## Consequences

- A crash at any point leaves a repository that `fsck` reports without errors. One open of the repository brings it back to a completely clean state. The subprocess crash test checks this.
- `refs.log` lines are slightly longer than in the plan's original format.
- Repositories can only be served from Unix systems, in line with the non-goal of hosting F1 on Windows.

## Alternatives considered

- **Rolling back the last logged group instead of forward.** Equally consistent, but it discards an update whose log record was already durable. Roll-forward makes the log the single commit point.
- **A single `refs.log` line per group.** This would make torn groups trivial to detect, but multi-ref pushes would produce very long lines that are hard to read and audit.
- **Copying sealed packs through `Backend.Put`.** This doubles the write volume for every pack. Renaming within the repository directory is atomic and free.
