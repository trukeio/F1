# F1 user manual

F1 is one storage system for files of every size, with full history. It replaces S3-style object storage, SVN-style repositories of large binaries, and Git hosting with one storage model and three ways in: its own client and HTTP API, the S3 API, and Git (with Git LFS).

This manual describes F1 as built at the end of phase 9 (hardening and performance). It has four parts:

1. **Getting started**: what F1 is, its concepts, running a server, and a first session.
2. **Using F1**: namespaces, the native client `f1`, S3, Git and Git LFS, locks, history and retention.
3. **The native API**: its conventions, endpoints and errors, and the Go client package.
4. **Operating F1**: deployment, configuration, identities and access, `f1d`, backups, garbage collection, checks, monitoring and limits.

**Conventions.**

- `$ f1 …` is a command typed in a working copy, with the token of the user in `F1_TOKEN`.
- `$ f1d …` is an administration command, run as the server's user on the server's host. Each takes `--config <file>` (default `f1d.toml`); the examples leave it out. Flags may come before or after the arguments.
- The examples use one server, `http://f1.example.com:8080` (S3 on port 9000), two users, `alice` and `bob` in group `eng`, and three namespaces: `design` (circuit boards and documents, SVN style), `code` (a Git repository) and `media` (an S3 bucket).
- Hashes are written shortened (`4a7dd57ad875`); real ones are 64 hexadecimal digits (BLAKE3).
- References such as "D13" or "ADR-0010" point to the fixed decisions in [`doc/plan.md`](plan.md) and the decision records in [`doc/adr/`](adr/), which are the normative text. This manual explains; those documents decide.

---

# 1 Getting started {#part-1}

## 1.1 What F1 is {#what-f1-is}

F1 stores **files in repositories**. A repository, called a **namespace**, holds a tree of files and its history: every change is a **commit**, and **refs** (branches and tags) name the current commits. Content is split into chunks and stored once, whatever the path, version or protocol that wrote it, so a small edit to a 2 GB file stores only the chunks it touched.

The same namespace answers every protocol. A file pushed with Git is readable with an S3 `GetObject`; a file uploaded with the AWS CLI shows up in `f1 pull` and in `git fetch`.

| Feature | What it gives you | Where |
|---|---|---|
| The native client | working copies, sparse checkouts, local commits, pull with a three-way merge, push, locks, branches; made for large binaries | §2.2 |
| S3 | the AWS CLI, the AWS SDKs, rclone and `mc` against an F1 namespace, including multipart uploads and versioning | §2.3 |
| Git and Git LFS | `git clone`, `fetch`, `push` and partial clone; commit and tree SHAs round-trip exactly; LFS uploads and locks | §2.4 |
| Locks | exclusive file locks enforced on every protocol; required locks for unmergeable formats, as `svn:needs-lock` | §2.5 |
| History and retention | full history, the last N commits, N days, or none, per namespace | §2.6 |
| Path ACLs | read, write, lock and admin per user or group, per path prefix | §4.3 |
| The native API | a small HTTP API for files, listings, history, diffs, objects, commits and locks, with a Go client | Part 3 |
| Portable repositories | a namespace is one directory; a copy or a ZIP of it is a working repository with its history on any F1 server | §4.5 |
| Size transparency | no command, protocol or limit depends on file size: a 1-byte file and a 100 GiB file behave the same | §1.2 |

**One copy of everything.** Content is stored once per repository, however many versions, paths or protocols refer to it. History costs only what changed, measured in chunks of about 64 KiB.

**The server never merges file content.** It accepts a new commit if it is based on the branch's current commit (compare-and-swap), or, when the writer asks, if none of the files it changes were changed since its base (a rebase, as Subversion does). Merging the content of a file changed on both sides happens in the client: Git, or `f1 pull` for the native client (D10, ADR-0014).

**What you cannot read does not exist.** A namespace or bucket you have no permission in answers "not found", exactly as one that does not exist.

### Architecture

<!--diagram:architecture-->

| Component | Role |
|---|---|
| `f1d` | the server and its administration commands: the native API, Git and LFS on one port (default 8080), the S3 API on another (default 9000), `/healthz` and `/metrics`. One process per server. |
| `f1` | the native client, for working copies of large binary repositories. Runs on Linux, macOS and Windows. |
| Data directory | `server.db` (users, tokens, S3 keys, ACLs, the list of namespaces) and `repos/<namespace>/`, one self-contained directory per namespace. |
| Standard clients | Git and Git LFS, the AWS CLI and SDKs, rclone, MinIO `mc`, `curl`. |
| Identity provider | optional: any OIDC provider (Keycloak, Microsoft Entra ID, …) whose access tokens F1 accepts, the same provider G1 trusts. |

Version 1 runs on one Linux, macOS or BSD host with local disk, and SQLite for metadata. It has no external database.

## 1.2 Concepts {#concepts}

**Namespace.** A repository: a name (`design`), a configuration, refs, and a directory `repos/design/` that holds all of its data. To S3 a namespace is a **bucket**; to Git it is a repository at `/git/design.git`.

**Layout.** How a namespace names files. `tree`: paths are hierarchical, as in Git: `a/b` needs `a` to be a directory, and `a` cannot also be a file. `flat`: keys are opaque strings, as in S3: `a` and `a/b` can both exist, and keys may end in `/`.

**Ref.** A name for a commit: a branch (`heads/main`, written `main` in most places) or a tag (`tags/v1`). A ref changes only by compare-and-swap: "move `main` from commit A to commit B" fails if `main` is no longer at A.

**Commit.** A snapshot of the whole tree, its parents, an author, a time and a message. Every write makes a commit: a file saved through S3, a `f1 push`, a `git push`.

**File and chunk.** A file's content is cut at content-defined boundaries into chunks of 16 to 256 KiB (64 KiB on average), each compressed with zstd and stored once, named by its BLAKE3 hash. A File object lists its chunks. Because boundaries depend on the content, an insertion in the middle of a large file changes only the chunks around it.

**Retention.** How much history a namespace keeps: `all`, `last:N` commits per ref, `days:N`, or `none` (only the current state, as a plain S3 bucket). Older history is removed by garbage collection.

**Autocommit.** In an autocommit namespace, writes do not name the commit they are based on: concurrent writes are batched into one commit each few milliseconds (group commit). This is how S3 buckets work. Without autocommit, every write must say which commit it is based on, and fails if the branch has moved.

**Lock.** A claim by one user on one path of a branch. While it is held, nobody else can commit a change to that path, through any protocol.

**Principal.** Who is calling: `user:alice`, with groups such as `group:eng`. Local users get tokens from `f1d`; users of an OIDC identity provider use its access tokens. ACLs grant permissions to principals per path prefix.

**Size transparency (D19).** Nothing a user sees depends on file size: the same commands, limits and protocols apply to a 1-byte file and to a 100 GiB file, and the server and the client use bounded memory for any size.

## 1.3 Running a server {#running}

### From source

F1 needs Go 1.25.

```sh
$ git clone https://github.com/trukeio/F1 && cd F1
$ make build
$ ls bin
f1  f1d
```

Both are static binaries. The container image and the systemd unit are in §4.1.

### Configuring

`f1d` reads one TOML file. A minimal one:

```toml
[server]
data_dir = "/var/lib/f1"        # server.db and repos/
listen   = "0.0.0.0:8080"       # native API, Git, LFS, /metrics, /healthz

[server.s3]
listen = "0.0.0.0:9000"         # S3 API; "" disables it
```

