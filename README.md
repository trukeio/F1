# F1

F1 is one storage system for files of every size, with full history. It replaces SVN-style repositories of large binaries, Git hosting and S3-style object storage with one storage model and three ways in: its own client and HTTP API, Git (with Git LFS), and the S3 API.

The same repository answers every protocol. A file pushed with Git can be read with an S3 `GetObject`, and a file uploaded with the AWS CLI shows up in `f1 pull` and in `git fetch`.

## Why F1

Subversion and Git each solve half of the problem of keeping files with history.

**Subversion**

- ✔ **File size does not matter.** A working copy holds only the current version of the files it checks out, so a repository of circuit boards, CAD models or videos of hundreds of gigabytes works as well as one of text files.
- ✔ **Commits go straight to the server.** There is one copy of the history, on the server, and a commit is refused only if one of its own files is out of date. Locks keep two people from editing a file that cannot be merged.
- ✘ **The directory structure expresses the branches.** `trunk/`, `branches/` and `tags/` are ordinary directories, and a branch is a copy of one. Which directories are branches is a convention, not part of the model, and every tool and user has to know it.

**Git**

- ✔ **Branches and tags are refs**, names for commits, separate from the files. Branching, merging and comparing are cheap and well defined.
- ✔ **The ecosystem**: every developer, editor, CI system and code review tool speaks Git.
- ✘ **Big files are a problem.** A clone downloads every version of every file, and the repository's size grows with each change to a large binary. Git LFS moves the content elsewhere, at the cost of a second system with its own rules, and a file must be marked for LFS before it is first committed.

**F1 takes the strengths of both.**

- From Subversion: no behavior depends on file size, sparse checkouts, locks (required for unmergeable formats, as `svn:needs-lock`), and commits that go straight to the server and are rebased there when they touch no file changed in the meantime.
- From Git: branches and tags are refs, not directories; the server never merges file content; and a namespace can be served as a Git repository, with commit and tree SHAs that round-trip exactly, partial clone and Git LFS.
- Storage that suits both: content is cut into chunks of about 64 KiB and stored once per repository, so a small edit to a 2 GB file stores only the chunks it touched.

**And it adds S3 for flat repositories.** Not every set of files is a project. Backups, build outputs, media libraries and application data are keys in a bucket, written by programs. An F1 namespace with `layout = flat` is an S3 bucket: keys are opaque strings, as in S3, writes are last-writer-wins, and history can be kept in full, for N commits or days, or not at all. The AWS CLI, the AWS SDKs, rclone and MinIO `mc` work against it, and the same ACLs, locks and backups apply as to every other namespace.

## Components

| Component | Role |
|---|---|
| `f1d` | the server and its administration commands: the native API, Git and LFS on one port (default 8080), the S3 API on another (default 9000) |
| `f1` | the native client: working copies, sparse checkouts, local commits, pull with a three-way merge, push, locks and branches; made for large binaries |
| `f1web` | a web interface for browsing namespaces, files and history, and for administration, as a client of the native API |

Each namespace is one self-contained directory: a copy or a ZIP of it is a working repository, with its history, on any F1 server. Version 1 runs on one Linux, macOS or BSD host with local disk and SQLite; clients run anywhere, including Windows.

## Getting started

F1 needs Go 1.25.

```sh
$ git clone https://github.com/trukeio/F1 && cd F1
$ make build                      # bin/f1d, bin/f1, bin/f1web
$ bin/f1d ns create design --preset svn --config example.toml
$ bin/f1d serve --config example.toml
```

The namespace presets are `svn` (large binaries with history and locks), `git` (source code) and `s3` (object storage). The [user manual](doc/manual.md) continues from here: a first session, each client, and operating a server. A Subversion repository can be imported with its full history with `f1d repo import-svn` (manual §2.7).

## Documentation

- [`doc/manual.md`](doc/manual.md): the user manual.
- [`doc/plan.md`](doc/plan.md): the specification and implementation plan, with the fixed decisions.
- [`doc/adr/`](doc/adr/): architecture decision records.
- [`doc/format.md`](doc/format.md): the on-disk format.
- [`doc/admin.md`](doc/admin.md), [`doc/benchmarks.md`](doc/benchmarks.md): administration and measurements.

## License

MIT; see [`LICENSE`](LICENSE).
