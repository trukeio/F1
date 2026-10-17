# F1 administration guide

This guide covers installing and running `f1d`: identities and access, namespaces, the clients, backups, garbage collection and monitoring. The design is in `doc/plan.md`, the decisions in `doc/adr/`, and the on-disk format in `doc/format.md`.

## Install

F1 is two static binaries:
- `f1d`: the server and its admin CLI.
- `f1`: the native client.

The server runs on Linux, macOS and the BSDs. Clients run anywhere.

- **From source:** `make build` writes `bin/f1d` and `bin/f1` (Go 1.25).
- **systemd:** `deploy/f1d.service` runs f1d as user `f1` with its data in `/var/lib/f1` and a hardened sandbox. The unit's header lists the installation steps. `deploy/f1d.toml` is a matching configuration.
- **Container:** `docker build -f deploy/Dockerfile -t f1 .`, then:
  ```
  docker run -d --name f1 -p 8080:8080 -p 9000:9000 -v f1-data:/var/lib/f1 f1
  ```
  Admin commands run inside the container:
  ```
  docker exec f1 f1d user add --config /etc/f1/f1d.toml alice
  ```

**TLS.** F1 serves plain HTTP. Put it behind a TLS-terminating reverse proxy (nginx, Caddy, HAProxy). The proxy must:
- pass the `Host` header unchanged (S3 signatures cover it);
- allow large request bodies and long requests (uploads of any size);
- not buffer responses (Git and downloads stream).

## Configuration

`f1d serve --config f1d.toml` reads one TOML file. Unknown keys are errors. `example.toml` documents every key:

| Key | Default | Meaning |
|---|---|---|
| `server.data_dir` | `data` | `server.db` and `repos/` (relative to the config file) |
| `server.listen` | `127.0.0.1:8080` | Native API, Git, `/metrics`, `/healthz` |
| `server.s3.listen` | `127.0.0.1:9000` | S3 API; empty disables it |
| `server.s3.region`, `domain`, `create_bucket`, `upload_expiry`, `[[server.s3.alias]]` | | S3 settings (ADR-0007) |
| `server.gc.interval` | `24h` | Garbage collection of every repository; `0` disables it |
| `server.gc.grace` | `24h` | Unreachable objects newer than this are kept |
| `server.metrics.enabled` | `true` | Serve `/metrics` (unauthenticated) |
| `server.metrics.namespaces` | `false` | Per-namespace storage metrics (reveal namespace names) |
| `[[auth.oidc]]` | none | Trusted OIDC providers (ADR-0005) |
| `auth.s3_group_ttl` | `24h` | How long an OIDC principal's groups apply to its S3 keys |
| `log.level`, `log.format` | `info`, `text` | `debug`, `info`, `warn`, `error`; `text` or `json` |

### Admin commands while the server runs

Admin commands that act on a running server talk to it over `<data_dir>/admin.sock` (mode 0600). Run them as the server's user, with the same configuration file. Without a running server they open the data directory directly.

## Identities and access

- **Local users and tokens.**
  - `f1d user add alice --group eng` creates a user.
  - `f1d token create alice` prints a bearer token (`f1_...`), once.
  - `f1d token list` and `f1d token revoke <id>` manage tokens.
  - `f1d user disable alice` blocks all of a user's tokens and keys.
- **OIDC.** Add an `[[auth.oidc]]` section with the issuer, audiences and claims. The users and groups of the enterprise identity provider can then use their access tokens directly (ADR-0005). Principals are `user:<sub>` and `group:<group>`.
- **S3 keys.**
  - `f1d s3key create alice` creates a key for a local user.
  - Any authenticated user, including OIDC users, can create their own with `POST /api/v1/me/s3keys`.
  - Secrets are stored in plain text in `server.db`, because SigV4 needs them. Protect the data directory and its backups accordingly.
- **ACLs.** `f1d acl set <namespace|*> <principal> <prefix> <perms>` grants `read`, `write`, `lock` and/or `admin` on paths starting with `<prefix>`.
  - The longest prefix wins; `none` denies.
  - `f1d acl set '*' user:root "" admin` makes a server administrator.
  - Git, LFS, the native object endpoints and the native client need read on the whole namespace (D13, prefix `""`).
  - ADR-0006 has the exact rules. `f1d repo export-acl` and `import-acl` move a namespace's ACL between servers.

## Namespaces

A namespace is a repository: a self-contained directory `repos/<ns>/`.

- **Creating.** Server administrators create namespaces with `f1d ns create <name> --preset s3|svn|git`, through `POST /api/v1/ns`, or with S3 `CreateBucket` when `create_bucket` is on.
- **Presets.**

  | Preset | Layout | Retention | Other settings |
  |---|---|---|---|
  | `s3` | flat | `none` | autocommit, no branches |
  | `svn` | tree | `all` | required locks for `*.sch,*.brd,*.docx` |
  | `git` | tree | `all` | Git exposure |

- **Configuration.** A namespace's configuration is in `repos/<ns>/f1repo.toml`:
  - `layout`, `retention`, `autocommit`, `branches` and `locks`;
  - `git` and `default_ref`;
  - `[merge]`: globs mapped to `text` or `binary` for the native client.

  Edit the file while the repository is detached, or with the server stopped.
- **Retention.** `retention` is `all`, `last:N` (commits per ref), `days:N` or `none`. Git namespaces need `all` (D12). History beyond the retention is removed by garbage collection.

## Clients

- **Native client** (`f1`, for large binary repositories):
  - `f1 clone [--sparse dir] <server-url> <namespace>`, with the token in `F1_TOKEN`;
  - then `status`, `add`, `commit`, `push`, `pull`, `lock`, `unlock` and `diff`;
  - `f1 help` lists the commands. ADR-0010 has the details.