Every key is in §4.2, and `example.toml` in the repository documents them all.

### The first administrator, namespaces and tokens

Administration commands work whether or not the server runs.

```sh
$ f1d user add alice --group eng
user:alice created
$ f1d user add bob --group eng
user:bob created
$ f1d acl set '*' user:alice "" admin              # alice administers the server
$ f1d ns create design --preset svn
design created
$ f1d ns create code --preset git
code created
$ f1d ns create media --preset s3
media created
$ f1d acl set design group:eng "" read,write,lock
$ f1d acl set code   group:eng "" read,write,lock
$ f1d acl set media  group:eng "" read,write
$ f1d token create alice --description laptop
f1_l5z6qpemkheo2m7apnbemwr6wwe7tnuq
token tok_w277dv7x for user:alice, expires never; it is shown only once
$ f1d s3key create alice
access_key = F1UTEBLFZABJORMU7NQ5
secret_key = ytjm6legeoohh7uozfgajs4w5gezm5x73jnyhlgf
```

The token goes to standard output and the note to standard error, so `f1d token create alice > alice.token` keeps only the token.

### Starting

```sh
$ f1d serve --config f1d.toml
$ curl http://localhost:8080/healthz
ok
```

`f1d serve` stops cleanly on `SIGINT` or `SIGTERM`: it stops accepting requests, finishes the running ones, and seals its open packs.

**TLS.** F1 serves plain HTTP. In production, put it behind a reverse proxy that terminates TLS (§4.1).

## 1.4 Which way in {#access}

| You want to… | Use | Credential |
|---|---|---|
| work on large binary files with history, locks and partial checkouts (CAD, boards, media, documents) | the native client `f1` (§2.2) | token in `F1_TOKEN` |
| store and serve objects from applications, backups, pipelines | S3 tools and SDKs (§2.3) | S3 access key |
| keep source code | Git (§2.4) | token as the HTTP password |
| script reads, writes and history from a program | the native API or `pkg/f1client` (Part 3) | bearer token |

All four can be used on the same namespace at once, within the rules of its configuration: Git needs `layout = tree` and `retention = all`, and the native client needs `layout = tree`.

## 1.5 A first session {#first-session}

Alice creates a working copy of `design`, adds files, locks the board and pushes:

```sh
$ export F1_TOKEN=f1_l5z6…
$ f1 clone http://f1.example.com:8080 design
cloned design (main) into design: 0 file(s)
$ cd design
$ mkdir boards docs
$ cp ~/work/main.brd boards/ && echo "first draft" > docs/spec.md
$ f1 status
? boards/main.brd
? docs/spec.md
$ f1 add boards docs
$ f1 commit -m "Initial design"
committed 2 change(s) locally; f1 push sends them
$ f1 lock boards/main.brd
$ f1 push
pushed 4a7dd57ad875 Initial design
```

Bob checks out only the documents, and later adds the boards:

```sh
$ f1 clone --sparse docs http://f1.example.com:8080 design
cloned design (main) into design: 1 file(s)
$ cd design
$ f1 sparse add boards
$ ls -l boards
-r--r--r-- 1 bob bob 300000 Oct 16 07:27 main.brd
```

`*.brd` files need a lock in an SVN-style namespace, so Bob's copy is read-only until he locks it, and Alice holds the lock.

The same files, through S3 and Git:

```sh
$ aws --endpoint-url http://f1.example.com:9000 s3 cp s3://design/docs/spec.md -
first draft
$ git clone http://f1.example.com:8080/git/code.git       # user name: anything; password: the token
```

---

# 2 Using F1 {#part-2}

## 2.1 Namespaces {#namespaces}

A namespace's configuration decides what it is for. Server administrators create namespaces from a preset:

```sh
$ f1d ns create design --preset svn
```

or through the native API (`POST /api/v1/ns`), or with S3 `CreateBucket` (§2.3). The presets:

| Preset | `layout` | `retention` | `autocommit` | `locks` | `git` | For |
|---|---|---|---|---|---|---|
| `s3` | `flat` | `none` | `true` | `exclusive` | `false` | object storage: latest version only, any key |
| `svn` | `tree` | `all` | `false` | `required:*.sch,*.brd,*.docx` | `false` | large binaries with history and locks |
| `git` | `tree` | `all` | `false` | `exclusive` | `true` | source code |
| none | `tree` | `all` | `false` | `exclusive` | `false` | the default |

The configuration is the file `repos/<namespace>/f1repo.toml`:

```toml
format = 1
uuid   = "6e928293-9d1f-4e2a-b325-054988abf0b9"

layout      = "tree"                       # tree | flat
retention   = "all"                        # all | last:N | days:N | none
autocommit  = false
branches    = true
locks       = "required:*.sch,*.brd,*.docx"   # off | exclusive | required:<glob>,<glob>
git         = false                        # serve under /git/<namespace>.git
default_ref = "main"

[merge]                                    # optional: text or binary merge per glob (§2.2)
"*.svg" = "text"
"*.dat" = "binary"
```

Edit it with the server stopped, or with the namespace detached (`f1d repo detach`, edit, `f1d repo attach`). Rules:

- **Git namespaces** need `layout = tree` and `retention = all` (D12).
- **Paths in `tree` layout** must not be empty, start or end with `/`, contain `//`, or have `.` or `..` components. S3 keys that break these rules, including "folder markers" ending in `/`, need a `flat` namespace.
- **Listings** are in byte order of the full path in both layouts. In `tree` layout a directory sorts as `name/`, as in Git, so `a.txt` comes before `a/b`.

## 2.2 The native client {#client}

`f1` keeps a **working copy**: a directory with your files and a `.f1/` directory with the client's state. It is made for repositories of large binaries: it checks out only the directories you ask for, keeps no second copy of the files, and locks files that cannot be merged.

The client reads the token from `F1_TOKEN` and never stores it. It needs read permission on the whole namespace (D13); a sparse checkout limits what is downloaded, not what you may read.

```
f1 <command> [flags] [args]

  clone <server-url> <namespace> [dir]   create a working copy
        --ref <branch>                   branch to follow (default: the namespace's default)
        --sparse <dir>                   check out only this directory (repeatable)
  sparse list|add <dir>|remove <dir>     change the sparse checkout
  status                                 show changed files
  add <path>...                          add files; mark conflicts resolved
  rm <path>...                           delete files; resolve as deleted
  commit -m <message>                    record a local commit
  push [--no-rebase]                     send local commits to the server; with
                                         --no-rebase, fail if others changed
                                         any file meanwhile
  pull                                   merge the server's new commits
  log [--limit n] [path]                 show history
  diff [path]...                         show uncommitted changes
  lock <path>...                         lock files
  unlock [--force] <path>...             unlock files
  locks                                  list locks
  checkout <branch|commit>               switch the working copy
  branch [name]                          list branches, or create one at the base commit
  version                                print the version
```

`f1` exits with 0 on success, 1 on an error or unresolved conflicts, and 2 on a usage error.

### Clone and sparse checkouts

```sh
$ f1 clone http://f1.example.com:8080 design                    # everything, into ./design
$ f1 clone --sparse boards/rev-c --sparse docs http://f1.example.com:8080 design work
$ f1 clone --ref rev-b http://f1.example.com:8080 design design-rev-b
```

A sparse pattern is a directory: `docs` checks out `docs/…`. Change the patterns later with:

