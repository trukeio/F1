# ADR-0013: Delta-encoded pack records (evaluation)

- Status: proposed (needs a decision: it is a format addition)
- Date: 2026-10-15
- Deciders: Rolf Veen

## Context

doc/plan.md, "Known limitations", and phase 9 ask for an evaluation of a delta-encoded chunk type for small, frequently edited files.

F1 stores every changed version of a file whole: zstd-compressed chunks of at least 16 KiB, and files below that are a single chunk. A source code history therefore takes several times the space of a Git pack. The on-disk format is permanent once released, so any delta encoding is a format addition that needs an ADR and the user's approval.

## Measurements

Two Go projects were each pushed with their full history to an F1 Git namespace, and compared with `git gc --aggressive` of the same history.

| Repository | Commits | Git pack | F1 packs | F1 / Git |
|---|---|---|---|---|
| go.uber.org/zap | 698 | 1,420 KiB | 8,460 KiB | 6.0 |
| github.com/AllenDang/giu | 1,792 | 8,403 KiB | 24,320 KiB | 2.9 |
| F1 itself | 18 | 506 KiB | 1,236 KiB | 2.4 |

Where F1's space goes, for zap:

| Objects | Count | Stored |
|---|---|---|
| chunks | 2,322 | 5,166 KiB |
| tree nodes | 1,741 | 1,845 KiB |
| commits | 698 | 403 KiB |
| DigestRecords | 2,320 | 314 KiB |
| Files | 2,320 | 172 KiB |

Git stores 2,126 of zap's 2,320 blob versions as deltas, and 3,040 of giu's 3,404.

| Repository | Those blobs compressed whole | As Git's deltas |
|---|---|---|
| zap | 4,211 KiB | 249 KiB |
| giu | 10,438 KiB | 469 KiB |

Delta-encoding chunks would therefore bring zap from 8.5 to about 4.5 MiB (3.2× Git) and giu from 24 to about 14 MiB (1.7×).

Tree nodes are the next largest item: every commit rewrites the nodes on its changed paths. Git deltifies trees as well. Deltifying tree nodes too would bring zap to about 2.1–2.5 MiB, roughly 1.5–1.8× Git. The rest is F1's per-version metadata: Files, DigestRecords, and commits with F1 fields.

## Proposed decision

Do not add an object type. Add a **pack record encoding**:

- **Record.** A record flag (`FlagDelta = 0x02`, beside `FlagZstd`) means that the stored data is `[base hash][delta]`, zstd-compressed as a whole, in Git's delta format.
  - The base is another record in the same pack or an earlier one.
  - The record's hash, type and size are unchanged: the hash of the reconstructed payload. Object identity, deduplication, Git export and every reader are unaffected.
  - Only pack reading changes: resolving a record may require its base, with a chain length limit (e.g. 16).
- **Where deltas are made.** Writes stay as they are. Deltas are made when packs are rewritten: GC rewrites and consolidation, which are the `git gc` of F1.
  - For each chunk or tree node of a new version of a path, the base is the record of the previous version of the same path. Tree diffs give these pairs without reading content.
  - A delta is kept only if it halves the stored size.
- **GC and D9.** The base of a delta record is a reference of the record. It is marked with it, may not be condemned while the record lives, and fsck checks it. A pack rewrite that drops a base re-encodes the dependent records whole first.
- **Bounded memory.** Only records of at most 256 KiB are deltified (all chunks and tree nodes are), and a resolution chain is at most 16 records long.
- **Compatibility.** Format version 2 of `f1repo.toml`: an older server refuses a repository that may contain delta records, with the existing "upgrade F1" message.

### Cost

- **Reads.** A read of a deltified record also reads its chain. The read cache absorbs this for hot objects; large files are rarely deltified, because their chunks rarely repeat with small edits at 64 KiB granularity.
- **Code.** A delta encoder and decoder exist already (Git fetch and push). Repacking needs per-path pairing in the rewrite, and fsck and GC need the base reference.

## Alternatives

1. **Do nothing.** Code repositories with long histories use 3–6× Git's space. The plan already says that code repositories should use Git hosting, and F1's Git compatibility makes that a transfer of history, not a migration.
2. **A delta chunk object type** (as the plan first suggested). A new object type changes object identity: the same content would have two hashes. Deduplication and the object model would need special cases. The record encoding avoids this.
3. **Deltas at write time.** This needs the previous version of the path on the write path. That is cheap for the native client and Git push, but not for S3 PutObject, and it would slow every write. Deltifying in the background keeps writes simple.
4. **Smaller chunks** (e.g. 4 KiB minimum). This changes D2, a permanent constant, multiplies per-chunk metadata, and breaks deduplication with existing data.

## Recommendation

Implement the record encoding in a later version, after version 1 is in use. It saves 40–75 % of the storage of code histories and does not affect identities or write paths. Version 1 documents the limitation, as it does now.

**This ADR needs the user's decision before any implementation.**
