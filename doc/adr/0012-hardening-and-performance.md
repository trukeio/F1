# ADR-0012: Hardening and performance

- Status: accepted
- Date: 2026-10-15
- Deciders: Rolf Veen

## Context

Phase 9 (doc/plan.md) covers:
- delta compression in Git pack generation;
- read caches and prefetch;
- metrics, backup, admin documentation and an example deployment;
- documented benchmarks with a regression check;
- an evaluation of delta-encoded storage (ADR-0013, proposed).

Measurements during the phase found three bottlenecks the plan did not name: group commit latency, SQLite connection churn, and SQLite mutex contention on existence checks. This ADR records the decisions; `doc/benchmarks.md` has the numbers.

## Decision

### Git fetch deltas

1. **Path-based delta bases.**
   - The fetch walk records each tree's and blob's path. Newer versions come first, as in Git.
   - The pack writer keeps the last version written for each type and path in a cache of at most 32 MiB.
   - A tree or blob of at most 1 MiB is sent as a delta against the cached version of its path if the delta is at most half its size.
   - Delta chains are at most 50 deep. Larger blobs are streamed whole, as before (D19).
2. **Delta format.** Git's delta format, produced by F1's own encoder:
   - The base is indexed in 16-byte blocks, with at most 64 positions per block hash.
   - Matches are extended forwards and backwards.
   - The encoder is fuzzed against the decoder the push path uses.
3. **Base references.** Bases are always earlier in the same pack: `ofs-delta` when the client negotiated it (protocol v2 argument or v0 capability), `ref-delta` otherwise. Thin packs against the client's objects are not sent.

### Write throughput

4. **Group commit commits as soon as the queue is empty.**
   - The committer used to wait the full 20 ms window after a batch's first request.
   - It now takes the queued requests, plus those arriving less than 1 ms apart, for at most 20 ms and 1000 changes.
   - Requests that arrive while a batch commits form the next batch. A lone writer no longer pays 20 ms; concurrent writers still share commits.
5. **SQLite connection pools.** `server.db` and every `cache.db` keep up to 32 connections open, closing idle ones after 5 minutes. With Go's default of two idle connections, concurrent requests opened and closed connections constantly; each opening cost its pragmas and a schema parse, about 30 % of the CPU in the small-write benchmark.
6. **Existence filter.** Existence checks (`Store.Has`) serialized on SQLite's mutexes. Two in-memory structures now answer most of them without a query:
   - **A Bloom filter of every indexed hash** (12 bits per key, 8 hash functions from the BLAKE3 hash bytes) answers "not indexed" for new content with certainty.
     - It is built in the background when the store opens; checks query `cache.db` until it is ready.
     - Every seal extends it, under the store mutex, so a check always finds an object in the open pack or in the filter.
     - It is rebuilt when it outgrows its size, and after every GC sweep, whose deletions it cannot remove.
   - **A set of up to 65,536 hashes recently confirmed indexed** answers repeated checks of existing objects; tree updates check every entry of every changed node. A sweep clears it.

   Both describe `cache.db` only; the GC's condemned set still applies on top (ADR-0011).

### Reads

7. **Read cache.**
   - `Store.Get` keeps recently read records of at most 64 KiB in an LRU of 64 MiB (`store.Options.ReadCache`); larger records are big files' chunks streamed once. This covers Files, FileIndexes, tree nodes and small files' chunks.
   - Records are immutable, so entries never go stale. A sweep clears the cache anyway, so that removed objects are not served.
   - Cached bytes are shared and read-only; their capacity is clamped so that an append cannot write into them.
8. **Sequential read-ahead.**
   - A content reader whose last two `ReadAt` calls each started where the previous one ended fetches the next 4 chunks of the current FileIndex node in the background, and uses them in order.
   - A single read never reads ahead, whatever its size, so random access still fetches only the chunks it overlaps.
   - Memory is bounded by 4 chunks per reader.

### Operations

