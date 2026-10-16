# ADR-0015: History indexes in cache.db

- Status: accepted
- Date: 2026-10-16
- Deciders: Rolf Veen

## Context

Two history queries walk the whole commit graph:

- **`Log` with a path** (`internal/repo/read.go`, used by `GET /log?path=` and `f1 log <path>`). It walks commits newest first, and for every commit looks the path up in the commit's tree and in each parent's tree. A rarely changed file makes it walk the whole history, doing O(depth × log n) tree reads per commit and parent, before it finds `limit` matches.
- **`MergeBase`** (`read.go`). It first collects every ancestor of `a`, however close the merge base is. It is called on every branch creation, for lock and ACL checks against the default branch (`refChanges` in `internal/repo/locks.go`). Each new branch therefore reads every commit in its history while the ref update waits, holding the GC lock shared.

Both are fine for the repositories F1 has been tested with: thousands of commits. Their cost is linear in history, and imported Git histories can reach 100,000 commits.

Scion ("Redesigning source control at Anthropic", 2026) and Git (commit-graph files with generation numbers and changed-path Bloom filters) both solve this with data derived from each commit and its parents, computed once and kept beside the history. F1's `cache.db` is the place for such data: it is derived from the packs and can be deleted and rebuilt (D7).

## Decision

1. **Measure first.** A benchmark (`internal/repo`, `-bench`, with a generated history, plus a manual run on an imported large Git repository such as `golang/go`, about 60,000 commits) measures:
   - `Log` with a path changed once, near the root of history, `limit` 20;
   - `MergeBase` at the creation of a branch from the head, and of two branches 10,000 commits apart.

   Items 2–6 are implemented only if one of these takes more than 1 s at 60,000 commits. The results are recorded in this ADR and in `doc/benchmarks.md` either way.
2. **Table.** `cache.db` schema version 4 adds, as an in-place upgrade like versions 2 and 3:
   ```sql
   CREATE TABLE commit_graph (
       commit_hash BLOB PRIMARY KEY,  -- 32 bytes
       generation  INTEGER NOT NULL,  -- 1 + max generation of the stored parents
       time        INTEGER NOT NULL,  -- committer time (Unix s)
       parents     BLOB NOT NULL,     -- concatenated 32-byte hashes, in order
       flags       INTEGER NOT NULL,  -- 1: a parent was not stored when the row was computed
       paths       BLOB               -- changed-path filter; NULL until computed
   ) WITHOUT ROWID;
   ```
   An older server opening a version 4 cache discards and rebuilds it, as it does today for any unknown version.
3. **Computed on demand, kept in memory and in cache.db.**
   - As with the Git map (ADR-0008), a row is computed when a query needs it. A query on a commit without a row first computes the missing rows of its stored ancestors, parents first. This reads each commit once.
   - Rows are written to `cache.db` in batches of 5,000.
   - They are kept in memory once loaded: walks read thousands of rows, which one SQLite query each would make too slow. The table is loaded on the repository's first history query.
   - Later queries pay only for new commits. A rebuilt `cache.db` starts empty and refills the same way.
4. **Generation numbers.** A parent that is not stored (truncated history, D9 exception) counts as absent, so a root has generation 1.
   - **MergeBase** walks from both sides by descending generation, marking each commit with the sides that reach it. The first commit reached from both sides has the highest generation of all common ancestors, so no other common ancestor descends from it: it is a best common ancestor. A branch created from the head of the default branch costs two rows instead of the whole history.
   - **Generations stay valid when GC removes commits**, because they only ever decrease along parent links. No row is recomputed.
   - **A missing parent stored again** could break the order: a row computed while a parent was missing has `flags` 1. If a walk finds such a parent stored, the server drops every row, logs a warning, and repeats the query once. This needs a truncated commit to be uploaded again, so it is rare.
5. **Changed-path filters.**
   - **Content.** The filter holds the paths changed relative to the first parent, or every path of the commit for a root. It includes every ancestor directory of each changed path, so `log` of a directory works. Its first byte records which of the two it is, or "any path" for a commit that changes more than 512 paths.
   - **Parameters are Git's:** 10 bits per path and 7 hash functions, with the hash taken from BLAKE3 of the path.
   - **Computed lazily.** A filter costs a first-parent tree diff, so it is computed when a path query first reaches the commit. The query then computes the filters of the next 256 commits it is likely to reach, with one worker per CPU. A query that stops early (`limit`) therefore pays for little more than the commits it visits.
   - **Use in `Log`.** If the filter excludes the path, the commit is skipped without reading trees. This is exact: `Log` includes a commit only if the path differs from every parent, so equality with the first parent excludes it. A filter is used only if it was computed against what `Log` compares with now: the first parent if it is stored, or nothing for a commit whose parents were all removed.
   - **False positives** (about 1 % per commit) fall through to today's tree comparison, so results are identical with and without the index.