```sh
$ f1 sparse list
docs
$ f1 sparse add boards          # downloads boards/
$ f1 sparse remove docs         # deletes docs/ from the working copy
```

`sparse remove` refuses the last pattern, and a directory with local changes. A sparse clone transfers only the content of the files it checks out, plus a few kilobytes of metadata: cloning one tenth of a 50 GB repository transfers 5 GB.

Namespaces with `layout = flat` cannot be cloned; they are for S3 tools.

### Changes and commits

```sh
$ f1 status
M boards/main.brd
A docs/review.md
D docs/old.md
? docs/draft.md
2 local commit(s) not pushed
```

| Code | Meaning |
|---|---|
| `M` | modified |
| `A` | added with `f1 add`, not committed yet |
| `D` | deleted (with `f1 rm`, or removed from disk) |
| `?` | not tracked: `f1 add` it to include it |
| `C` | in conflict after a pull |

- **`f1 add <path>…`** tracks new files; a directory adds every file below it.
- **`f1 rm <path>…`** deletes files from disk and from the next commit.
- **`f1 commit -m <message>`** records every change to tracked files as a local commit: modified and deleted files need no `add`. Local commits stay in the working copy (`.f1/objects/`) until pushed.
- **`f1 diff [path]…`** prints a unified diff of uncommitted changes to text files.
- **`f1 log [--limit n] [path]`** lists your local commits, then the server's history up to your working copy's base commit. Pull to see newer commits.

Status is fast on large working copies: a file whose size, modification time and inode match the index is not read. Only changed files are hashed.

### Push

```sh
$ f1 push
pushed 4a7dd57ad875 Initial design
```

Push sends local commits in order: it uploads the content the server lacks, then asks the server to move the branch from your base commit to the new one.

If someone pushed in the meantime, the outcome depends on what they changed:

- **Other files:** the server puts your commits on top of theirs, and push then updates your working copy with their changes, as `f1 pull` would. This is Subversion's rule: a commit fails only if one of its files is out of date.
- **The same files:** push fails, names them, and tells you to pull. Pull merges the changes into your commits; then push again.

`f1 push --no-rebase` fails whenever the branch has moved, even on other files. Use it when your commits must be tested exactly as they will land.

If the server refuses a commit (a locked file, a missing permission), push stops there; earlier commits stay pushed.

```sh
$ f1 push
pushed 9c1e4b07d2aa Alice edits b.txt
the server rebased the push onto other changes; 1 file(s) updated
$ f1 push
f1 push: the branch has new commits on the server; run f1 pull, then push again (changed there: b.txt)
```

```sh
$ f1 push
f1 push: push "Bob edits board": f1: 423 locked: repo: path is locked by user:alice: boards/main.brd
```

### Pull and merge

```sh
$ f1 pull
2 file(s) updated, 1 local commit(s) rebased
```

Pull brings in the server's new commits and **rebases** your local commits onto them, path by path, so history stays linear. For each path both sides changed:

- **Text** is merged line by line, three-way. A file is text if its first 8 KiB are valid UTF-8 without NUL bytes, in all three versions. Large text files merge with bounded memory.
- **Binary** files are not merged: your version is kept as `<name>.mine`, the server's as `<name>.theirs`.
- **Delete against modify** keeps the surviving side as `.mine` or `.theirs`.

The namespace's `[merge]` table overrides the text test per glob; the longest matching glob wins:

```toml
[merge]
"*.svg" = "text"
"*.dat" = "binary"
```

A local commit that merges cleanly is rebased and keeps its message (it gets a new hash, as with `git rebase`). A commit that conflicts, and every local commit after it, becomes working changes again; their messages are kept for the next `f1 commit`:

```sh
$ f1 pull
1 file(s) updated
1 local commit(s) conflicted and are working changes again; their messages are kept for f1 commit
C docs/notes.txt
resolve the conflicts, then f1 add (or f1 rm) each path
$ cat docs/notes.txt
a
<<<<<<< mine
B
=======
ALICE
>>>>>>> theirs
C
```

Resolve each conflict by editing the file (or choosing between `.mine` and `.theirs`), then `f1 add` it, or `f1 rm` it to resolve it as deleted. `f1 add` removes the `.mine` and `.theirs` files. `commit` and `pull` refuse while conflicts remain. `f1 commit` without `-m` then uses the kept message.

Pull computes everything first (downloads, merges) and only then changes the working copy, so a failed pull leaves it as it was. A pull of a large conflicting file needs temporary disk space of up to three times its size.

### Locks

```sh
$ f1 lock boards/main.brd
$ f1 locks
boards/main.brd user:alice 2026-10-16 07:26
$ f1 unlock boards/main.brd
$ f1 unlock --force boards/main.brd       # someone else's lock; needs admin on the path
```

In a namespace with `locks = required:<globs>`, matching files are checked out **read-only** until you lock them, as with `svn:needs-lock`. `f1 lock` makes the file writable, and `f1 unlock` read-only again. Lock before you edit: a commit that changes a required-lock file you have not locked is refused at push. The lock rules are in §2.5.

### Branches

```sh
$ f1 branch
* main
$ f1 branch rev-b           # create rev-b at the working copy's base commit
$ f1 checkout rev-b         # switch; the working copy must have no changes and no unpushed commits
$ f1 checkout 4a7dd57ad875… # a commit: a detached, read-only working copy
```

Merging branches is done with Git, or by hand; the native client has no branch merge in version 1.

### What `.f1/` holds

| File | Content |
|---|---|
| `config.json` | server URL, namespace, branch and sparse patterns |
| `state.json` | base commit, unpushed commits, added paths, conflicts, kept messages |
| `index.json` | per path: the server's version, the local content hash, mtime and inode |
| `objects/` | one pack per unpushed commit; emptied when everything is pushed |
| `tmp/` | downloads and merges in progress |

There are no pristine copies: a 50 GB checkout takes 50 GB. The base version of a file, needed for a merge or a diff, is downloaded when needed.

The client refuses unsafe paths from the server (absolute paths, `..`, writes through symbolic links) and skips Git submodules with a warning.

## 2.3 S3 {#s3}

The S3 API listens on its own port (default `127.0.0.1:9000`). A bucket is a namespace; reads and writes go to its default branch.

### Keys and clients

S3 requests are signed with an access key (SigV4); bearer tokens do not work there. An administrator creates a key with `f1d s3key create alice`, or any authenticated user creates their own:

```sh
$ curl -s -X POST -H "Authorization: Bearer $F1_TOKEN" http://f1.example.com:8080/api/v1/me/s3keys
{"access_key":"F1UTEBLFZABJORMU7NQ5","secret":"ytjm6l…","created":"2026-10-16T05:28:00Z"}
```

The secret is shown once. `GET /api/v1/me/s3keys` lists your keys and `DELETE /api/v1/me/s3keys/<key>` revokes one. A key acts as its user, with the same ACLs. For OIDC users, the key carries the groups of the user's last native request, for at most `auth.s3_group_ttl` (24 hours by default).

The region is `us-east-1` unless the server sets another. Addressing is path style (`http://host:9000/bucket/key`); virtual hosts (`bucket.s3.example.com`) work when the server sets `server.s3.domain`.

