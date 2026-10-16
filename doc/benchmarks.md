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

## Other measurements

These are acceptance tests rather than benchmarks; see their ADRs.

- **Size transparency** (`test/sizes_test.go`, D19): peak server memory and client heap for files from 0 B to 10 GiB through every protocol. The phase 7 and phase 8 results are in ADR-0010 and ADR-0012.
- **Garbage collection** (`internal/gc`, ADR-0011): 1,000,000 overwrites of 1,000 keys leave 486 KiB of packs for 309 KiB of live data. The collection deletes 3 million objects in about a minute.
- **Sparse checkout** (ADR-0010): cloning one subdirectory of a 50 GB repository transfers the subtree plus 1,659 bytes of metadata.
- **Storage of code histories** (ADR-0013): F1 needs 2.4–6× the space of an aggressively packed Git repository.
