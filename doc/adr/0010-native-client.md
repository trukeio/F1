# ADR-0010: Native client

- Status: accepted
- Date: 2026-10-14
- Deciders: Rolf Veen

## Context

Phase 7 delivers the native client `cmd/f1` (doc/plan.md, "Native client"): working copies with sparse checkouts, local commits, push, pull with merge, and locks. The plan fixes the commands, the merge strategy by content type, and the push protocol. It leaves open the 3-way text merge implementation (a dependency to evaluate) and the details recorded here.

## Decision

### Code

1. **Layout.** The working copy logic is in `internal/client`, the text diff and merge in `internal/client/textmerge`, and `cmd/f1` is a thin command line. Integration tests drive both the binary (end-to-end scripts) and the package (byte counts, fault injection).
2. **No merge dependency.** F1 implements the diff, the three-way merge and the unified diff in about 900 lines, with property and fuzz tests, and a test against GNU `diff -u`. No library considered bounds memory for files of any size (decision 10).

### Working copy

3. **Metadata in `.f1/`.** The directory holds:
   - `config.json`: server URL, namespace, ref, and sparse patterns.
   - `state.json`: the base commit, unpushed local commits, paths scheduled for adding, unresolved conflicts, and kept commit messages.
   - `index.json`: for each path, its HEAD value, its local content hash, mtime and inode.
   - `objects/`: one pack file per local commit, in the F1 pack format.
   - `tmp/`: downloads and merge results in progress.
   JSON files are replaced atomically and fsynced. The bearer token comes from `F1_TOKEN` and is never stored.
4. **Reads use the path-addressed API.** Clone and `sparse add` use listings, pull and checkout use diffs, and file content uses `GET files/{commit}/{path}`, all limited to the sparse patterns.
   - The plan asks to "fetch only trees within the sparse patterns". Listings and diffs give the same result without fetching tree objects: only metadata inside the patterns, and only the content of files checked out.
   - Push uses the object endpoints. The client therefore needs whole-namespace read (D13).
5. **No pristine copies.** Checked-out content is not cached; a 50 GB checkout does not take 100 GB. The base version a merge or `diff` needs is downloaded when needed. The local object cache holds only the content of unpushed commits, and is deleted when none remain.
6. **Sparse patterns are directories.** `d` checks out `d/...`. `sparse remove` refuses to remove the last pattern, and any pattern with local changes below it.
7. **Status.**
   - A path whose kind, size, mtime and inode match the index is unchanged without reading it.
   - Otherwise the client hashes the file with the FastCDC chunking of ingest (`content.FileHash`) and compares it with the index's local hash.
   - Racy timestamps are handled as in Git: an mtime not older than the index file minus 1 s forces hashing.
   - The local hash is kept apart from the server's File hash, because a File uploaded with S3 multipart has other chunk boundaries.
8. **Path safety.** Paths from the server are checked before any write:
   - no absolute paths, no `.` or `..` components, no top-level `.f1`;
   - parent directories must be real directories, never symbolic links, so a checked-out symlink cannot redirect a write outside the working copy.
   Entries the client cannot represent (`gitlink`) are skipped with a warning. Namespaces with `layout = flat` are refused; they are for S3 tools.
9. **Required locks.** Files matching `locks = required` globs are checked out read-only unless the caller holds their lock. `lock` makes a file writable; `unlock` makes it read-only again. The glob rule is shared with the server (`policy.MatchGlob`).

### Commits, push and pull

10. **Local commits.** `commit` stores the changed files' content in a new pack and records each change as `{path, old, new}`.
    - Commits are pushed in order: upload the objects the server lacks (`objects/missing`), then `POST /commits` with the base commit as parent.
    - A CAS failure tells the user to pull. Since ADR-0014, push asks the server to rebase, so only a change to the same paths fails.
    - `missing_objects` makes the client check and upload again, up to 3 attempts in all. The objects reported present may have been collected by GC since.
    - DigestRecords are never uploaded (decision 15).
11. **Pull rebases path by path.** History stays linear, since the server never merges (D10). For each local commit, in order, each change to a path the server also changed is merged three-way: the change's old value, its new value, and the path's new value so far.
    - Equal results and one-sided changes need no content.
    - A clean text merge is stored as the rebased commit's new value.
    - A commit whose changes become empty is dropped, because the server already has them.
    - If a merge conflicts (text conflicts, binary content, delete/modify, symlinks), that commit and all later ones are folded back into working changes. Their messages are kept, and `commit` without `-m` uses them.
    - Uncommitted changes, including folded ones, are then merged the same way into the working files.
