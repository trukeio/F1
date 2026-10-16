# ADR-0017: SVN wire protocol (evaluation)

- Status: accepted: not pursued in version 1, deferred indefinitely
- Date: 2026-10-16
- Deciders: Rolf Veen

## Context

The SVN wire protocol is a version 1 non-goal (`doc/plan.md`, "Non-goals"): SVN users move to the native client, Git or S3. `TODO.md` asks what serving `svn` clients directly would take, over ra_svn or WebDAV/DeltaV, and what it would bring to SVN-style namespaces.

SVN-style namespaces (`--preset svn`) are already built around Subversion's behaviour:

- required locks with read-only checkout, as `svn:needs-lock` (ADR-0006);
- commits that fail only when one of their files is out of date (ADR-0014);
- sparse checkout, large binaries and full history.

What they lack is the `svn` client itself, and the tools built on it: TortoiseSVN, IDE plugins, and the SVN integrations of EDA and CAD tools.

There is one precedent at scale. GitHub served `svn` clients from Git repositories from 2010, with `trunk` mapped to the default branch and `branches/` and `tags/` to refs. It removed the bridge on 2024-01-08 because of low use: fewer than 0.02 % of requests to its Git backend came through Subversion. It named sparse checkout and partial clone in Git as the replacements ("Sunsetting Subversion support", GitHub blog, 2023-01-20).

## Which protocol

An `svn` client reaches a server in one of three ways:

| Access | Server | Transport and authentication | Fit with F1 |
|---|---|---|---|
| `svn://` (ra_svn) | `svnserve` | Own TCP protocol on port 3690, no TLS. CRAM-MD5 or SASL | Poor. CRAM-MD5 needs the plaintext secret, but F1 stores only SHA-256 hashes of tokens and cannot check OIDC tokens this way. It also needs a third listener |
| `svn+ssh://` | `svnserve -t` over SSH | SSH keys | None. F1 has no SSH server and no SSH identities |
| `http(s)://` (ra_serf) | `mod_dav_svn` | WebDAV/DeltaV plus SVN extensions, over HTTP | Good. It uses the main listener, HTTP Basic with a token as the password (as Git does), TLS at the proxy and the same `Allow` checks |

**Only HTTP is worth considering.** The server would speak only the "HTTP v2" protocol of Subversion 1.7 and later: `OPTIONS` advertising `SVN-Me-Resource`, `!svn/rev`, `!svn/rvr`, `!svn/txn` and `!svn/txr` resources, and `POST` to create transactions. Pre-1.7 clients would not be served; 1.7 was released in 2011.

The server side of HTTP v2 consists of:

- **Discovery:** `OPTIONS` and `PROPFIND` with the DAV and SVN live properties: repository UUID, youngest revision, version names, checked-in hrefs and similar.
- **Reads:**
  - `GET` of a file at a revision, optionally as svndiff against a base revision;
  - `PROPFIND` for `svn ls`, `svn info` and properties.
- **`REPORT` variants:**
  - `update-report`: checkout, update, switch, status `-u` and diff against the repository;
  - `log-report`, `get-locations`, `get-location-segments`, `file-revs` (blame), `get-dated-rev`, `get-deleted-rev`, `get-locks`, `inherited-props`;
  - `replay` and `replay-range`, used by `svnsync` and `svnrdump`.
- **Writes:**
  - `POST` create-txn;
  - `PUT` of svndiff against the base, plus `DELETE`, `MKCOL`, `COPY` and `PROPPATCH` inside the transaction;
  - `MERGE` to commit, and `DELETE` of the transaction to abort.
- **Locks:** `LOCK` and `UNLOCK` with `opaquelocktoken:` tokens and comments, and lock tokens sent at commit.
- **Encodings:** svndiff versions 0–2 (version 0 is enough to send; all three must be read), and the XML editor drive inside `update-report` (`send-all` mode, or skeleton mode followed by one `GET` per file).

For scale: the Git layer is about 4,500 lines of Go plus 1,500 lines of tests, for two protocol services and one object mapping. HTTP v2 has more request types, a richer mapping (below), and two encodings to implement (XML editor drives and svndiff). An estimate is 7,000–10,000 lines with tests, plus the three cache indexes below.

## What the mapping needs

F1 serves the protocol only if the following can be answered from F1's model. Items 1–4 need new derived data or new stored data.

