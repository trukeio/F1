# ADR-0019: Importing Subversion repositories

- Status: accepted
- Date: 2026-10-18
- Deciders: Rolf Veen

## Context

SVN users move to F1 by migrating (`doc/plan.md`, "Non-goals"; ADR-0017), but F1 has no tool for it. ADR-0017 recommends building an importer first: read a dump, write one F1 commit per revision onto refs from the standard layout, map properties, and record the source revision in each message.

Converting to Git first works for code repositories. svn-all-fast-export or git-svn, then a push (manual §2.4, "Importing from Git"). It fits SVN-style repositories of large binaries poorly:

- the result is a `git` namespace, not one set up for the native client with required locks;
- the intermediate Git repository stores every binary version whole, and `git pack-objects` needs a lot of memory and time on multi-GB histories;
- `svn:needs-lock`, `svn:mime-type` and revision numbers are lost unless the converter is told how to keep them.

Constraints:

- **D19.** Dumps of binary repositories hold files of any size, so the importer streams content with bounded memory.
- **Authors and dates are kept.** The native API writes every commit as its caller, at the current time. The repository package takes an author, a committer and a time (`repo.CommitRequest`).
- **No format change.** Properties that F1 cannot represent are reported, not stored (ADR-0017, item 4).
- **Dump formats as produced by Subversion 1.14:**
  - `svnadmin dump` writes format 2, with full texts.
  - `svnadmin dump --deltas` and `svnrdump dump` write format 3, with svndiff0 deltas against the copy source or the previous version.
  - Version 1.14.5 of `svn`, `svnadmin` and `svnrdump` is installed on the development machine.

## Decision

### Command

The importer is an `f1d` command, beside `repo import`:

```
f1d repo import-svn <ns> <dump|-> [--authors <file>] [--trunk <path>] [--branches <path>]... [--tags <path>]...
                                   [--root] [--create <preset>] [--dry-run]
```

- It reads a dump file, or standard input:
  - `svnadmin dump /srv/svn/boards | f1d repo import-svn boards -`
  - `svnrdump dump https://svn.example.com/boards | f1d repo import-svn boards -` for a server without shell access.
- With a running server it streams the dump through the admin socket, as the other `repo` commands do. Otherwise it opens the data directory directly.
- The namespace must exist and be empty, unless `--create <preset>` creates it (usually `svn`, or `git` for code). A namespace already imported from the same SVN repository continues an incremental import (below).
- It needs `layout = tree`. With retention other than `all` it warns, because GC would then remove imported history.
- `--dry-run` reads and maps the whole dump, writes nothing, and prints the report (below).

### Dump reading

- **Formats 2 and 3.**
  - Node actions: `add`, `delete`, `change`, `replace`.
  - Kinds: `file`, `dir`.
  - `Node-copyfrom-path` and `Node-copyfrom-rev`; property blocks, including `Prop-delta`.
- **Text deltas** in svndiff versions 0 and 1 (zlib, from the standard library). Version 2 needs LZ4, a new dependency. Subversion's own dumps do not use it, so it is refused with a message.
- **Delta sources** are read from F1 with ranged reads of the base file, window by window, so a delta against a large file needs no copy of it in memory.
- **Checksums.** `Text-content-md5`, and `Text-content-sha1` when present, are checked against the result. A mismatch stops the import.
- **Bounded memory.** Text content is ingested as it streams, with the same chunking as uploads. Memory holds one revision's change list and the revision map, not file contents.

### Layout

- **Standard layout (default).**
  - `trunk/` is the namespace's `default_ref`.
  - `branches/<x>/` is `heads/<x>`, and `tags/<x>/` is `tags/<x>`.
  - `--trunk`, `--branches` and `--tags` change the paths, and the last two may repeat, as in git-svn.
  - A ref name that `policy.ValidRefName` refuses has each forbidden character replaced by `-`, with the change reported. Two SVN names that map to the same ref stop the import.