```sh
# AWS CLI
$ aws configure set aws_access_key_id F1UTEBLFZABJORMU7NQ5
$ aws configure set aws_secret_access_key ytjm6l…
$ aws --endpoint-url http://f1.example.com:9000 s3 cp render.mp4 s3://media/2026/render.mp4
$ aws --endpoint-url http://f1.example.com:9000 s3 sync ./site s3://media/site

# rclone (~/.config/rclone/rclone.conf)
[f1]
type = s3
provider = Other
endpoint = http://f1.example.com:9000
access_key_id = F1UTEBLFZABJORMU7NQ5
secret_access_key = ytjm6l…

# MinIO client
$ mc alias set f1 http://f1.example.com:9000 F1UTEBLFZABJORMU7NQ5 ytjm6l…
$ mc ls f1/media
```

```python
import boto3
s3 = boto3.client("s3", endpoint_url="http://f1.example.com:9000", region_name="us-east-1",
                  aws_access_key_id="F1UTEBLFZABJORMU7NQ5", aws_secret_access_key="ytjm6l…")
s3.put_object(Bucket="media", Key="photos/cat.jpg", Body=b"…", ContentType="image/jpeg",
              Metadata={"camera": "x100"})
print(s3.get_object(Bucket="media", Key="photos/cat.jpg")["Metadata"])
```

### Operations

| Group | Operations |
|---|---|
| Service | ListBuckets |
| Buckets | HeadBucket, CreateBucket, DeleteBucket, GetBucketLocation, GetBucketVersioning |
| Objects | GetObject (Range, If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since), HeadObject, PutObject (also conditional, `If-None-Match: *` and `If-Match`), DeleteObject, DeleteObjects, CopyObject (metadata COPY or REPLACE, also between buckets) |
| Listings | ListObjectsV2 (prefix, delimiter, continuation token, start-after, max-keys, fetch-owner), ListObjects (v1), ListObjectVersions |
| Multipart | CreateMultipartUpload, UploadPart, UploadPartCopy, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads |
| Everything else | `501 NotImplemented`: bucket policies and ACLs, tagging, lifecycle, CORS, website, logging, notifications, replication, encryption settings, object lock, Select |

Access is governed by F1's ACLs (§4.3), not by S3 policies.

### How S3 maps onto F1

- **Writes are commits.** In an autocommit namespace (`s3` preset), concurrent writes are batched into group commits; conditional writes are checked in order inside the batch. Otherwise each write is one commit, `s3: PutObject <key>`, by the key's user. DeleteObjects is one commit.
- **Buckets.** CreateBucket makes a namespace with the `s3` preset and needs a server administrator; `server.s3.create_bucket = false` disables it and DeleteBucket. DeleteBucket needs an empty bucket and moves its directory aside to `repos/.deleted-<name>-<suffix>`; nothing is erased.
- **Aliases.** The server can expose a branch as another bucket (`[[server.s3.alias]]`, §4.2), for example `design-dev` for branch `dev` of `design`.
- **Metadata.** `Content-Type`, `x-amz-meta-*` (at most 2 KB) and Cache-Control, Content-Disposition, Content-Encoding, Content-Language and Expires are kept. Objects without a type are served as `binary/octet-stream`.
- **ETags** are the MD5 of the content, or for multipart uploads the MD5 of the part MD5s followed by `-<parts>`, as in S3. Files written through Git or the native client get an MD5 ETag too.
- **Checksums.** `Content-MD5` and `x-amz-checksum-crc32`, `-crc32c`, `-crc64nvme`, `-sha1` and `-sha256` are verified when sent, stored, and returned with `x-amz-checksum-mode: ENABLED`. F1 does not add a checksum the client did not send.
- **Multipart.** Parts are at least 5 MiB except the last; completing an upload copies no data. Uploads not completed within `server.s3.upload_expiry` (7 days) are aborted.
- **Ranges.** One byte range per request; several ranges return the whole object, as S3 does.
- **Presigned URLs** work for up to 7 days. Anonymous requests and SigV2 are refused.

### Versioning

A bucket is versioned exactly when its namespace keeps history (`retention` other than `none`); PutBucketVersioning is not implemented, because retention decides.

- A **version id** is the commit that wrote the version. GetObject and HeadObject accept any commit of the bucket as `versionId`.
- **ListObjectVersions** walks the history: every change of a key is a version, every deletion a delete marker. On long histories, the native `log` (§3.2) is faster.
- **Deleting a version** is possible only for the newest version of a key, which makes the previous one current. Deleting a delete marker restores the object. Older versions cannot be deleted: history is immutable, and retention expires it.

```python
>>> [(v["VersionId"][:10], v["IsLatest"]) for v in s3.list_object_versions(Bucket="design", Prefix="docs/spec.md")["Versions"]]
[('NB3PN7BHG2', True), ('6CZ6XRRMIV', False), ('JJ65K6WYOX', False)]
```

### S3 errors

| Situation | Answer |
|---|---|
| Unknown bucket, or no permission in it | `404 NoSuchBucket` |
| The path is locked by someone else | `403 AccessDenied`, `path is locked by <owner>` |
| A key `tree` layout cannot hold (`a//b`, `dir/`) | `400 InvalidArgument`, pointing to `layout = flat` |
| `a` is a file and `a/b` is written (tree layout) | `409 InvalidRequest` |
| The repository is frozen for a copy | `503 SlowDown` |
| A checksum or signature does not match | `400 BadDigest`, `403 SignatureDoesNotMatch` |

## 2.4 Git and Git LFS {#git}

A namespace with `git = true` (the `git` preset) is a Git repository at `/git/<namespace>.git` on the main port.

```sh
$ git clone http://f1.example.com:8080/git/code.git
Username: alice                      # anything
Password: f1_l5z6…                   # the token
```

The password is the token; the user name is ignored. With a credential helper the token is asked once. An OIDC access token works the same way, or as a bearer header:

```sh
$ git -c http.extraHeader="Authorization: Bearer $F1_TOKEN" clone http://f1.example.com:8080/git/code.git
```

### What works

- `clone`, `fetch`, `pull` and `push`, with protocol v2 for fetches and v0/v1 for older clients and for push.
- **Partial clone**: `git clone --filter=blob:none` or `--filter=blob:limit=1m` downloads file content on demand. This is how Git users avoid downloading large files.
- Shallow clones (`--depth`, `--shallow-since`), tags, annotated and signed commits and tags, atomic pushes (`git push --atomic`), branch and tag deletion, push options.
- **Exact SHAs.** Commits, trees and tags keep their Git SHA-1s, including odd trees that other tools rewrite (non-standard modes, entry order). Pushing a repository to F1 and cloning it back gives the same hashes.
- **Writes from other protocols.** A file saved through S3 or the native client becomes a normal Git commit, authored by its user with time zone `+0000`; `git pull` sees it.
- **Locks and ACLs** apply to pushes: a push that changes a file locked by someone else, or a path you may not write, is refused with a message.

Fetch packs carry deltas between versions of the same file, as Git does, for files of at most 1 MiB. They are not thin packs against the objects you already have, so an incremental fetch can transfer more than from a Git server.

Git needs read permission on the whole namespace (D13). Pushes that Git's `fsck` would reject as errors (trees with duplicate entries, SHA-1 collision attacks) are refused.

### Git LFS

LFS works against the same URL with no configuration: the batch API, the `basic` transfer, and the locking API.

```sh
$ git lfs install
$ git lfs track "*.psd"
$ git add .gitattributes art.psd && git commit -m "Artwork" && git push
$ git lfs lock art.psd
$ git lfs locks
```

LFS locks are F1 locks: a file locked with `git lfs lock` is locked for the native client and S3 as well. Set `git config lfs.locksverify true` so that `git push` checks them first.