- **S3:** AWS CLI, SDKs, rclone and mc, with path-style or virtual-host addressing.
- **Git:** `git clone http://host:8080/git/<namespace>.git`, with any user name and the token as the password. LFS works against the same URL.

## Web viewer

`f1web` is a separate server that shows namespaces, files and history in a browser (ADR-0018). It calls the native API as the signed-in user, so users see exactly what their ACLs allow. `deploy/f1web.toml` is an example configuration.

- **Run** `f1web --config f1web.toml` next to `f1d`, behind the same TLS-terminating proxy. With an `https` `public_url`, the session cookie is `Secure`.
- **Sign-in with OIDC.** Register `f1web` at the identity provider as a confidential client with redirect URI `<public_url>/auth/callback`. Its access tokens must be accepted by F1, so they must carry F1's audience (`f1`):
  - Keycloak: add an audience mapper for `f1` to the client;
  - Entra ID: request the F1 API's scope (`api://f1/.default`), plus `offline_access` for refresh tokens.

  `f1web` refreshes access tokens with the refresh token. Without one, the user signs in again when the access token expires.
- **Sign-in with a token.** With `token_login = true`, users can paste an F1 token instead; local users have no other way.
- **Sessions** are kept in memory and end after `session_idle` without a request, at sign-out, or when `f1web` restarts. Tokens stay in `f1web`; the browser holds only a session ID.

## Operations

### Backup and restore

`f1d backup <dir>` writes:
- `<dir>/server.db`, an online copy;
- `<dir>/repos/<ns>/`, each repository without `cache.db`.

It works while the server runs; writes continue, and garbage collection waits.

**Running it again** updates the backup incrementally: it copies only new packs and removes packs the server no longer has. A backup interrupted at any point is consistent. Back up to a different disk or host, and encrypt it at rest, since `server.db` holds S3 secrets.

**Restore** the whole server by using a copy of the backup directory as the data directory and starting f1d: each `cache.db` is rebuilt on open. To restore one repository, run `f1d repo import <dir>/repos/<ns>`, which copies it, rebuilds its cache, runs fsck and registers it. Then restore its ACL with `import-acl`, or restore `server.db`.

### Copies, archives and moves

- **`f1d repo archive <ns> <file.zip>`** writes a ZIP while writes continue. `f1d repo import <file.zip>` restores it on any server.
- **`f1d repo freeze <ns>`** makes a repository read-only so that its directory can be copied with any tool; `f1d repo unfreeze <ns>` ends it.
- **Moving.** `f1d repo detach <ns>` releases a directory. On the target server, place the directory in `repos/` and run `f1d repo attach <ns>`.

### Garbage collection

The server collects every repository each `server.gc.interval`:
- history beyond the retention policy;
- uploads that were never committed, once older than `server.gc.grace`;
- deleted S3 objects;
- superseded versions in `retention = none` namespaces.

Run it on demand with `f1d gc <ns>`. `--dry-run` reports what would be deleted, `--list` prints the hashes, and `--grace 1h` overrides the grace period. Frozen repositories, and repositories being archived or backed up, are skipped. Deleted pack files linger for an hour (in-flight reads) before they are removed. ADR-0011 has the algorithm.

### Checks and repair

- **`f1d fsck <ns>`** (server stopped, or with `--dir <path>` on a copy) checks:
  - pack checksums and payload hashes;
  - `cache.db` against the packs;
  - references (D9) and reachability from the refs;
  - retention boundaries.

  Warnings are about derived state (`cache.db`), which the server repairs when it opens the repository. Errors mean missing or corrupt data.
- **`f1d repo rebuild-cache`** rebuilds `cache.db` from the packs. Deleting `cache.db` has the same effect at the next open.

### Monitoring

`GET /healthz` answers `ok`. `GET /metrics` serves Prometheus text:

| Metric | Meaning |
|---|---|
| `f1_http_requests_total{api,code}` | Requests per API (`native`, `s3`, `git`) and status class |
| `f1_http_request_duration_seconds{api}` | Request durations (histogram) |
| `f1_http_request_bytes_total{api}`, `f1_http_response_bytes_total{api}` | Body bytes |
| `f1_ref_updates_total` | Commits, pushes and ref changes |
| `f1_packs_sealed_total`, `f1_objects_written_total`, `f1_pack_bytes_written_total` | Writes |
| `f1_gc_runs_total{result}`, `f1_gc_deleted_objects_total`, `f1_gc_deleted_bytes_total` | Garbage collection |
| `f1_repositories`, `f1_packs`, `f1_pack_objects`, `f1_pack_bytes` | Storage, garbage included |
| `f1_namespace_packs`, `f1_namespace_pack_objects`, `f1_namespace_pack_bytes` | The same per namespace, if `server.metrics.namespaces` is on |

`/metrics` needs no authentication. Restrict it at the proxy if the request rates are sensitive. Logs are structured (`log/slog`); `log.format = "json"` suits log collectors.

### Upgrades

Stop f1d, replace the binaries and start it. Schema changes are applied automatically:
- `server.db` is migrated;
- `cache.db` is upgraded or rebuilt.

A repository whose `f1repo.toml` has a newer `format` than the server supports is refused with a message naming the required version. Back up before upgrading.

### Capacity and performance

- **Storage.** Repositories deduplicate content within themselves, not across repositories (D6). Packs are 64 MiB and immutable.
- **Disks.** Use SSDs with power-loss protection: every commit fsyncs about six times (doc/plan.md, "Known limitations").
- **Benchmarks.** `doc/benchmarks.md` has reference numbers and how to compare a build against them.