9. **Metrics.**
   - `/metrics` on the main listener is written in the Prometheus text format by `internal/metrics`, without a client library.
   - It counts requests, durations and bytes per API (native, S3, Git), ref updates, pack writes and GC results. Storage gauges are computed from the pack indexes at scrape time.
   - It is unauthenticated and enabled by default (`[server.metrics] enabled`).
   - Per-namespace gauges are off by default (`namespaces = false`), because their labels reveal namespace names.
   - Each server has its own registry for its repositories' gauges; process-wide counters live in a default registry.
10. **Backup** (`f1d backup <dir>`): an online copy of `server.db` (`VACUUM INTO`), then for each repository, under a store hold:
    - copy `f1repo.toml`, `refs`, `refs.log` and `locks` into memory;
    - copy the live packs the target lacks;
    - replace the target's text files;
    - remove target packs that are no longer live.

    An interrupted backup is consistent: the target's refs change only after the packs they need are there. The backup is a data directory without `cache.db`, so restoring means starting f1d on a copy of it, or `f1d repo import` per repository. It works through the admin socket, or offline.
11. **Deployment.**
    - `deploy/f1d.service`: a hardened systemd unit (state directory, `ProtectSystem=strict`, no capabilities).
    - `deploy/f1d.toml`.
    - `deploy/Dockerfile`: a static build on `golang:1.25.6-bookworm`, an `alpine:3.22.1` runtime, a non-root user, and a volume at `/var/lib/f1`.

    `doc/admin.md` is the administration guide.

### Benchmarks

12. **The benchmarks** are `test/bench_test.go`, against an in-process server on a temporary directory:
    - ingest of 64 MiB files;
    - 1 KiB Puts from 24 concurrent clients to an autocommit namespace, over a fixed set of 1,000 keys;
    - S3 GET latency of a 4 KiB object;
    - sequential reads of a 64 MiB file;
    - `git clone` of a 300-commit, 400-file fixture.

    `make bench` runs each five times. `make bench-check` compares the medians with `testdata/bench/baseline.txt` (`scripts/benchcmp`) and fails if any metric is more than 10 % worse. Results depend on the hardware, so the check compares runs on the same machine. The baseline is re-recorded when the reference machine changes, and before each release.

### Memory

13. **zstd window of 1 MiB.** The process-wide zstd encoder used the default 8 MiB window. It kept a 16 MiB history buffer per concurrent encoding: about 100 MiB in every f1d process with 6 cores, more with more cores.
    - Chunks are at most 256 KiB and other objects are small, so a 1 MiB window compresses them identically.
    - The heap after a server stops dropped from 112 MiB to 21 MiB, and ingest speed is unchanged.
    - Compressed bytes are not part of object identity, and the golden pack vector holds only uncompressed records, so this is not a format change.

### Findings without code changes

14. **Client memory.** Peak heap grows from 32 MiB at 64 MiB files to 233 MiB at 1 GiB and 306 MiB at 10 GiB, below the 512 MiB bound. Heap profiles at the peak show 35–45 MiB live: the growth is garbage awaiting collection while gigabytes stream through, not retained state.

    The test's own text generator allocated 96 % of the bytes; it was rewritten to allocate nothing. Data structures per 64 KiB chunk exist (chunk lists of the merge diff, upload lists, the local pack's index) and cost about 1 KiB per MiB of file. A 100 GiB file would need about 100 MiB more, still within the bound.

## Consequences

- Small writes are about four times faster (`doc/benchmarks.md`).
- S3 GET latency is lower; large reads are faster; Git clones of code histories are much smaller.
- Existence checks can answer "stored" for an object deleted by a sweep only while the condemned set hides it, so GC semantics are unchanged.
- The Bloom filter costs about 1.5 bytes per object (12 bits): 15 MiB for 10 million objects, rebuilt in the background on open.
- Metrics, backup and deployment complete the operational surface of version 1.

## Alternatives considered

- **Caching authentication and ACL lookups.** It would save about 20 % of an S3 GET, but delay token revocation and ACL changes. Not done.
- **Prometheus client library.** Not needed for a few dozen series; writing the format directly keeps the dependency list short.
- **Deltas against the client's objects (thin fetch packs).** These would make incremental fetches smaller. The walk would have to keep the client's objects as bases, which needs their content; left for later.
- **Read-ahead in the HTTP layer.** The content reader knows the chunk layout, so it fetches exactly the next chunks.
