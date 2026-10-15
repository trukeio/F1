# ADR-0011: Retention and garbage collection

- Status: accepted
- Date: 2026-10-15
- Deciders: Rolf Veen

## Context

Phase 8 implements retention policies and garbage collection (doc/plan.md, "Retention and garbage collection"). The plan fixes mark and sweep with a grace period, a touched set and a condemned-set check, and the safety rules. This ADR records how they are implemented, and where the implementation completes or departs from the plan.

## Decision

### Cooperation with writers

1. **The store is the only meeting point.** Every existence check (dedup during ingest, D9 checks, `objects/missing`, ref targets) goes through `Store.Has`. The collector (`internal/gc`) talks to writers only through the store:
   - **Epoch.** `BeginGC` starts an epoch. From then until `Condemn`, every object `Has` reports stored, and every object `Put` writes, is recorded in the touched set.
   - **Condemn.** `Condemn` computes the condemned set: unmarked objects of old packs, minus everything the touched objects reference, transitively. It then publishes the set.
   - **Fresh copies.** From publication on, `Has` reports condemned objects as not stored, so a writer that needs one stores a fresh copy. Sealing a pack that holds a fresh copy moves the object's index row to the new pack (`AddPacksReplacing`) and removes the object from the condemned set.
2. **Checks and writes are atomic with the epoch's steps.**
   - `Has` and `Put` hold the GC lock shared; `BeginGC` and `Condemn` take it exclusively.
   - A ref update holds it from the check of its new targets to the write of `refs` (`Store.Guard`, `HasGuarded`).
   - Every check therefore falls entirely before `BeginGC`, between `BeginGC` and `Condemn`, or after `Condemn`. A reference checked before the epoch belongs to an object that is either in the open pack when the collector lists it, or written in a pack the collector lists after it.
   - Lock order is: GC lock, then the store mutex, then two leaf locks (condemned set, touched map). `Condemn` reads objects but never calls `Has`.
3. **New objects are never condemned.** An object written during the epoch is touched, whatever the age of the pack it lands in. A pack opened before the cutoff may still be receiving objects.
4. **Holds.**
   - A frozen or held repository (freeze, archive, backup) is skipped when the collection starts.
   - `Condemn` takes the maintenance lock and fails if a hold is active. The lock is kept until the sweep ends, so consolidation does not interleave with the sweep, and `Hold` waits for the sweep to end.
   - An archive started during a collection therefore lists the packs after the sweep, and is valid.

### Marking

5. **Roots.**
   - The objects of the open pack (listed first).
   - Every object of packs created within the grace period.
   - The parts of open multipart uploads.
   - The commits the retention policy keeps.
   - Whatever the refs point to (tags).
   - Objects are marked by hash. Chunks are never read: their type is known from the File or FileIndex that references them. Commit parents are not followed; retention decides which commits are kept.
6. **Pack age** is the time in the pack's ULID name: when the pack was opened. Packs written by consolidation or a GC rewrite are therefore young, and their objects are protected for another grace period. This is conservative.
7. **Retention.**
   - `all` keeps every ancestor of every ref.
   - `last:N` keeps the first N commits of each ref's first-parent history.
   - `days:N` keeps the head, plus first-parent commits with a committer time within N days of the collection's start.
   - `none` keeps the heads.
   - Tags are followed to their commits. A missing commit ends a walk; it is the boundary of truncated history.
8. **DigestRecords live with their Files.** Marking a File marks the record that its `digests` row describes, by computing that record's hash. Records are never read during a collection.
9. **Client upload leases are not needed.** The plan lists "active client upload leases" among the roots. The native client's uploads are covered by three mechanisms instead:
   - The grace period: uploaded objects are new.
   - The touched set: `objects/missing` answers are `Has` calls.
   - The push retry of ADR-0010, decision 10: a commit whose objects were collected after all re-uploads them.

### Sweeping