6. **Walks use the rows.** `Log` with a path and `MergeBase` read parents, committer time and generation from the rows, and decode a commit object only to compare trees or to return it. `Log` without a path is unchanged: it stops after `limit` commits.
7. **GC.**
   - A walk checks that each parent is stored (`Store.Exists`, which does not take the GC lock and does not mark the object as referenced). A parent that is not stored is the history boundary, as today, whether or not it has a row.
   - Rows of commits that are no longer stored are deleted from `cache.db` when the table is next loaded. The sweep does not touch the table, so GC and the index stay independent.

## Results

The benchmarks are in `internal/repo/history_bench_test.go`. Each runs on a generated history (`F1_BENCH_COMMITS=60000`): 5,000 files in 100 directories, each commit changing two random files. The machine is the reference machine of `doc/benchmarks.md`, with the data directory on tmpfs.

| Query, 60,000 commits | Before | After | First query |
|---|---|---|---|
| `Log` of a path changed once, `limit` 20 | 11.8 s | 94 ms | 8.3 s |
| `MergeBase` at branch creation from the head | 563 ms | 1.3 µs | 0.83 s |
| `MergeBase` of branches 10,000 commits apart | 326 ms | 8.1 ms | — |

- "First query" starts from an empty graph. For merge-base, it computes the rows of the whole history. For `Log`, it computes the rows and every commit's path filter.
- Before the change, every query paid the full walk. The first `Log` on a history still costs 8.3 s once, which is less than the 11.8 s every query took before. Without the parallel prefetch of item 5, it took 28 s.

**golang/go.** The `master` branch of golang/go (68,110 commits) was pushed into a Git namespace on a server built from this change: 30 min 51 s, 2.5 GiB of packs. The same queries were timed through the native API, with the server before and after this change on the same data:

| Request, golang/go | Before | After | First request after |
|---|---|---|---|
| `GET /log/main?path=PATENTS&limit=20` (changed once) | 6.5 s | 0.9–1.0 s | 18.6 s |
| `GET /log/main?path=src/cmd/go/main.go&limit=20` | 0.75 s | 26–126 ms | 210 ms |
| Create a branch at the head | 0.8–2.0 s | 7–9 ms | 9 ms |
| Create a branch 1,000 commits behind the head | 0.8–2.0 s | 9 ms | 4.2 s |

- On real history, the first path query costs about three of the old queries. Every later query then costs a sixth or less.
- Real commits are larger to read than generated ones, so the rows cost 4.2 s for the first walk instead of 0.8 s.
- The trial found an unrelated import bug: golang/go has commits that list a parent twice, which F1 refused. It is fixed in the same change (doc/format.md, "Commit").

## Consequences

- **Cost becomes proportional to the answer.** `Log` with a path costs one in-memory row per commit, plus tree reads only for commits whose filter may contain the path. Branch creation no longer reads the whole history.
- **No format change.** The index is in `cache.db`, which is rebuildable (D7). Repositories copied to another server or restored from backup rebuild it on demand.
- **Space.** About 120 bytes per commit plus the filter: roughly 10 bits per changed path and parent directory, and at least 9 bytes. That is 15–30 MB for 100,000 commits, in `cache.db` and in the memory of a server that has queried that history.
- **Write path unchanged.** Rows are computed by queries, not by commits, so commit latency and fsync counts do not change.
- **Tests:**
  - a property test where `Log` gives the same results as the walk it replaces, and `MergeBase` a best common ancestor, on random histories with merges, unrelated histories, and history added after rows were computed;
  - GC removing commits that have rows, and the rows being dropped on the next load;
  - rows persisting across a reopen;
  - the version 3 → 4 upgrade.
- **Later uses.** Git fetch negotiation and `allow-reachable-sha1-in-want` checks can use generation numbers too. Not in scope here.

## Alternatives considered

- **Compute rows at commit time.** This keeps the index always complete, but adds a tree diff and a `cache.db` write to every commit and to every commit imported by Git push. Lazy computation keeps the write path as measured in phase 9. Rejected, but it can be added later for the native commit path, where the changed paths are known for free.
- **A table of (commit, path) rows instead of Bloom filters.** This is exact, but it grows with every changed path, and large commits (imports, S3 batches) make it big. Rejected.
- **Store the index in the packs.** This would make it part of the permanent format, for data that is derivable. Rejected (D7).
- **Do nothing.** This is reasonable if the benchmark in item 1 stays under 1 s. That is why item 1 comes first.