F1 keeps LFS only for compatibility. It stores the real content in either case, and S3 and native reads of an LFS file return the content, not the pointer. A write through S3 or the native client to an LFS path keeps it an LFS file. Git users can rely on partial clone instead of LFS for large files.

A push whose LFS pointers name objects the server lacks is refused with a message to run `git lfs push`.

## 2.5 Locks {#locks}

A lock is held by one principal on one path of one branch. The namespace's `locks` setting decides how locks act:

| `locks` | Locking a file | A commit that changes a path someone else locked | A commit that changes an unlocked path |
|---|---|---|---|
| `off` | not possible | allowed | allowed |
| `exclusive` | optional | refused | allowed |
| `required:<glob>,…` | needed for matching paths | refused | refused for matching paths, allowed for others |

- **Globs.** A glob without `/` matches the file name in any directory (`*.brd` matches `boards/rev-c/main.brd`); a glob with `/` matches the whole path.
- **Every protocol.** Locks are checked when a branch moves, on the files the move changes, so the native client, S3, Git and the native API all obey them. Deletions are changes. For a new branch, the changed files are those that differ from its merge base with the default branch, so the default branch's locks apply.
- **Taking and releasing.** Locking a path you already hold refreshes it. Only the owner unlocks; `--force` (or `?force=true`) needs `admin` on the path. A lock may have an expiry (native API `ttl_seconds`); expired locks are ignored. The `lock` permission is needed to take a lock.
- **Where they are seen.** `f1 locks`, `git lfs locks`, `GET /api/v1/ns/<ns>/locks`. A refused write names the owner: native `423 locked`, S3 `403 AccessDenied`, Git a push error.

## 2.6 History and retention {#history}

Every namespace except `retention = none` keeps history, and every protocol sees the same history:

| To see… | Native client | Git | S3 | Native API |
|---|---|---|---|---|
| the history of a branch | `f1 log` | `git log` | | `GET …/log/<ref>` |
| the history of a file | `f1 log <path>` | `git log -- <path>` | ListObjectVersions | `GET …/log/<ref>?path=` |
| what changed | `f1 diff`, pull output | `git diff` | | `GET …/diff/<a>..<b>` |
| an old version | `f1 checkout <commit>` | `git checkout` | GetObject `versionId` | `GET …/files/<commit>/<path>` |

Retention removes older history at the next garbage collection (§4.6):

- `all`: keep every commit.
- `last:N`: per branch, keep the last N commits of its first-parent history.
- `days:N`: per branch, keep commits newer than N days.
- `none`: keep only the commit each ref points to.

The oldest kept commit then has parents that are no longer stored; history simply ends there. Content stays as long as any kept commit, open multipart upload or recent write uses it.

---

# 3 The native API {#part-3}

## 3.1 Conventions {#api-conventions}

The native API is plain HTTP under `/api/v1` on the main port: JSON for metadata, raw bytes for file content.

- **Authentication.** `Authorization: Bearer <token>`: a local `f1_…` token or an OIDC access token.
- **Refs in URLs.** A ref is one path segment: `main`, `heads%2Frev-b`, or a commit hash. Short names mean branches (`main` is `heads/main`).
- **Parents.** A write names the commit it is based on in the `F1-Parent` header: a commit hash, or `new` for a branch that must not exist yet. Autocommit namespaces need no parent; elsewhere a write without one answers `428 parent_required`. If the branch has moved, the write fails with `409 conflict` and the current commit.
- **Rebase.** With `F1-Rebase: true` (in `POST /commits`: `"rebase": true`), a write whose branch has moved is applied on top of the current commit instead, if none of its paths changed since its parent, and none of their directories was deleted or replaced by a file. Otherwise it fails with `409 conflict`, and `paths` lists the paths that changed. The result's `parent` is the commit the write was built on: when it differs from the one you sent, your write was rebased. Use it when your writes stand alone, such as an agent or a script updating its own files. Leave it off when a write is only correct together with the files it was based on.
- **Reads** support `Range`, `If-Match`, `If-None-Match` and `HEAD`. The ETag is the File hash. Headers `F1-Commit`, `F1-File`, `F1-Kind` and `F1-Size` describe the result.
- **Caching.** A read by commit hash never changes. It is sent with `Cache-Control: private, max-age=31536000, immutable`, as are reads of `objects/{hash}`. A read by branch name is sent with `private, no-cache`, so a cache revalidates it, and gets `304 Not Modified` while the file is unchanged.
- **Errors** are JSON: `{"code": "...", "message": "...", "current": "...", "owner": "...", "paths": [...]}` (§3.4).

```sh
$ curl -H "Authorization: Bearer $F1_TOKEN" -H "F1-Parent: $(…current commit…)" \
       -T report.pdf http://f1.example.com:8080/api/v1/ns/design/files/main/docs/report.pdf
$ curl -H "Authorization: Bearer $F1_TOKEN" -r 0-99 \
       http://f1.example.com:8080/api/v1/ns/design/files/main/docs/report.pdf
```

## 3.2 Endpoints {#endpoints}

Paths below are relative to `/api/v1`.

| Method and path | Does | Needs |
|---|---|---|
| `GET /ns` | list the namespaces you can access, with their configuration | |
| `POST /ns` | create a namespace: `{"name", "preset"}` or `{"name", "config"}` | server admin |
| `GET /ns/{ns}/refs` | list refs | read |
| `PUT /ns/{ns}/refs/{ref}` | move a ref: `{"old", "new"}`, compare-and-swap | write on the changed files |
| `GET`, `HEAD /ns/{ns}/files/{ref}/{path}` | read or stat a file at a ref or commit | read on the path |
| `PUT /ns/{ns}/files/{ref}/{path}` | write a file and commit; `F1-Parent`, `F1-Rebase`, `F1-Message`, `Content-Type` | write on the path |
| `DELETE /ns/{ns}/files/{ref}/{path}` | delete a file and commit; `F1-Parent`, `F1-Rebase`, `F1-Message` | write on the path |
| `GET /ns/{ns}/tree/{ref}/{prefix}?delim=/&after=&limit=` | list files, by page | read; filtered to readable paths |
| `GET /ns/{ns}/log/{ref}?path=&limit=` | history, of a ref or of one path | read on the namespace, or on the path |
| `GET /ns/{ns}/diff/{a}..{b}?prefix=` | changed files between two commits | read; filtered to readable paths |
| `POST /ns/{ns}/objects/missing` | which of these hashes are not stored | whole-namespace read |
| `POST /ns/{ns}/objects` | upload objects as a pack stream | whole-namespace read and write |
| `GET /ns/{ns}/objects/{hash}` | one object | whole-namespace read |
| `POST /ns/{ns}/objects/fetch` | several objects as a pack stream | whole-namespace read |
| `POST /ns/{ns}/commits` | commit uploaded objects: `{"ref", "parent", "changes", "message", "rebase"}` | whole-namespace read; write on the changed paths |
| `POST /ns/{ns}/locks`, `GET /ns/{ns}/locks`, `DELETE /ns/{ns}/locks/{id}` | take, list and release locks | lock; `?force=true` needs admin |
| `GET /ns/{ns}/archive` | the repository as a ZIP | admin |
| `GET /me` | who you are, with your groups | |
| `POST`, `GET /me/s3keys`, `DELETE /me/s3keys/{key}` | your S3 keys | |

The object endpoints are what the native client uses to push: upload the content the server lacks, then commit it. They take hashes rather than paths, so they need read permission on the whole namespace (D13). The pack stream and object encodings are specified in [`doc/format.md`](format.md). The server computes content digests itself and refuses uploaded DigestRecords.