- **`--root`.** The whole repository is `default_ref`, with no branch mapping. This suits hardware repositories that never used branches.
- **Paths outside the layout** (files next to `trunk/` in the standard layout) stop the import with the revision and path, and suggest `--root` or other layout paths.
- **Branch and tag creation.** A copy of a ref's root (`svn cp ^/trunk@r ^/branches/x`) creates the ref at the commit that holds `trunk@r`, with no new commit. A copy of a subdirectory, or a copy followed by changes in the same revision, creates a commit whose tree reuses the source entries. No content is copied.
- **Changes after creation.** A tag changed after its creation gets a new commit on top of the old one, and the tag moves, with a warning.
- **Deleted branches and tags** are kept, so their history survives GC. They are renamed to `heads/svn-deleted/<x>-r<rev>` or `tags/svn-deleted/<x>-r<rev>`, where `<rev>` is the deleting revision.
- **Copies inside a ref** (`svn cp`, `svn mv`) reuse the source entry. F1 records no copy source, so `f1 log` of the target starts at the copy (ADR-0017, item 5).
- **Merges.** `svn:mergeinfo` is dropped. Every commit has one parent.

### Commits

- **One revision makes one commit on each ref it changes.**
- **Refs move at checkpoints:** every 1,000 revisions or 1 GiB of content, and at the end. Each checkpoint is one atomic multi-ref update, as a Git push is, and never splits a revision. One update per revision would seal one pack per revision, because every ref update makes the objects it needs durable first. An interrupted import loses at most the revisions since the last checkpoint.
- **The lock policy is not applied.** The import writes history that no user is editing. A namespace with `locks = required:*.brd` would otherwise refuse every revision that changed a `.brd` file.
- Revisions that change nothing inside the layout, revision 0, and revisions that only change revision properties make no commit.
- **Author** from `svn:author`, through the authors file:

  ```toml
  jdoe = "John Doe <jdoe@example.com>"
  ```

  An author missing from the file becomes `jdoe <jdoe@<repository UUID>>`, as git-svn does, and is listed in the report.
- **Committer** is the author. **Time** comes from `svn:date`, in UTC.
- **Principal.** `refs.log` records `system:svn-import`.
- **Message** from `svn:log`, followed by two trailers:

  ```
  SVN-Revision: 1234
  SVN-UUID: 0f6c6f2e-…
  ```

  The trailers let users find a commit by its old revision number, and let an incremental import walk the history of earlier imports.

### Properties

| Property | Becomes |
|---|---|
| `svn:executable` | kind `exec` |
| `svn:special` with `link <target>` | kind `symlink` |
| `svn:mime-type` | `attrs.ct`, as an S3 Content-Type |
| `svn:needs-lock` | reported: the importer suggests a `locks = required:<globs>` line from the extensions of the files that carry it, and does not change the configuration |
| `svn:externals`, `svn:ignore` | reported per path |
| `svn:eol-style`, `svn:keywords` | dropped. Content is stored as it is in the repository: LF line endings for `native`, keywords unexpanded |
| `svn:mergeinfo`, other properties | dropped and counted |

- **Empty directories** are pruned, as in every F1 tree.
- **Locks** are not in dumps. Users take them again after the move.

### Incremental imports

- **The marker.** Every checkpoint also moves the ref `notes/svn-import`, in the same atomic update. It points at a commit with an empty tree whose message records the last revision imported, the repository UUID and the layout (`SVN-Revision`, `SVN-UUID`, `SVN-Layout`).
  - The marker is exact even when the last revisions wrote no commit: a tag made by a plain copy, a branch deletion, or a property change on the root.
  - The web viewer and the native client list only `heads/` and `tags/`, so the marker does not appear there. It can be deleted when the migration is over.
- **Continuing.** An import into a namespace that holds a marker continues with revision N+1, where N is the marker's revision:
  - `svnadmin dump -r N+1:HEAD --incremental`
  - `svnrdump dump -r N+1:HEAD --incremental`
- **Refused:**
  - a dump that starts at any other revision;
  - a dump of another repository (UUID);
  - other layout options than the marker records;
  - a ref whose last commit has no `SVN-Revision`: a commit made in F1 since the import;
  - a non-empty namespace without a marker.
- **No state is stored besides the marker.** The importer finds the ref of an SVN path by the name the layout gives it, when a node first touches it.
  - A copy from a revision of an earlier import walks that ref's first-parent history and reads the trailers.
  - A branch deleted after the source revision is found under `svn-deleted/`, whose names carry the deleting revision. Deleted branches are kept as refs for this reason too.
- **Not supported:** deleting a whole layout directory (`branches/`) in an incremental import. The importer would have to list every branch under it, which the layout's names cannot map back to SVN paths.
- **Ref moves.** Every ref update is a compare-and-swap. If a user pushes to an imported branch while an import runs, the import stops and names the ref. Teams import incrementally until the switch, then stop committing to SVN.

