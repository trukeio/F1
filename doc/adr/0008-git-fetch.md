# ADR-0008: Git fetch

- Status: accepted
- Date: 2026-10-13
- Deciders: Rolf Veen

## Context

Phase 5 serves Git fetch (smart HTTP upload-pack) from F1 namespaces (doc/plan.md, "Git compatibility"). The plan fixes the object mapping, the protocol scope and the acceptance tests. This ADR records how they are implemented and where the implementation departs from the plan.

## Decision

### Code and dependencies

1. **No go-git for fetch.** The plan lists go-git plumbing for pack and pkt-line parsing. Fetch needs only these, which F1 implements directly in `internal/api/git` in a few hundred lines, with fuzz tests:
   - pkt-lines;
   - a pack writer without deltas;
   - parsing and encoding of tree, commit and tag objects.
   Pack parsing with delta resolution (push) is decided in phase 6, with bounded memory as the requirement.
2. **The importer is production code.** `git.Importer` converts Git objects to F1 objects. The test-only importer (`internal/gittest`) only reads fixture repositories with `git cat-file` and calls it, so phase 6 (push) uses the same conversion.

### Object mapping

3. **Round-trip by construction.** Every imported tree, commit and tag is exported again, and its Git id must equal the original.
   - Trees that do not re-encode canonically (modes such as `100664`, zero-padded modes, unusual order) keep their bytes in a GitRaw object.
   - Commits and tags that do not re-encode keep their bytes in `git_raw_hint`. Examples: header order other than tree, parents, author, committer, extra headers; or idents that do not re-format byte for byte.
   - Export uses raw bytes only after checking that they reference the same tree, parents or entries.
4. **Tags of non-canonical trees are refused.** A tag that points to a tree kept as GitRaw is rejected by the importer, because such a tag cannot be exported reliably. Tags of trees are rare, and tags of non-canonical trees rarer still.
5. **`git_map`** is a table in `cache.db` (schema version 3, upgraded in place from versions 1 and 2). Its key is an F1 commit, tree or tag hash, or a GitRaw hash for raw trees. Blob ids are not in it: they come from DigestRecords (D21). Many F1 objects may share one Git id.
6. **Rebuild on demand only.** The plan describes a background rebuild of `git_map` after a cache rebuild. Version 1 computes entries on demand, at the same cost, and memoizes them. The first fetch after a rebuild walks the history it advertises. A test asserts that this reads no chunk payload.

### Protocol

7. **Protocol versions.** v2 serves `ls-refs` (symrefs, peel, ref-prefix, unborn), `fetch` and `object-info`. v0 and v1 serve the stateless-RPC exchange with `multi_ack_detailed`.
8. **Negotiation.** In v2, a fetch without `done` is answered with the acknowledgments of the known haves, `ready`, and the pack in the same response. The server can always send a pack; unknown haves only make it larger. In v0, a request without `done` gets `ACK … common`, `ACK … ready` and `NAK`; the client then sends `done`. A first request of a shallow fetch (wants and depth only) gets the shallow list alone, as git-upload-pack does.
9. **What is sent.** The objects reachable from the wants, minus:
   - commits reachable from the common haves (not past the client's shallow commits);
   - the trees and blobs of the have commits themselves.
   Older trees the client may have are sent again: the pack is larger, never wrong.
   Packs have no deltas (phase 9 adds them). Objects are zlib-compressed at the fastest level, and blobs are streamed from the store, so memory is bounded for every size (D19).
10. **Shallow.** `deepen`, `deepen-since` and `deepen-relative` are supported; `deepen-not` is refused.
11. **Filters.** The partial clone filters are `blob:none`, `blob:limit=<n>` and `tree:0`; other filters are refused. Explicitly wanted objects are always sent, which is how lazy fetches work.
12. **Wants by id.** Any object of the namespace can be wanted by id, which amounts to `allow-any-sha1-in-want`. Git access already needs whole-namespace read (D13), so this reveals nothing more.
13. **Authentication and access.**
    - Credentials are HTTP Basic with a token as the password (any user name), or a bearer token.
    - Namespaces without `git = true`, unknown namespaces and inaccessible namespaces all answer 404.
    - Partial readers get 403 (D13).
    - Push answers 403 until phase 6.

### Limitations

14. **LFS pointer blobs can't be fetched lazily yet.** A lazy fetch of an LFS pointer blob by id is not supported, because pointer bytes are not addressable on their own. LFS arrives with push in phase 6, which will make pointer blobs addressable. (Resolved in phase 6: pointer blobs are stored as Files, ADR-0009.)

## Consequences

- Fixture repositories round-trip exactly over protocols v2, v1 and v0. They cover merges, an octopus merge, signed commits and tags, non-UTF-8 names and messages, executables, symlinks, submodules, an empty tree, unusual time zones and a non-canonical tree.
- Clones from F1 pass `git fsck --strict`.
- Native and S3 writes appear in Git with deterministic ids.
- Packs are larger than Git's until deltas are added in phase 9.

## Alternatives considered

- **go-git for packs and pkt-lines.** It would add a large dependency tree for code F1 needs only a small part of, and its pack writer buffers objects; streaming large blobs (D19) is simpler in F1's own writer.
- **A background `git_map` rebuild.** It adds a job and its coordination for no correctness gain; it can be added if first fetches after a rebuild prove slow.