12. **Pull applies in two phases.** First every rebase, download and merge is computed into packs and temporary files. Then the planned renames are applied and the state is saved. A failed download or merge leaves the working copy as it was.
13. **Conflicts.**
    - Text conflicts are written between `<<<<<<< mine`, `=======` and `>>>>>>> theirs` markers.
    - Binary conflicts keep the working version as `<name>.mine` and the new version as `<name>.theirs`.
    - Delete/modify conflicts keep the surviving side as `.mine` or `.theirs`.
    - Conflicted paths are recorded; `commit` and `pull` refuse while any remain. `add` marks a path resolved and removes its `.mine` and `.theirs` files; `rm` resolves it as deleted. There is no separate `resolve` command.
14. **Text or binary.** A version is text if its first 8 KiB are valid UTF-8 (a rune cut at the 8 KiB boundary is allowed) and contain no NUL byte. A merge is textual only if all three versions are text. The namespace configuration overrides this per glob, with the longest matching glob winning:
    ```toml
    [merge]
    "*.svg" = "text"
    "*.dat" = "binary"
    ```
    The namespace listing returns this table (`NamespaceConfig.Merge`).
15. **DigestRecords are written by the server only.** `POST /objects` refuses them with 400. They assert digests that Git, LFS and S3 trust (D21), so a client must not be able to forge them. `POST /commits` schedules a background digest for every committed File that lacks one, as multipart uploads do. This closes a hole in the phase 3 API.
16. **Branches.**
    - `branch <name>` creates a branch at the base commit.
    - `checkout <branch|commit>` switches a clean working copy, with no changes and no unpushed commits. A commit hash gives a detached working copy, which cannot push.

### Text merge with bounded memory

17. **Two-level diff.**
    - Files up to `RegionLimit` (8 MiB) on both sides are diffed line by line as a whole.
    - Larger files are first cut into FastCDC chunks (the ingest chunker, so typical edits change one or two chunks), and equal chunks are matched by BLAKE3 hash. The matches are shrunk to whole lines.
    - Each region between matches is diffed line by line if it is at most 8 MiB on both sides. A larger region counts as replaced: the merge stays correct but is coarser, as a whole-region conflict if both sides changed it.
    - Memory is proportional to the number of chunks (one entry per 64 KiB on average) plus 8 MiB per region, never to the file size (D19).
18. **Sequence diff.** Patience diff anchors on elements unique in both sequences (their longest increasing subsequence) and recurses into the gaps. Gaps without unique elements use Myers' linear-space algorithm. The fallback is limited to 65,536 elements and edit distance 2,048; beyond that the gap counts as replaced.
19. **Three-way merge** follows diff3. Base ranges matched in both versions are stable. Between them:
    - a region changed on one side takes that side;
    - a region changed equally on both sides is taken once;
    - anything else is a conflict.
    Output is streamed.
20. **Unified diff** (`f1 diff`) uses the same matches, with 3 lines of context, hunks joined across at most 6 unchanged lines, and `\ No newline at end of file`.

## Consequences

- Each acceptance test is listed with its result.
  - Two working copies with concurrent edits, rebase, text and binary conflicts, folded commits, locks, sparse changes and branches pass as end-to-end scripts with the binary.
  - A sparse clone of one tenth of a 5 GB repository transfers the subtree's bytes plus about 2 KB of metadata.
  - Push succeeds after objects disappear between `objects/missing` and the commit.
- The size transparency suite covers checkout, commit, push, pull and a rebased three-way text merge at every size. Memory stays bounded.
- A pull of a large conflicting file needs temporary disk for the versions it compares, up to three times the file size.
- Rebased commits keep their messages but get new commit hashes on the server, as with `git rebase`.
- Phase 8 (GC) must keep objects that a client has just been told are present, or rely on this retry; decision 10 already handles the second case.

## Alternatives considered

- **Pristine copies of checked-out files**, as SVN keeps. Status and merges would need no download, but disk use would double for large binary repositories, the main use case.
- **Fetching tree objects** for sparse checkouts. Equal in transferred bytes for metadata, but more code in the client, which would also have to reimplement prolly-tree reads over HTTP.
- **Squashing local commits on pull.** Simpler, but loses commit messages and boundaries. Folding only from the first conflicting commit keeps clean commits intact.
- **A merge library** (go-diff, diffmatchpatch, Git's xdiff through cgo). Each holds whole files in memory, which breaks D19 for large text files; cgo would also break the pure Go build.
- **Accepting client DigestRecords with verification.** The server would have to read the whole content, which costs the same as computing the record itself.