### Dry runs

`--dry-run` reads the whole dump and maps it without ingesting content or writing a commit or ref.
- The namespace may not exist yet. With `--create`, the dry run assumes that preset's default branch.
- It works on a namespace an earlier import filled, to check that a dump continues it.
- Its commit count assumes every revision that touches a branch changes it. Its byte count is the size of the texts in the dump, which for deltas is less than the content.

### Report

At the end, and with `--dry-run`, the importer prints:

- revisions read, commits written per ref, and bytes ingested;
- renamed refs and refs kept under `svn-deleted/`;
- authors not in the authors file;
- the suggested `locks` line;
- paths with `svn:externals` or `svn:ignore`;
- counts of dropped properties.

### Tests

- **Unit tests.**
  - The dump parser runs on dumps generated in the test with `svnadmin`, in formats 2 and 3.
  - svndiff runs on handwritten windows with each instruction kind, and on a zlib window.
  - The layout mapping has a table test.
- **Acceptance test** (`test/`, skipped when `svnadmin` is missing). A script builds a repository with the `svn` client, covering:
  - binary and text files, an executable and a symlink;
  - `svn:needs-lock` and `svn:mime-type`;
  - a branch from trunk and a branch from a subdirectory;
  - a tag changed after creation, a deleted branch, and a `replace` node;
  - a file larger than the chunk size changed in its middle;
  - a revision changing two branches.

  The test imports the repository, then checks every revision: `svn export -r N` of each branch must match F1's tree at the mapped commit, byte for byte and with the same kinds. It then repeats the check with `svnrdump` input and with an incremental import split at a middle revision.
- **Size test.** A 1 GiB file in a dump imports with the server's memory bounded, as in the D19 size tests.

### Phases

- **S0:** this ADR.
- **S1:** the dump reader and svndiff, with unit tests.
- **S2:** layout mapping and commits; `f1d repo import-svn`, offline and through the admin socket; the acceptance test.
- **S3:** incremental imports, the report, `--dry-run`, and the manual (a "Moving from Subversion" section in §2, next to "Importing from Git").

## Consequences

- **Decision (2026-10-18, the user): accepted.** Deleted branches and tags are kept as refs under `svn-deleted/`, and the importer is a server command only; an import capability in the native API is not planned.
- SVN repositories move to F1 with their full history, authors, dates and revision numbers, into any kind of namespace. An imported `git` namespace gets ordinary Git commits, so this is also a path from SVN to Git hosting.
- The importer runs on the server host as the server's user. Teams without access to the host ask an administrator, who can import from a remote SVN server with `svnrdump`.
- No change to the on-disk format, the native API or the Git layer. The trailers are ordinary message text, and the marker an ordinary ref. Git clients see the marker in `git ls-remote` as `refs/notes/svn-import`, but do not fetch it by default.
- A revision that only copies a whole branch, such as tagging trunk, writes no commit, so its log message and author are not kept: the tag points at the source's commit.
- Estimated size: 3,000 lines with tests, mostly the dump reader, svndiff and the mapping. No new dependency.
- Copy history and merge tracking are lost, as in every SVN-to-Git converter. `blame` and `log` stop at an `svn mv`.
- Dropped properties (`svn:eol-style`, `svn:keywords`, user properties) cannot be recovered from F1 later. The source repository, or its dump, should be kept.

## Alternatives considered

- **Convert to Git, then push.** This works today and remains the documented path for code. It is not enough for binary repositories, as described in Context.
- **An importer as a client of the native API.** It could run anywhere, with no host access. But the native API writes every commit as its caller at the current time. Keeping authors and dates would need an "import as another user" capability in the API, which is an impersonation right that version 1 has nowhere else. Server-side imports are enough.
- **Replaying a live repository (`svnsync`-style) instead of reading dumps.** `svnrdump` already turns any reachable repository into a dump stream, so reading dumps covers this without an SVN protocol client in F1.
- **Storing all SVN properties** in a new entry attr. This is a format addition (ADR-0017, item 4), useful only if SVN clients are served later. Not needed to migrate.
- **Deleting refs of deleted branches.** This matches SVN's view of the head revision, but GC would then remove their history, and resuming would lose the revision map. Keeping them under `svn-deleted/` costs only refs.
