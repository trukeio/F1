# Benchmarks

The benchmarks of phase 9 (doc/plan.md) are in `test/bench_test.go`. Each runs against an in-process server whose data directory is a temporary directory, through the public interfaces:

| Benchmark | What it measures |
|---|---|
| `BenchmarkIngest` | Native Put of 64 MiB files: chunking, hashing, compression, digests, pack writes and the commit |
| `BenchmarkSmallPut` | 1 KiB Puts from 24 concurrent clients into an autocommit namespace (group commit), over a fixed set of 1,000 keys |
| `BenchmarkS3GetLatency` | S3 GetObject of a 4 KiB object with the AWS SDK: SigV4, lookup, read |
| `BenchmarkGetLarge` | Sequential native Get of a 64 MiB file |
| `BenchmarkGitClone` | `git clone --bare` of a fixture with 300 commits over 400 Go source files; `pack-MiB` is the received pack |

## Running and checking for regressions

- `make bench` runs every benchmark five times (`-count 5`, `-benchtime 3s`) and writes `bench.txt`.
- `make bench-check` runs them and compares `bench.txt` with `testdata/bench/baseline.txt`, using `scripts/benchcmp`:
  - it takes the median of the five runs per metric;
  - it fails if any metric is more than 10 % worse.
- Lower is better for `ns/op` and sizes; higher is better for `MB/s` and `*/s`.

Results depend on the machine. Compare only runs on the same hardware, and re-record the baseline when the reference machine changes:

```
go test ./test -run '^$' -bench . -benchtime 3s -count 5 > testdata/bench/baseline.txt
```

Check before each release that no metric regressed by more than 10 % against the baseline of the previous release.

## Reference results

The reference machine is an Intel Core i5-9400 (6 cores, 2.9 GHz) with 32 GiB of RAM, Linux, Go 1.25.6. The data directory is on tmpfs, so these numbers measure CPU and locking, not disk.

Each value is the median of five runs. Phase 8 is commit f0bdfcd; phase 9 is the baseline in `testdata/bench/baseline.txt`.

| Benchmark | Phase 8 | Phase 9 | Change |
|---|---|---|---|
| Ingest, 64 MiB files | 421 MB/s | 435 MB/s | +3 % |
| Small Puts, 24 clients | 664 /s | 2,657 /s | ×4.0 |
| S3 GET latency, 4 KiB | 659 µs | 472 µs | −28 % |
| Sequential read, 64 MiB | 555 MB/s | 1,628 MB/s | ×2.9 |
| `git clone`, 300 commits | 608 ms, 1.50 MiB pack | 529 ms, 0.43 MiB pack | −13 % time, −71 % size |

What changed (ADR-0012):

- **Small Puts:**
  - group commit no longer waits 20 ms after the first request of a batch;
  - SQLite connections are pooled instead of reopened;
  - an in-memory existence filter answers most existence checks without SQLite.
- **S3 GET:** pooled connections and a read cache of recent records.
- **Sequential reads:** read-ahead of 4 chunks for streaming readers.
- **Git clone:** fetch packs carry deltas against the previous version of each path.

### Variance

The small Put rate varies most between runs on the reference machine: from about 1,700 to 3,450 per second in one series. That machine's memory is under pressure (its swap is full), and the ingest benchmark that runs before it writes over a gigabyte to tmpfs. Medians of five runs absorb most of this, but a single run says little.

## History queries

The history benchmarks of ADR-0015 are in `internal/repo/history_bench_test.go`. They run on a generated history: 5,000 files in 100 directories, each commit changing two random files. `F1_BENCH_COMMITS` sets the number of commits (2,000 by default, so that `go test -bench .` stays quick):

```
F1_BENCH_COMMITS=60000 go test ./internal/repo -run '^$' -bench 'LogRare|MergeBase' -benchtime 5x -timeout 30m
```

`cold-ms` is the first query on an empty commit graph, which computes the index. Results on the reference machine, 60,000 commits:

| Benchmark | Before ADR-0015 | After | `cold-ms` |
|---|---|---|---|
| `LogRarePath`: log of a path changed once, `limit` 20 | 11.8 s | 94 ms | 8,266 |
| `MergeBaseNewBranch`: merge base when a branch is created at the head | 563 ms | 1.3 µs | 834 |
| `MergeBaseApart`: merge base of branches 10,000 commits apart | 326 ms | 8.1 ms | |

