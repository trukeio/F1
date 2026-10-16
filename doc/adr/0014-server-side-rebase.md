# ADR-0014: Server-side rebase of non-overlapping writes

- Status: accepted
- Date: 2026-10-16
- Deciders: Rolf Veen

## Context

A native write names the commit it is based on (`F1-Parent`, or `parent` in `POST /commits`), and the ref update is a compare-and-swap on that commit (D8). If any other write lands first, the write fails with `409 conflict`. The writer must then read the new head, or run `f1 pull`, and try again, even when the two writes touched unrelated files.

With a few people this is rare. With many automated writers on one branch, such as agents, CI jobs or scripts, most writes fail at least once, and each retry costs a round trip and, for the native client, a pull. Anthropic's write-up of its source control system (Scion, "Redesigning source control at Anthropic", 2026) reaches the same conclusion at a larger scale: a global lock or a strict queue on the branch head does not scale with automated writers. Concurrent changes should land, and only real overlaps should conflict.

Subversion has always behaved this way: a commit fails only if one of its files is out of date. F1's SVN-style namespaces target the same users.

F1 already does this in one place. In an autocommit namespace, a write without `F1-Parent` goes through the group committer, which applies it to whatever the head is (ADR-0012).

D10 says: "Merge: client-side only. The server never merges." This ADR keeps the intent of D10, that the server never combines two versions of a file's content, but it does make the server build a commit on a head the writer has not seen. D10 is a fixed decision, so this needs the user's approval.

## Decision

1. **Opt-in per request.**
   - `PUT` and `DELETE /ns/{ns}/files/{ref}/{path}` accept the header `F1-Rebase: true`.
   - `POST /ns/{ns}/commits` accepts `"rebase": true`.
   - The option requires a parent. Without it, behaviour is unchanged: strict compare-and-swap.
2. **The rule.** The request's paths are:
   - the path of each put and delete;
   - both `from` and `path` of each rename.

   A write based on parent `P`, when the ref is at head `H ≠ P`, is applied to `H` if, for every request path, the entry at `P` and the entry at `H` are equal: same kind, target and attrs, or absent from both. Otherwise the write fails with the usual `409 conflict`.
   - In tree layout, a directory entry's target is its subtree hash. A request path that names a directory therefore conflicts with any change below it.
   - Each parent directory of a request path must also be unchanged in kind: present as a directory at `H`, or of the same kind (or absent) at `P` and `H`. A directory deleted or replaced by a file since `P` is a conflict. A directory that appeared since `P` is not, so writers adding files to a new directory do not conflict.
   - The check reads two entries per request path and per parent directory of it: O(paths × depth × log n). It does not depend on how far `H` is ahead of `P`, and it does not need `P` to be an ancestor of `H`.
3. **Equivalence.** For every path, the resulting tree holds:
   - the request's value, for the paths the request changes;
   - `H`'s value, for every other path.

   Because the request's paths are unchanged between `P` and `H`, this is exactly what the writer would get by reading `H` and repeating the write. No file is changed by both sides, and no content is merged. The native client's `pull` already resolves such one-sided changes without reading content (ADR-0010, item 11).
4. **The commit.**
   - It has the single parent `H`, the request's message, author and time, and is a normal commit.
   - The ref update is a compare-and-swap from `H`. If another write lands in between, the server re-reads the head and repeats from step 2, up to 8 times, then returns `409 conflict`.
   - The `refs.log` reason is `commit (rebased)`.
5. **Checks.** Locks and write ACLs are checked as today, on the diff between the old and new value of the ref. That diff is exactly the request's paths. A lock taken on a request path after `P` therefore still blocks the write.
6. **Responses.**
   - `CommitResult` gains `parent`: the commit the new commit was built on. A client compares it with the parent it sent to know whether the write was rebased.
   - A refused rebase returns `409 conflict` with `current` as today, plus `paths`: the request paths that changed between `P` and `H`.
   - `pkg/f1client` gains `PutOptions.Rebase`, `CommitRequest.Rebase`, `CommitResult.Parent` and `Error.Paths`.
7. **Native client.**
   - `f1 push` rebases by default, which matches Subversion: it pushes local commits in order, each with the previous pushed commit as its parent, and the first with the base commit.
   - If any push was rebased, the client then updates the working copy to the new head as `pull` does. No local commits remain, so only working files change: paths the client pushed are equal on both sides, and paths others changed are taken from the server or merged with uncommitted edits as in a pull.
   - A refused rebase reports the conflicting paths and asks for `f1 pull`, as today.
   - `f1 push --no-rebase` keeps strict compare-and-swap.
8. **Unchanged.**
   - Git push: Git clients expect non-fast-forward pushes to be rejected, and commit SHAs are theirs.
   - S3: S3 writes are last-writer-wins through the group committer already.
   - The group committer.
   - A parent of `new`: ref creation, which cannot be rebased.
9. **Retention.** Rebasing needs the tree of `P`. If GC has removed it (retention other than `all`), the write is a conflict as today (ADR-0011). Namespaces with truncating retention are expected to use autocommit.
10. **D10 is reworded** as: "Client-side only. The server never merges file content. A write may opt in to being rebased by the server when no path it changes was changed since its parent (ADR-0014)."

## Consequences

- Concurrent writers to one branch conflict only when they touch the same paths. Agents and scripts can write through the API without a pull-and-retry loop.
- No format change. Commits, trees, refs and `refs.log` are as before. Older clients are unaffected: the option is off unless requested.
- **Semantic conflicts are possible.** Two writes that are each correct may together be wrong, for example renaming a function in one file while another write adds a call to the old name. This is the trade-off Subversion and trunk-based development with post-merge testing make. It is why the API option is opt-in, and why `f1 push --no-rebase` exists. The resulting history is linear and every commit is a real state of the branch, so the pair can be found and reverted.
- Users of `f1 push` no longer see "run f1 pull" when others changed unrelated files. They do get those files in the working copy after the push.
- Write cost: a rebased write reads two entries per path and parent directory, and applies its changes to `H`'s tree. This is the same as a commit on `H`.
- Tests:
  - the rule on files, directories, renames, ancestors replaced by files, and attrs-only changes, in both layouts;
  - a concurrent stress test, where N writers on disjoint paths all succeed and writers on one path get exactly one success per round;
  - lock and ACL checks on rebased writes;
  - `f1 push` end to end, with and without `--no-rebase`.

## Alternatives considered

- **Always rebase.** This changes the meaning of `F1-Parent` for existing callers, which rely on compare-and-swap to detect any concurrent change. Rejected.
- **Rebase in the client only.** The client could retry with pull and push. This already works, but each round costs a pull, and with many writers the retries can repeat indefinitely. The server checks and applies the change in one step under the ref update. Rejected as the only mechanism; `f1 push --no-rebase` keeps it available.
- **Server-side content merge** (three-way text merge when both sides changed a file). This would contradict the intent of D10, cost server CPU and memory in the write path, and create file contents no client produced. Rejected.
- **Conflict detection by diff of `P` and `H`.** This is equivalent for files, but costs O(changes since `P`), which grows with staleness, and is coarser for directories. Rejected in favour of comparing the request's paths.
- **A merge queue or a branch lock.** This serializes writers globally, which is the problem being solved. Rejected.