1. **Revision numbers.** SVN numbers revisions 0, 1, 2, … for the whole repository, and the numbers never change. F1 has commits per ref and no global order, except `refs.log`. That log is an append-only list of every ref update, copied with the repository (D18), and its `<time>` field is a natural `svn:date`.
   - Revision *n* is derived from the *n*-th update group in `refs.log`.
   - A fast-forward of one branch by several commits, such as a Git push or a native push of local commits, expands into one revision per new first-parent commit. Each commit message stays visible in `svn log`.
   - A table `svn_revs(rev, tx, ref, commit)` in `cache.db` holds the result and is rebuilt by reading `refs.log` (D7). The repository UUID is the one in `f1repo.toml`.
   - **Revisions must never become unreadable.** GC today keeps only what refs reach, so deleting a branch (`svn rm ^/branches/x`, which SVN users expect to undo with a peg revision) or rewinding a ref would make old revisions disappear. A namespace with SVN access would need every commit named in `refs.log` as a GC root. That is a change to GC (ADR-0011), and SVN access would require `retention = all`, as Git access does (D12).
2. **Repository layout.** Two mappings are possible:
   - **Branches as directories:** `trunk/` = `default_ref`, `branches/<x>/` = `heads/x`, `tags/<x>/` = `tags/x`. Creating a branch with `svn copy ^/trunk ^/branches/x` becomes a ref creation, and the copy source comes from the revision table, so branch history survives `--stop-on-copy`. A commit that changes several branches becomes one multi-ref update, which the core supports (atomic push).
   - **One ref as the root:** the repository root is `default_ref`, and other refs are invisible.

   GitHub used the first. It is also what migrated users expect.
3. **Last-changed revision of every node.** The `update-report` and `PROPFIND` send, for every file and directory, the revision it last changed in, with its date and author (`svn:entry:committed-rev`, `committed-date`, `last-author`). The working copy stores them, and `svn info`, `svn ls -v` and keyword expansion show them. FSFS stores this per node. F1 would have to derive it.
   - ADR-0015's Bloom filters answer one path at a time, which costs one query per file in a checkout of 100,000 files. That is too slow.
   - A workable index is an exact per-revision list of changed paths, with ancestor directories, in `cache.db`. A checkout then scans the revisions newest first until every node in the requested subtree is assigned. Unchanged subtree hashes let the scan assign a whole subtree at once.
   - ADR-0015 rejected an exact path table for its size. SVN keeps the same list in every revision file, so it is the known cost of this feature.
4. **Properties.** The client reads and writes versioned properties on files and directories, and some it sets automatically. `svn add` sets `svn:mime-type` on binary files, and auto-props commonly set `svn:eol-style` and `svn:keywords`.
   - **Derived from F1 data:**
     - `svn:executable` from kind `exec`;
     - `svn:special` from `symlink`, with content `link <target>`;
     - `svn:mime-type` from `attrs.ct`;
     - `svn:needs-lock` from the `locks = required` globs, read-only.
   - **Everything else needs storage.** That covers `svn:eol-style`, `svn:keywords`, `svn:ignore`, `svn:externals`, `svn:mergeinfo` and user properties.
     - File and directory properties could go in a new entry attr (for example `svnprops`), ignored by Git export like the S3 attrs. Directory entries already carry attrs (`git_raw`).
     - The root directory has no entry, so its properties need a commit field or a hidden entry.
     - Either is a format addition.
   - Refusing other properties breaks ordinary commits. Dropping them silently leaves working copies that disagree with a fresh checkout.
5. **Copy history.** F1 trees record no copy source. Copies inside a tree (`svn cp`, `svn mv`) can be stored cheaply, since the target is reused, but `svn log` and `blame` stop at the copy. Branch copies keep their history through item 2.
6. **Merge tracking.** `svn merge` needs `svn:mergeinfo` (item 4) and copy history (item 5). Without both it works only as a plain two-tree merge (`--ignore-ancestry`). Hardware teams with locked binaries rarely merge. Code teams would be better served by Git.
7. **Commits.** Commits are a good fit, with three extensions:
   - **Transaction state.** A transaction spans many requests. Its state and its ingested Files would live in `tmp/` and `cache.db`, as GC roots while open, like S3 multipart uploads (ADR-0007).
   - **Out-of-date check.** Each `PUT`, `DELETE` and `PROPPATCH` names the base revision of that path, and a mixed-revision working copy sends different bases per path. ADR-0014's rule ("the entry at `P` equals the entry at `H`") applies, with `P` chosen per path instead of per request. Directory deletes and directory property changes already conflict with any change below them under that rule, as SVN requires.
   - **Deltas against the base.** `PUT` bodies are svndiff against the base text. They can be applied as they stream, by reading source windows with ranged reads (D19). The result is checked against the client's `X-SVN-Result-Fulltext-MD5`, and the DigestRecord's MD5 provides the base checksum.
   - **Locks** map directly: lock tokens are F1 lock IDs, and the existing commit-path checks enforce them.