These benchmarks are not in `make bench`: building the 60,000-commit history takes about two minutes. ADR-0015 also records the same queries on golang/go (68,110 commits) through the native API: a path log of a file changed once went from 6.5 s to about 1 s after an 18.6 s first query, and branch creation from 0.8–2 s to under 10 ms.

## Web site files

`BenchmarkSite` (`test/site_bench_test.go`) measures serving a web site's small files from F1's native API (`GET /api/v1/ns/{ns}/files/{ref}/{path}`) against Go's `http.FileServer`. Both serve the same files from the same tmpfs, and the client is a plain `net/http` client. Each size has 100 files of random (incompressible) content, read in turn. It is not in `make bench`:

```
F1_BENCH_SITE=1 go test ./test -run '^$' -bench Site -benchtime 2s -count 5 -timeout 60m
```

What it measures:

- **branch:** the file read by branch name (`main`). These responses are revalidated on every use.
- **commit:** the file read by commit hash, cacheable for a year (ADR-0016).
- **304:** a conditional GET that revalidates a cached copy (`If-None-Match` for F1, `If-Modified-Since` for the file server).
- **cold:** each file read once from a server that was just restarted, so its read cache is empty.
- **Serial** is one client, waiting for each response. **Parallel** is 24 concurrent clients.

The results are medians of five runs on the reference machine. Client and servers share its 6 cores over loopback.

| Size | Source | Serial latency | Parallel rate | Parallel throughput |
|---|---|---|---|---|
| 10 kB | file server | 80 µs | 67,300 /s | 673 MB/s |
| | F1, branch | 312 µs | 18,400 /s | 184 MB/s |
| | F1, commit | 319 µs | 17,900 /s | 179 MB/s |
| | F1, branch, cold | 457 µs | | |
| 100 kB | file server | 97 µs | 51,500 /s | 5,150 MB/s |
| | F1, branch | 447 µs | 10,200 /s | 1,020 MB/s |
| | F1, commit | 455 µs | 9,980 /s | 998 MB/s |
| | F1, branch, cold | 556 µs | | |
| 500 kB | file server | 203 µs | 24,400 /s | 12,200 MB/s |
| | F1, branch | 739 µs | 3,290 /s | 1,650 MB/s |
| | F1, commit | 747 µs | 3,270 /s | 1,640 MB/s |
| | F1, branch, cold | 841 µs | | |
| any | file server, 304 | 65 µs | 91,000 /s | |
| | F1, 304 | 270–290 µs | 23,800 /s | |

F1 takes 3.6–4.6 times as long per file as the file server. With 24 clients it serves 3.7 times fewer requests for 10 kB files and 7.4 times fewer for 500 kB files.

- **A fixed cost per request.** A 304, which reads no content, costs about 200 µs more in F1 than in the file server.
  - About 40 % of F1's CPU time per request goes to four SQLite queries: token, principal, local groups and namespace ACL.
  - Most of that time is spent parsing the SQL text again for every query.
  - Caching verified tokens and ACLs in memory, or keeping the statements prepared, would remove most of this cost.
- **A cost per byte.** Above the fixed cost, F1 streams about 1.1 GB/s per request: it reads and decompresses records. The file server streams about 4 GB/s.
- **Reads by name and by commit cost the same.** A read by commit hash saves the revalidation request entirely, because the client can keep its copy for a year.
- **Cold reads** add about 100–150 µs per file.

What serving a site from F1 needs, beyond speed:

- **No anonymous reads.** Every request needs a token (native) or a signature (S3). A public site needs a proxy in front of F1 that adds the credential.
- **Shared caches do not store F1's responses.** They are `Cache-Control: private` (ADR-0016), because a response depends on the caller's permissions. A caching proxy must be configured to override this.
- **Content types are not guessed.** A file is served as `application/octet-stream` unless its `Content-Type` was set when it was written. The file server derives the type from the file name extension.

## Other measurements

These are acceptance tests rather than benchmarks; see their ADRs.

- **Size transparency** (`test/sizes_test.go`, D19): peak server memory and client heap for files from 0 B to 10 GiB through every protocol. The phase 7 and phase 8 results are in ADR-0010 and ADR-0012.
- **Garbage collection** (`internal/gc`, ADR-0011): 1,000,000 overwrites of 1,000 keys leave 486 KiB of packs for 309 KiB of live data. The collection deletes 3 million objects in about a minute.
- **Sparse checkout** (ADR-0010): cloning one subdirectory of a 50 GB repository transfers the subtree plus 1,659 bytes of metadata.
- **Storage of code histories** (ADR-0013): F1 needs 2.4–6× the space of an aggressively packed Git repository.
