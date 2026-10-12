# ADR-0001: Fixed decisions

- Status: accepted
- Date: 2026-10-12
- Deciders: Rolf Veen

## Context

`doc/plan.md` lists decisions D1–D21 that are settled before implementation starts. This ADR records them as the baseline, so that any later change is made by a new ADR that supersedes the affected entry. The plan remains the detailed specification; where this ADR and the plan disagree, the plan as of this date is authoritative and the discrepancy must be fixed by an ADR.

## Decision

| # | Decision | Value |
|---|---|---|
| D1 | Object hash | BLAKE3, 32 bytes |
| D2 | Chunking | FastCDC, min 16 KiB, avg 64 KiB, max 256 KiB, fixed gear table committed in source |
| D3 | Chunk hash input | Uncompressed chunk bytes |
| D4 | Chunk compression | zstd per chunk, stored compressed only if it saves at least 5 % |
| D5 | Object encoding | Canonical CBOR (RFC 8949 §4.2) with a 1-byte type tag; object hash = BLAKE3(type tag ‖ CBOR bytes) |
| D6 | Physical storage | One directory per repository with its own immutable pack files (target 64 MiB); no sharing between repositories; local filesystem backend only in version 1 |
| D7 | Metadata | Per repository: `refs` file (source of truth) and rebuildable `cache.db`. Server level: `server.db`. Pure Go SQLite (`modernc.org/sqlite`) |
| D8 | Concurrency primitive | Compare-and-swap on refs, serialized per repository in the server process |
| D9 | Storage invariant | An object is stored only if all objects it references are stored; sole exception: parents of the oldest retained commits under truncating retention |
| D10 | Merge | Client-side only |
| D11 | Git protocol | Smart HTTP; protocol v2 for fetch, v0/v1 for push |
| D12 | Git namespaces | Require `layout = tree` and `retention = all` |
| D13 | Path ACLs | Hash-addressed access (Git, LFS, native object endpoints, `POST /commits`, native client) requires whole-namespace read; path read ACLs apply to path-addressed native endpoints and S3; write ACLs apply everywhere |
| D14 | Server config format | TOML |
| D15 | Module path | `github.com/trukeio/f1` (lowercase) |
| D16 | License | MIT, `LICENSE` at the root, no per-file headers |
| D17 | Authentication | Pluggable token verifiers: local tokens and external JWT issuers (JWKS) in version 1 |
| D18 | Repository portability | A repository directory holds everything needed to serve it except identities and ACLs; no deduplication across repositories |
| D19 | Size transparency | No behavior depends on file size; all paths stream with bounded memory |
| D20 | Prolly tree boundaries | Name-only boundary hash, average 64 entries, min 16, max 512 entries, max 128 KiB per node; interior levels mix the level into the hash |
| D21 | Durable external identities | Git SHA-1, SHA-256 and MD5 of each File stored in packs as DigestRecords; `cache.db` only indexes them |

## Consequences

- D1–D5, D20 and D21 define the on-disk format. They become permanent when phase 1 is merged; changing them afterwards breaks object identity and deduplication.
- All other open points in the plan are decided by further ADRs before they are implemented.

## Alternatives considered

Recorded in `doc/plan.md` and in the feasibility review that produced D20, D21 and the current D9 and D13.
