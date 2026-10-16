# ADR-0016: HTTP caching of immutable reads

- Status: accepted
- Date: 2026-10-16
- Deciders: Rolf Veen

## Context

Some native API responses can never change:

- `GET /ns/{ns}/objects/{hash}` returns an object addressed by its hash.
- `GET /ns/{ns}/files/{ref}/{path}` returns a file whose content is determined by the commit, when `ref` is a commit hash.

Scion ("Redesigning source control at Anthropic", 2026) builds its serving tiers on this property: every request is for an immutable object, so any cache can answer it.

F1 is a single server and has no caching tier. Standard HTTP caches can still use the property, for example a browser, an HTTP client library with a cache, or a private caching proxy in front of CI runners. For that, the responses must say they are immutable.

Today, `GET files` sets `ETag` to the File hash, and `http.ServeContent` already answers `If-None-Match` with `304` and handles `Range` and `If-Range`. No response sets `Cache-Control`, and `GET objects/{hash}` has no `ETag` and no conditional handling.

## Decision

1. **Reads by commit hash.** `GET` and `HEAD /files/{ref}/{path}`, when `ref` is a full commit hash (64 hex digits), send `Cache-Control: private, max-age=31536000, immutable`.
2. **Reads by ref name.** The same endpoints with a ref name send `Cache-Control: private, no-cache`. A cache may store the response but must revalidate it. Revalidation sends `If-None-Match` with the File hash and gets `304` when the file is unchanged, without content.
3. **Objects.** `GET /objects/{hash}` sends `ETag: "<hash>"` and the immutable `Cache-Control` of item 1.
   - It answers a matching `If-None-Match` with `304`, after the permission check, and only if the object is still stored.
   - An object removed by GC is `404` with or without the header.
4. **`private` everywhere.** Responses depend on the caller's permissions (path ACLs, D13), so a shared cache must not serve one caller's response to another. RFC 9111 §3.5 already keeps shared caches from storing responses to requests with `Authorization` unless told otherwise. `private` makes this explicit.
5. **Not cached:** `tree`, `log`, `diff`, refs, locks and namespace listings. Their results are filtered by ACLs that can change, and `log` and `diff` at fixed commits can change when retention removes history. They keep the default: no `Cache-Control`.
6. **Unchanged:**
   - the S3 API, which follows S3's own `Cache-Control` and `ETag` semantics, with stored headers per object;
   - Git, whose responses are `no-cache` as today;
   - the native client, which keeps no pristine copies (ADR-0010, item 5).

## Consequences

- HTTP clients with a cache, and private caching proxies, serve repeated reads of pinned commits and objects without contacting F1. Reads by ref name cost a `304` round trip instead of the content.
- The gain is modest, but the cost is a few lines per handler and tests of the headers and `304` responses.
- **"Immutable" means the bytes never change, not that they stay available.**
  - A cached copy of content later removed by retention remains in that cache.
  - A user whose read permission is revoked keeps what their own cache already holds, as with any local copy.
  - F1 has no purge operation, so neither case is a new exposure.
- No format change, and no effect on other protocols.

## Alternatives considered

- **`public` caching for unauthenticated namespaces.** F1 has no anonymous read access in version 1. Rejected until it does.
- **Built-in cache tiers, like Scion's regional caches.** These are for multi-node deployments, which are a version 1 non-goal. The headers decided here are what such a tier would rely on later.
- **ETag and conditional requests for `tree`, `log` and `diff` at commit hashes.** These would need an ETag that includes the caller's effective permissions and the retention state. The complexity is not justified by the use. Rejected.