10. **Per-pack plan.**
    - A pack without live objects is deleted.
    - A pack less than half live, by stored bytes, is rewritten with its live objects into new packs.
    - Other packs keep their condemned objects until a later collection. This bounds storage to about twice the live data.
11. **D9 for every stored object.** A condemned object left in a kept pack stays stored, so everything it references must stay stored too.
    - Rescuing those references would let a small garbage object pin large garbage: a dead DigestRecord would keep a dead File and all its chunks.
    - Instead, a kept pack whose garbage references objects being removed is rewritten as well, dropping that garbage. Removing more never requires keeping more, so the planning converges.
    - fsck checks D9 over all stored objects, garbage included.
12. **Index and files.**
    - In one transaction, the rewritten packs are indexed and take over their objects' rows. The rows still pointing at swept packs are removed, along with the swept packs themselves. Fresh copies have moved their rows elsewhere and keep them.
    - Swept pack files are deleted after `DeleteGrace` (default 1 h) through the existing pending-delete table, so in-flight reads complete.
    - Consolidation of small packs runs afterwards, under the same lock.
13. **Metadata.** `digests` and `git_map` rows of objects no longer stored are removed.
14. **Dry run.** A dry run computes the same plan without publishing the condemned set or changing anything. With no writes in between, it reports exactly what the next collection deletes; tests compare the object lists.

### Operation

15. **Scheduling.**
    - Settings: `[server.gc] interval` (default `24h`, `0` disables) and `grace` (default `24h`).
    - The server collects every repository in turn, once per interval.
    - `f1d gc <ns> [--dry-run] [--grace d] [--list]` runs a collection through the admin socket, or offline with `--dir` when no server holds the repository.
    - A second collection of the same repository while one runs is refused.
16. **fsck.**
    - Missing commit parents were already accepted only with truncating retention.
    - fsck now also warns when a ref's first-parent history is shorter than a `last:N` policy keeps. A stricter earlier policy explains this; otherwise history was lost.
    - A DigestRecord whose File is missing is a D9 error.
17. **Small-file ingest.** A file of known size up to 1 MiB is ingested in one goroutine without the concurrent pipeline, whose buffers cost more than such a file. The objects are identical, as tests check against the streaming chunker. This was needed for the million-write acceptance test, and helps S3 small-object writes.

## Consequences

- Each acceptance test is listed with its result.
  - Concurrent writers that reuse old content run during repeated collections with no grace period. No acknowledged write is lost, and fsck is clean. Writes whose own uncommitted objects were collected under them fail; they were never acknowledged.
  - A write during the pause between condemn and sweep stores fresh copies and stays readable.
  - A ref pointed at an unreachable commit during the epoch keeps that commit's whole tree.
  - fsck on a `last:N` namespace accepts the boundary parents and reports a missing tree.
  - Collections of frozen or held repositories are skipped, and an archive taken during a collection verifies.
  - With `retention = none`, 1,000,000 overwrites of 1,000 keys leave 486 KiB of packs for 309 KiB of live data. The collection deletes 3 million objects in about a minute.
  - Dry runs equal the real deletions.
- Existence checks take a shared lock; only `BeginGC` and `Condemn` take it exclusively, briefly.
- Retention `days:N` depends on commit times, which clients can set (Git). A commit dated in the future stays until that date plus N days.

## Alternatives considered

- **Reference counting.** Excluded by the plan; it is fragile under crashes and concurrent deduplication.
- **Pack age from file modification time.** Copies and archive extraction change it; the ULID travels with the pack.
- **Deleting only fully dead packs.** Simpler, but a pack with one live object would keep 64 MiB of garbage forever.
- **Tolerating dangling references among garbage** (D9 for reachable objects only). It would weaken fsck: a real loss of an object referenced by garbage would look the same as a deliberate deletion.
- **Rescuing what kept garbage references.** Simpler to plan, but garbage would pin other garbage of any size (decision 11).