8. **Path-level read ACLs.** SVN is path-addressed, so D13's whole-namespace read requirement does not apply to it:
   - an `update-report` reports unreadable children as absent;
   - `log` hides changed paths, and hides the message when no changed path is readable, as `mod_authz_svn` does.
   - SVN would then be the only working-copy client that partial readers could use. Today they have only S3 and the path-addressed native endpoints.
9. **Authentication.** HTTP Basic with a local token works, and the `svn` client caches the credential. OIDC tokens live for 5–15 minutes and the `svn` client cannot refresh them, so OIDC users would need long-lived local tokens. Git has the same limitation today.
10. **Names.** SVN paths must be UTF-8 without control characters. Entries with other names, which Git imports allow, would be omitted from SVN views, and writes to them refused.
11. **Size.** svndiff works in windows of about 100 KiB, so reads and writes stream at any file size. D19 holds.

## What it would bring

- **The existing tools.** TortoiseSVN, IDE and EDA/CAD integrations, and scripts keep working. For SVN-style namespaces this is the only real gain: F1 already offers the same features (locks, needs-lock, per-file out-of-date commits, sparse checkout) through the native client, which has no GUI.
- **Partial readers get a working copy** (item 8).
- **An exit path.** `svnsync` and `svnrdump` against F1 give a standard dump of the history.
- **Gradual migration.** Teams could keep their `svn` clients while others in the same namespace use Git, S3 or the native client, without a flag day.

## Recommendation

1. **Keep the SVN wire protocol a non-goal for version 1.** The protocol is large, and items 1, 3 and 4 add derived indexes, a GC root change and a format addition. GitHub ran the same kind of bridge, on the same layout, and removed it for lack of use.
2. **Build an SVN importer first.** The plan's stated path is migration, but F1 has no migration tool. An importer would read an `svnadmin dump` or `svnrdump` stream, or replay a live repository.
   - It would write one F1 commit per revision, onto refs derived from the standard layout.
   - It would map properties as in item 4 and record the source revision in each commit message.
   - Locks are not in dump files and would be taken again by hand.
   - This is a few thousand lines and needs no format change. It also exercises the mapping in items 1–5 in one direction only, which helps if item 3 below is ever wanted.
3. **If `svn` client access is wanted later**, the order would be:
   - **Prerequisites, each with its own ADR:**
     - the revision table and `refs.log` GC roots (item 1);
     - the last-changed index (item 3);
     - property storage (item 4).
   - **Read-only HTTP v2:** checkout, update, switch, log, info, ls, cat, blame, export, and lock and unlock. This covers viewers and CI.
   - **Commits** (item 7).
   - **Acceptance:** run the stock `svn` 1.14 client in integration tests, as Git and the S3 tools are run. `svn` 1.14.5 and `svnadmin` are installed on the development machine; `mod_dav_svn` is not, so it would have to be added to use it as a reference server.
   - Merge tracking (item 6) would stay out of scope.

## Consequences

- **Decision (2026-10-16, the user): the SVN wire protocol is not pursued in version 1 and is deferred indefinitely.** `doc/plan.md` keeps it as a non-goal.
- If item 2 of the recommendation is accepted, the importer gets its own ADR and a phase or TODO entry.
- If SVN access is pursued later, it requires `retention = all`, and it makes `refs.log` a permanent part of the repository's history. `refs.log` already is one, but it has not been a GC root before.

## Alternatives considered

- **ra_svn (`svnserve` protocol).** Its CRAM-MD5 and SASL authentication cannot verify F1's hashed tokens or OIDC tokens, it has no TLS, and it needs its own listener. Rejected in favour of HTTP.
- **Running a real Subversion server beside F1, synchronized both ways** (as SubGit does for Git). Two sources of truth, two lock tables and two ACL systems. Rejected.
- **Revision numbers from first-parent depth of `default_ref`.** This is simpler, but the numbers change when the ref is rewound, and other branches get no numbers. Rejected.
- **Exposing only `default_ref`, without `branches/` and `tags/`** (item 2, second mapping). This is simpler and enough for repositories without branches, but it breaks every repository URL that migrated users already have. It could be a per-namespace option.