## 3.3 The Go client {#go-client}

`github.com/trukeio/f1/pkg/f1client` wraps the native API. It uses only the standard library.

```sh
$ go get github.com/trukeio/f1/pkg/f1client
```

| Area | Calls |
|---|---|
| Caller | `Me`, `CreateS3Key`, `S3Keys`, `DeleteS3Key` |
| Namespaces and refs | `Namespaces`, `CreateNamespace`, `Refs`, `UpdateRef` |
| Files | `Get` (with `Range`), `Stat`, `Put`, `Delete` |
| Listings and history | `List`, `Log`, `Diff` |
| Objects and commits | `MissingObjects`, `UploadObjects`, `GetObject`, `FetchObjects`, `Commit` |
| Locks | `Lock`, `Unlock`, `Locks` |
| Repositories | `Archive` |
| Errors | `APIError`, `Code(err)`, the `Code…` constants |

A complete program:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

func main() {
	ctx := context.Background()
	c := f1client.New("http://f1.example.com:8080", os.Getenv("F1_TOKEN"))

	me, err := c.Me(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(me.Principal, me.Groups) // user:alice [group:eng]

	// Write a file: one commit on top of the branch's current commit.
	refs, err := c.Refs(ctx, "design")
	if err != nil {
		log.Fatal(err)
	}
	parent := "new" // the branch does not exist yet
	for _, r := range refs {
		if r.Name == "heads/main" {
			parent = r.Commit
		}
	}
	body := "Board revision C\n"
	res, err := c.Put(ctx, "design", "main", "docs/rev-c.txt", strings.NewReader(body), int64(len(body)),
		&f1client.PutOptions{Parent: parent, Message: "Add revision C notes", ContentType: "text/plain"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("commit", res.Commit)

	// A write based on the old commit fails: the branch has moved.
	_, err = c.Put(ctx, "design", "main", "docs/rev-c.txt", strings.NewReader("x"), 1,
		&f1client.PutOptions{Parent: parent})
	var apiErr *f1client.APIError
	if f1client.Code(err) == f1client.CodeConflict && errors.As(err, &apiErr) {
		fmt.Println("conflict; the branch is now at", apiErr.Error_.Current)
	}

	// Read part of the file.
	rc, info, err := c.Get(ctx, "design", "main", "docs/rev-c.txt", &f1client.GetOptions{Range: "bytes=6-13"})
	if err != nil {
		log.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	fmt.Printf("%q of %d bytes\n", data, info.Size) // "revision" of 17 bytes

	// List a directory, page by page.
	opts := &f1client.ListOptions{Delimiter: "/", Limit: 100}
	for {
		page, err := c.List(ctx, "design", "main", "docs/", opts)
		if err != nil {
			log.Fatal(err)
		}
		for _, it := range page.Items {
			fmt.Println(it.Path, it.Size)
		}
		if page.Next == "" {
			break
		}
		opts.After = page.Next
	}

	// The history of one file.
	hist, err := c.Log(ctx, "design", "main", "docs/notes.txt", 10)
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range hist {
		fmt.Println(h.Hash[:12], h.Author.Name, time.Unix(h.Author.Time, 0).Format(time.DateTime), strings.TrimSpace(h.Message))
	}

	// Lock a file for an hour, then release it.
	l, err := c.Lock(ctx, "design", "main", "boards/main.brd", time.Hour)
	if err != nil {
		log.Fatal(err)
	}
	if err := c.Unlock(ctx, "design", l.ID, false); err != nil {
		log.Fatal(err)
	}
}
```

`Put` streams: pass any `io.Reader` and its size, or `-1` if unknown. `Get` returns the body as a stream; the caller closes it.

## 3.4 Errors {#errors}

| HTTP | `code` | Meaning | What to do |
|---|---|---|---|
| 400 | `bad_request` | malformed request, invalid path or name | fix the request |
| 401 | `unauthenticated` | no token, or an invalid, expired or revoked one | get a new token |
| 403 | `forbidden` | the ACL does not allow it | ask an administrator |
| 404 | `not_found` | no such namespace, ref or file, or no permission to know | |
| 409 | `conflict` | the branch moved; `current` is its commit; with `F1-Rebase`, `paths` lists the paths changed meanwhile | read it, redo the change, retry |
| 409 | `path_conflict` | a file is in the way of a directory, or the reverse | |
| 409 | `exists` | the name is taken | |
| 409 | `missing_objects` | a commit names objects that are not stored (collected meanwhile) | upload them and retry |
| 423 | `locked` | the path is locked; `owner` is the holder, empty if you need to lock it yourself | lock, or wait |
| 409 | `locked` | taking a lock that someone else holds | wait, or ask them |
| 423 | `frozen` | the repository is frozen for a copy | retry later |
| 428 | `parent_required` | a write without `F1-Parent` outside autocommit | send the parent |
| 500 | `internal` | a server error; the log has details | |

---

# 4 Operating F1 {#part-4}

## 4.1 Deployment {#deployment}

**systemd.** `deploy/f1d.service` runs `f1d` as user `f1`, with its data in `/var/lib/f1` (`StateDirectory`), `ProtectSystem=strict`, no capabilities and a private `/tmp`. The unit's header lists the installation steps:

```sh
$ sudo useradd --system --home-dir /var/lib/f1 --shell /usr/sbin/nologin f1
$ sudo install -m 0755 bin/f1d /usr/local/bin/f1d
$ sudo install -D -m 0640 -g f1 deploy/f1d.toml /etc/f1/f1d.toml     # adjust data_dir and listeners
$ sudo install -m 0644 deploy/f1d.service /etc/systemd/system/
$ sudo systemctl daemon-reload && sudo systemctl enable --now f1d
$ sudo -u f1 f1d user add alice --config /etc/f1/f1d.toml
```

**Container.** `deploy/Dockerfile` builds a static `f1d` on an Alpine image, running as a non-root user, with a volume at `/var/lib/f1`:

```sh
$ docker build -f deploy/Dockerfile -t f1 .
$ docker run -d --name f1 -p 8080:8080 -p 9000:9000 -v f1-data:/var/lib/f1 f1
$ docker exec f1 f1d user add alice --config /etc/f1/f1d.toml
```

**TLS.** Put F1 behind a reverse proxy that terminates TLS (nginx, Caddy, HAProxy), and let `f1d` listen on localhost. The proxy must:

- pass the `Host` header unchanged, because S3 signatures cover it;
- allow request bodies of any size and long requests;
- not buffer requests or responses, because uploads, downloads and Git stream.

**Disks.** Use SSDs with power-loss protection: every commit makes about six fsyncs. Keep the data directory on a local filesystem.

**Platforms.** The server runs on Linux, macOS and the BSDs, not on Windows. Clients run anywhere.

## 4.2 Configuration {#configuration}

`f1d serve --config <file>` reads one TOML file. Unknown keys are errors. Relative paths are relative to the file.

| Key | Default | Meaning |
|---|---|---|
| `server.data_dir` | `data` | `server.db` and `repos/` |
| `server.listen` | `127.0.0.1:8080` | native API, Git, LFS, `/healthz`, `/metrics` |
| `server.s3.listen` | `127.0.0.1:9000` | S3 API; `""` disables it |
| `server.s3.region` | `us-east-1` | the region S3 clients sign for |
| `server.s3.domain` | none | virtual-host addressing: `<bucket>.<domain>` |
| `server.s3.create_bucket` | `true` | server administrators may create and delete buckets |
| `server.s3.upload_expiry` | `168h` | open multipart uploads are aborted after this |
| `[[server.s3.alias]]` | none | `bucket`, `namespace`, `ref`: a branch exposed as a bucket |
| `server.gc.interval` | `24h` | garbage collection of every repository; `0` disables it |
| `server.gc.grace` | `24h` | unreferenced objects newer than this are kept |
| `server.metrics.enabled` | `true` | serve `/metrics` (no authentication) |
| `server.metrics.namespaces` | `false` | storage metrics per namespace (reveal namespace names) |
| `[[auth.oidc]]` | none | trusted OIDC providers (§4.3) |
| `auth.s3_group_ttl` | `24h` | how long an OIDC user's groups apply to their S3 keys |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `text` | `text` or `json` |

```toml
[server]
data_dir = "/var/lib/f1"
listen   = "127.0.0.1:8080"

[server.s3]
listen        = "127.0.0.1:9000"
region        = "us-east-1"
domain        = "s3.example.com"
create_bucket = true
upload_expiry = "168h"

[[server.s3.alias]]
bucket    = "design-dev"
namespace = "design"
ref       = "heads/dev"

[server.gc]
interval = "24h"
grace    = "24h"

[server.metrics]
enabled    = true
namespaces = false

[[auth.oidc]]
name            = "corp"
issuer          = "https://login.example.com/realms/acme"
audiences       = ["f1"]
principal_claim = "sub"
group_claims    = ["groups"]

[log]
level  = "info"
format = "json"
```

## 4.3 Identities and access {#identities}

### Local users and tokens

```sh
$ f1d user add alice --group eng --group ops
$ f1d user list
user:alice	local	group:eng,group:ops
$ f1d user groups alice eng                 # replace the groups
$ f1d token create alice --expires 720h --description "build server"
$ f1d token list alice
$ f1d token revoke tok_w277dv7x
$ f1d user disable alice                    # blocks every token and S3 key of alice
$ f1d user enable alice
```

Tokens start with `f1_` and are stored hashed; a lost token cannot be shown again, only revoked and replaced.

### OIDC

F1 accepts access tokens from the enterprise identity provider, the same one G1 trusts, so both systems know the same users and groups. Register F1 as a client with its own audience (`f1`), and add the provider:

```toml
[[auth.oidc]]
name            = "corp"                                   # label for logs
issuer          = "https://login.example.com/realms/acme"
audiences       = ["f1"]
principal_claim = "sub"                                    # user:<sub>
group_claims    = ["groups"]                               # group:<value> for each value
# jwks_url      = "…"                                      # overrides discovery
```

- Tokens are JWTs verified locally against the provider's keys (RS256, ES256 or EdDSA), with their issuer, audience and expiry.
- The principal is `user:<value of principal_claim>`; groups come from the token on every request. A group change applies when the token is refreshed, so keep tokens short-lived (5 to 15 minutes).
- Users obtain tokens with the authorization code flow, services with client credentials. Version 1's clients take the token from the environment (`F1_TOKEN`, a Git password); a login command is planned.
- A name belongs to one issuer: a local user cannot take the name of an OIDC principal, and the reverse is refused and logged.

### ACLs

An ACL entry grants permissions to a principal on the paths of a namespace that start with a prefix:

```sh
$ f1d acl set design group:eng "" read,write,lock          # the whole namespace
$ f1d acl set design group:contractors "docs/" read         # docs/ only
$ f1d acl set design user:carol "docs/hr/" none             # deny below docs/hr/
$ f1d acl set '*' user:alice "" admin                       # server administrator
$ f1d acl list design
$ f1d acl rm design group:contractors "docs/"
```

| Permission | Allows |
|---|---|
| `read` | reading files, listings, history and diffs |
| `write` | changing files: commits, uploads, deletions, ref updates |
| `lock` | taking locks |
| `admin` | all of the above, removing others' locks, archiving the namespace |
| `none` | nothing: denies, overriding shorter prefixes |

- **Matching.** Prefixes are plain strings: `docs/` is a directory, while `docs` also matches `docs.txt`. The longest matching prefix decides, combining the entries of the user and of its groups at that prefix. Without an entry, access is denied.
- **Server administrators** have `admin` on namespace `*` with prefix `""`. They can create namespaces and buckets.
- **Hash-addressed access (D13).** Git, LFS, the native client and the native object endpoints address content by hash, not by path, so they need `read` on the whole namespace: prefix `""`, not narrowed by a longer prefix. Partial read access is for path-addressed reads: S3 and the native file, listing, history and diff endpoints.
- **Writes** are checked on every changed path, through every protocol.
- **Unknown or forbidden.** A namespace in which you have no permission answers 404, like one that does not exist.

`f1d repo export-acl <ns> [file]` writes a namespace's ACL as TOML, and `f1d repo import-acl <ns> <file>` applies it on another server, skipping principals it does not know.

## 4.4 `f1d` {#f1d}

```
f1d <command> [flags]

  serve     run the server
  fsck      check a repository (offline)
  gc        collect garbage in a repository
  backup    back up server.db and every repository to a directory (incremental)
  repo      manage repositories: list, freeze, archive, import, attach, ...
  ns        create namespaces
  user      manage local users
  token     manage local API tokens
  s3key     create S3 access keys
  acl       manage ACLs
  version   print the version
```

| Command | Does |
|---|---|
| `serve` | run the server |
| `user add <name> [--group g]…`, `user list`, `user groups <name> [g…]`, `user disable\|enable <name>` | local users |
| `token create <user> [--expires d] [--description t]`, `token list [user]`, `token revoke <id>` | local tokens |
| `s3key create <user>` | an S3 access key |
| `acl set <ns\|*> <principal> <prefix> <perms>`, `acl rm …`, `acl list <ns\|*>` | ACLs; principals written in full |
| `ns create <name> [--preset s3\|svn\|git]` | a namespace |
| `repo list` | namespaces and their UUIDs |
| `repo freeze <ns>`, `repo unfreeze <ns>` | make a repository read-only for copying |
| `repo archive <ns> <file.zip>` | a ZIP of the repository; writes continue |
| `repo import <file.zip\|dir> [--name ns] [--new-uuid]` | add a repository from an archive or directory |
| `repo attach <ns>`, `repo detach <ns>` | register `repos/<ns>`, or release it for moving |
| `repo rebuild-cache <ns> \| --dir <dir>` | rebuild `cache.db` (offline) |
| `repo export-acl <ns> [file]`, `repo import-acl <ns> <file>` | move a namespace's ACL |
| `gc <ns> [--dry-run] [--grace d] [--list]`, `gc --dir <dir>` | garbage collection (§4.6) |
| `fsck <ns>`, `fsck --dir <dir>` | check a repository (§4.7) |
| `backup <dir>` | back up everything (§4.5) |

While the server runs, commands that act on repositories reach it through `<data_dir>/admin.sock` (mode 0600), so run them as the server's user with the same configuration. Without a running server they open the data directory directly. Identity and ACL commands work either way.

## 4.5 Backups, copies and moves {#backup}

### Backup and restore

```sh
$ f1d backup /backup/f1
code: 2 packs, 2 copied (968 bytes), 0 removed
design: 23 packs, 23 copied (342621 bytes), 0 removed
media: 1 packs, 1 copied (1034 bytes), 0 removed
backup of 3 repositories in /backup/f1
```

The backup is `server.db` (an online copy) and `repos/<ns>/` for each repository, without the rebuildable `cache.db`. It runs while the server serves; garbage collection waits.

- **Incremental.** Running it again copies only new packs and removes packs the server no longer has. Packs are immutable, so this is cheap.
- **Consistent.** An interrupted backup is still a usable one: a repository's refs in the backup change only after the packs they need are there.
- **Protect it.** `server.db` holds S3 secrets in plain text, because SigV4 needs them. Store backups on another disk or host, encrypted at rest.

**Restore everything** by using a copy of the backup as the data directory and starting `f1d`: each `cache.db` is rebuilt when its repository opens. **Restore one repository** with `f1d repo import /backup/f1/repos/design`, which copies it, rebuilds its cache, checks it and registers it; then restore its ACL.

### Archives and copies

A namespace is a self-contained directory. Copies taken these ways are always consistent:

| How | Writes during the copy |
|---|---|
| `f1d repo archive design design.zip` | continue; the ZIP holds the state when it started |
| `GET /api/v1/ns/design/archive` (admin) | continue; the same ZIP, streamed |
| `f1d repo freeze design`, copy `repos/design/` with any tool, `f1d repo unfreeze design` | refused while frozen: native `423 frozen`, S3 `503 SlowDown`, Git an error |
| copy with the server stopped | none |

`f1d repo import design.zip` restores an archive on any F1 server, with its full history. `--name` imports it under another name; `--new-uuid` gives it a new identity, for a second copy on the same server. Identities and ACLs are not part of a repository: move them with `export-acl` and `import-acl`.

### Moving a repository between servers

```sh
old$ f1d repo detach design          # flushes and releases the directory
old$ rsync -a /var/lib/f1/repos/design/ new:/var/lib/f1/repos/design/
new$ f1d repo attach design
new$ f1d repo import-acl design design-acl.toml
```

A directory placed in `repos/` while the server is stopped is registered when it starts.

## 4.6 Garbage collection {#gc}

Every `server.gc.interval` the server collects each repository. It removes:

- history beyond the retention policy;
- versions superseded in `retention = none` namespaces, and deleted S3 objects;
- uploads that were never committed, once older than `server.gc.grace`.

Collection marks what the refs, the retained history and open multipart uploads still use, and rewrites or deletes the packs that hold mostly garbage. It never blocks reads or writes. Objects written within the grace period are always kept, and old pack files linger for an hour so that reads in progress finish.

```sh
$ f1d gc design --dry-run
would delete 0 objects (0 bytes): 0 packs deleted, 0 rewritten; 0 unreachable objects kept in mostly live packs; 7 retained commits, 68 live objects (4ms)
$ f1d gc media --grace 1h --list
```

`--dry-run` reports without deleting, `--list` prints the deleted hashes, and `--grace` overrides the grace period. Frozen repositories, and repositories being archived or backed up, are skipped.

## 4.7 Checks and repair {#fsck}

```sh
$ f1d fsck design                     # server stopped
$ f1d fsck --dir /backup/f1/repos/design
```

`fsck` checks pack checksums and every object's hash, `cache.db` against the packs, that every reference points to a stored object (D9), reachability from the refs, and retention boundaries. Warnings concern `cache.db`, which the server repairs when it opens the repository; errors mean missing or corrupt data.

`cache.db` holds only derived data. `f1d repo rebuild-cache <ns>` rebuilds it from the packs, and deleting it has the same effect at the next open.

## 4.8 Monitoring {#monitoring}

`GET /healthz` answers `ok`. `GET /metrics` serves the Prometheus text format, without authentication; restrict it at the proxy if request rates are sensitive.

| Metric | Meaning |
|---|---|
| `f1_http_requests_total{api,code}` | requests per API (`native`, `s3`, `git`) and status class |
| `f1_http_request_duration_seconds{api}` | request durations (histogram) |
| `f1_http_request_bytes_total{api}`, `f1_http_response_bytes_total{api}` | body bytes |
| `f1_ref_updates_total` | commits, pushes and ref changes |
| `f1_packs_sealed_total`, `f1_objects_written_total`, `f1_pack_bytes_written_total` | writes |
| `f1_gc_runs_total{result}`, `f1_gc_deleted_objects_total`, `f1_gc_deleted_bytes_total` | garbage collection |
| `f1_repositories`, `f1_packs`, `f1_pack_objects`, `f1_pack_bytes` | storage, garbage included |
| `f1_namespace_packs`, `f1_namespace_pack_objects`, `f1_namespace_pack_bytes` | the same per namespace, if `server.metrics.namespaces` is on |

Useful alerts: a rise in `code="5xx"`, `f1_gc_runs_total{result="error"}`, and the disk filling (from the host's own metrics).

Logs are structured (`log/slog`); `log.format = "json"` suits log collectors.

## 4.9 Upgrades and performance {#upgrades}

**Upgrades.** Back up, stop `f1d`, replace the binaries and start it. `server.db` is migrated and each `cache.db` upgraded or rebuilt automatically. A repository whose format is newer than the server is refused with a message naming the version needed.

**Performance.** Reference numbers on a 6-core desktop machine, data on tmpfs (`doc/benchmarks.md`):

| Benchmark | Result |
|---|---|
| Ingest of 64 MiB files | 435 MB/s |
| 1 KiB writes from 24 clients, one namespace | 2,657 per second |
| S3 GET of a 4 KiB object | 472 µs |
| Sequential read of a 64 MiB file | 1,628 MB/s |
| `git clone` of 300 commits over 400 files | 529 ms, 0.43 MiB pack |
| History of a file changed once, golang/go (68,110 commits) | about 1 s (18.6 s for the first query) |
| Creating a branch, golang/go | under 10 ms (up to 4.2 s the first time) |

`make bench` runs the benchmarks and `make bench-check` compares them with the recorded baseline.

**Memory** is bounded whatever the file size: in the size tests, a 10 GiB file passes through every protocol with the server below 100 MiB and the client below 512 MiB.

## 4.10 Limits {#limits}

| Item | Limit |
|---|---|
| File size | none (tested to 10 GiB through every protocol; 64-bit sizes) |
| Chunks | 16 KiB to 256 KiB, 64 KiB on average |
| Pack files | 64 MiB |
| S3 user metadata | 2 KB |
| S3 multipart parts | at least 5 MiB, except the last |
| S3 presigned URLs | 7 days |
| Open multipart uploads | `server.s3.upload_expiry` (7 days) |
| Group commits | up to 20 ms or 1,000 writes each |
| Directory size | none: trees are B-trees, a directory or bucket of millions of keys updates in O(log n) |

## 4.11 Not in version 1 {#not-in-v1}

- **Scale-out**: one server, local disk and SQLite. No clustering, no S3 or other remote storage for packs.
- **Storage of code histories**: small files are stored whole per version, so a code repository with a long history takes 3 to 6 times the space of a Git pack. Delta-encoded storage is proposed (ADR-0013).
- **Git**: thin fetch packs against the client's objects; SHA-256 repositories.
- **S3**: bucket policies and ACLs, lifecycle rules, object lock, encryption settings, tagging, website hosting, Select, replication.
- **Clients**: OIDC login in `f1` (tokens come from the environment), branch merges in the native client, a FUSE file system, a web UI.
- **Protocols**: the SVN wire protocol; SVN users move to the native client, Git or S3.
- **Hosting on Windows.** The server runs on Unix systems; clients run anywhere.
