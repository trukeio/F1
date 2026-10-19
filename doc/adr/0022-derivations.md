# ADR-0022: Derived files: previews in f1web, build outputs outside the commit

- Status: proposed
- Date: 2026-10-19
- Deciders: Rolf Veen

## Context

Many files in a repository have derived forms: a PDF of a Word or LibreOffice document, a PNG of a TIFF scan or a schematic, a rendered README, a firmware image built from source, a documentation site. Users either commit these next to their sources, or expect the host to produce them. `TODO.md` asks for one case: open PDFs and other binary documents in the viewer, without another window and without forcing a download.

Committing derived files has known costs:

- **Staleness.** Nothing keeps the derived file in step with its source. A commit that changes one but not the other is accepted silently.
- **Noise and conflicts.** Generated diffs hide the real change in review, and two branches that regenerate the same file conflict over bytes nobody edits.
- **Storage.** Binary outputs (PDF, images, firmware) change completely on every regeneration, and F1 stores every version whole (`doc/plan.md`, "Known limitations"). With `retention = all`, which Git namespaces require (D12), they are kept forever.
- **Commit-back loops.** A CI job that commits its output fires another ref update, and with it another event (ADR-0021).

Since objects are content-addressed, a derived file is a function of its input's hash and of the program that made it. It can be computed when needed, kept as long as convenient, and dropped at any time. F1 already treats derived data this way: `cache.db` is rebuildable (D7), the commit graph is computed on demand (ADR-0015), and reads by hash are immutable (ADR-0016).

Constraints:

- F1 is a storage system. ADR-0021 (item 1) keeps job execution, logs and artifacts out of it.
- `f1web` holds no credential that could read more than the signed-in user can (ADR-0018, item 3), and the content it serves from user files is inert (ADR-0018, item 8).
- Path read ACLs (D13) decide who can read a file. A derived file must not be readable by anyone who cannot read its source.
- Retention and GC (ADR-0011) may remove sources. Their derived files must then become unreachable.
- New dependencies need the user's approval (`doc/plan.md`, "Dependencies").

## Decision

### Principle

1. **Derived files are not committed by default.** They are produced in one of two places, depending on their input:
   - **Per-file derivations** (one file in, one file out: previews, conversions, thumbnails) are produced by `f1web` when a page needs them, and kept in a disposable cache (items 3–13).
   - **Per-commit derivations** (a tree in, outputs out: builds, documentation sites, test reports) are produced by CI from the events of ADR-0021, stored outside the source namespace, and linked from commit statuses (items 14–16).

   Committing a derived file stays possible and is the right choice in the cases of item 17.

2. **F1 itself does not change.** This ADR adds nothing to `f1d`, the native API, `server.db` or the on-disk format. Everything is in `f1web`, in conventions for CI, and in the manual.

### Transformers in f1web

3. **A transformer** turns one file into one derived file. It is configured in `f1web.toml`:

   ```toml
   [[derive.transformer]]
   name    = "pdf"                         # used in URLs and in the cache key
   version = "lo-24.8"                     # bumped by the administrator to invalidate outputs
   match   = ["*.docx", "*.xlsx", "*.pptx", "*.odt", "*.ods", "*.odp", "*.rtf"]
   output  = "application/pdf"
   command = ["soffice", "--headless", "--convert-to", "pdf", "--outdir", "{out}", "{in}"]
   result  = "{out}/{stem}.pdf"            # where the command leaves its output
   max_input = "64MiB"
   timeout   = "60s"
   ```

   - `match` globs are matched against the file's base name, case-insensitively. The first transformer that matches is the file's default preview. A file may be matched by several; the others are offered as links.
   - Transformers are external commands. `f1web` gains no conversion code and no dependency. The manual gives tested configurations (LibreOffice for office documents, ImageMagick or libvips for TIFF and HEIC, Inkscape for some vector formats).

4. **Outputs are inert types only.** `output` must be `application/pdf`, `image/png`, `image/jpeg`, `image/webp` or `text/plain`. HTML and SVG are not allowed, because a transformer fed with a crafted file could produce active content. The configuration is refused at startup otherwise.

5. **The cache key** is BLAKE3 of the canonical CBOR encoding of `[name, version, command, result, File hash]`.
   - The File hash is the `ETag` of `GET files` (ADR-0016). It identifies the content, so the same file in two namespaces, on two branches or in two commits shares one output.
   - Different chunk boundaries can give the same content two File hashes (`doc/plan.md`, "Objects"). That costs a second conversion, never a wrong output.
   - `command` and `result` are part of the key, so changing a transformer's arguments invalidates its outputs without a `version` bump. `version` covers changes the configuration does not show, such as an upgraded converter.

### Access

6. **Every request is checked against F1 as the user.** To serve a derived file, `f1web` resolves the ref to a commit and makes a `HEAD /files/{commit}/{path}` call with the user's token. A 404 or 403 is returned to the browser as it is. Only a successful call yields the File hash, and only then is the cache consulted.
   - So a user gets the output only for content they can read now. Path ACLs (D13) and the 404 rule apply unchanged.
   - Outputs are shared between users safely: the key is derived from content the user has just been shown to have access to, and identical content gives an identical output.
   - Content that retention has removed fails the `HEAD` call, so its outputs become unreachable at once, and are evicted later (item 9).
   - The check costs one API call, about 200 µs (`doc/benchmarks.md`, "Web site files"), per request.

### Running

7. **Isolation.** Converters parse untrusted files and have a history of vulnerabilities, so each run is contained:
   - The input is streamed from F1 into a new private temporary directory, which is the working directory and `HOME`. Everything is removed after the run.
   - The command runs without stdin, with an environment of `PATH`, `HOME` and `TMPDIR` only, in its own process group. The group is killed at `timeout`.
   - `[derive] wrapper` (a list of arguments put before every command) runs commands inside a sandbox. The manual gives a `bwrap` configuration without network, with a read-only system and only the temporary directory writable. Without a wrapper, `f1web` logs a warning at startup.
   - Output above `max_output` (default 256 MiB) is a failure.

8. **Concurrency.** At most `[derive] concurrency` runs (default 2) are active. Requests for a key already being computed wait for that run (single flight). A request that waits more than 5 s gets a page saying the preview is being generated, which reloads itself with `<meta http-equiv="refresh">`, so it works without JavaScript.

9. **The cache** is a directory, `[derive] cache_dir`, with one file per key named by its hex.
   - Outputs are written to a temporary name, fsynced and renamed, so a crash leaves no partial output.
   - A hit updates the file's modification time. When the directory exceeds `cache_max` (default 10 GiB), the least recently used files are removed until it is at 90 %.
   - Failures are cached as small entries with the command's exit status and the last 4 kB of its standard error, for `failure_ttl` (default 1 h), so a file that cannot be converted is not converted again on every view. Administrators see the error in the log; users see "no preview available" and the download link.
   - The cache is disposable: deleting the directory loses nothing but time. Several `f1web` instances may share it, since writes are atomic renames of complete files.

10. **Sizes.** `max_input` limits what a transformer converts, not what F1 serves. Larger files keep the download link only. This is the same reading of D19 as `max_render` (ADR-0018, item 12).

### Pages

11. **Route.** `/{ns}/derive/{transformer}/{ref}/{path}` serves the output of a transformer for a file.
    - Responses carry `ETag: "<cache key>"`. With a commit hash as `ref`, they carry `Cache-Control: private, max-age=31536000, immutable`; with a ref name, `private, no-cache`, as in ADR-0016, items 1 and 2.
    - Responses have the headers of `/raw` (`Content-Security-Policy` with `sandbox`, `X-Content-Type-Options: nosniff`) and `Content-Disposition: inline` with the source's stem and the output's extension.

12. **The file page** shows the default preview of a matched file in place of "download". Images are shown inline, text as text and PDFs in an `<iframe>`. Links to the other transformers' outputs and to the raw file are kept.

13. **PDFs in the viewer** (the `TODO.md` item) need no transformer: the file page shows the PDF itself the same way, through `/raw`.
    - `sandbox` in the CSP turns off the built-in PDF viewer in some browsers. V1 tests the current Chrome, Firefox and Safari.
    - If a PDF response cannot keep `sandbox`, `f1web` serves PDFs, and derived outputs, from a second origin, `[derive] content_url`, that holds no session cookie. Content from user files then never runs on `f1web`'s origin. This is the same separation GitHub uses for `raw.githubusercontent.com`. Without `content_url`, PDFs keep `sandbox` and the download link, in the browsers where the viewer does not work.

### Per-commit derivations

14. **CI produces them.** A coordinator follows the event feed or receives webhooks (ADR-0021), checks out the commit, builds, and stores the outputs. F1 runs nothing.

15. **Outputs go to a separate namespace, by convention.** The manual recommends one namespace per source namespace, `{ns}-builds`, with `layout = flat` and `retention = none`, and keys `{commit hash}/{output path}`.
    - Uploads use any protocol, typically S3 (`aws s3 cp`) or `f1 put`, with a CI user that has `write` on the builds namespace and only `read` and `status` on the source.
    - The builds namespace has its own ACLs. Its readers must be a subset of the source's whole-namespace readers, since a build reads the whole tree. The manual says so; F1 does not enforce it.
    - Keeping only the last N builds is the CI's job: it deletes old prefixes. A lifecycle rule in F1 is an S3 non-goal (`doc/plan.md`, "Non-goals").
    - The source namespace is never written by CI, so there are no commit-back loops, and the source history holds no build outputs.

16. **Statuses link them.** The CI sets a commit status (ADR-0021, item 10) whose `target_url` is the output's page in `f1web`, or the listing of its `{commit hash}/` prefix. `f1web` gains nothing for this: the status links are already shown next to the commit.

### Committed derived files

17. **Commit a derived file only when one of these holds:**
    - Consumers cannot run the generator: generated Go code that `go get` must find, or a datasheet PDF that users of a design namespace open with the native client or S3 rather than in `f1web`.
    - The output is reviewed as content: a generated schema, a lockfile, a netlist whose diff is the real change.
    - It is a release snapshot that must not change when a converter is upgraded: a signed or approved PDF.
    - The generator may not be available later: a proprietary tool, or a licence that expires.

    In these cases, a CI check regenerates the file and fails a status if the committed version differs, which addresses staleness.

18. **Generated files are collapsed in diffs.** `f1web` reads the root `.gitattributes` of the commit being diffed, and collapses files with the attribute `linguist-generated` set, as GitHub does, with a link to expand them. Only `linguist-generated`, `-linguist-generated` and `linguist-generated=true|false` on path patterns are understood. Other attributes, macros and nested `.gitattributes` files are ignored.

### Prewarming

19. **`f1web derive --follow <ns>...`** is a separate process that computes previews ahead of the first view. It follows the event feed (ADR-0021, item 3), lists the files each new commit changes (`diff` with the first parent), and runs the matching transformers into the shared `cache_dir`.
    - It uses its own token, set in its configuration, with `read` on the listed namespaces. The `f1web` server still holds no credential (ADR-0018, item 3). A cache filled by a more privileged process leaks nothing, because every request is checked as the user (item 6).
    - It is optional. Without it, previews are computed on first view.

### Documentation

20. **The manual gains a chapter, "Derived files"**, covering:
    - the choice between items 1, 15 and 17;
    - transformer configurations for LibreOffice, ImageMagick and libvips, with a `bwrap` wrapper;
    - the builds namespace pattern, with an example CI step that uploads outputs and sets a status;
    - the up-to-date check of item 17;
    - `linguist-generated`.

### Phases

21. Each phase passes `make ci`.
    - **V0:** this ADR.
    - **V1:** PDFs in the file page (item 13), with the browser tests and, if needed, `content_url`.
    - **V2:** transformers, cache and isolation (items 3–11), and previews in the file page (item 12). Tests use a shell-script transformer. They cover:
      - the access check (a user without path read gets 404, even with the output cached);
      - single flight;
      - timeouts and output limits;
      - cached failures;
      - eviction;
      - a key change on a configuration change.
    - **V3:** collapsing generated files in diffs (item 18).
    - **V4:** `f1web derive --follow` (item 19), after ADR-0021's C2.
    - **V5:** the manual chapter (item 20).

    Not included:
    - Derivations from several files in `f1web`, such as a document assembled from includes. They are per-commit derivations (item 14).
    - Transformers that produce HTML or SVG.
    - Storage of derived files inside F1, or links between a source namespace and its builds namespace in F1's configuration.

## Consequences

- Office documents, scans and other binary files can be viewed in the browser without a download, and PDFs open in place.
- Source histories stay free of generated binaries. With `retention = all`, this is the only way their storage stays bounded.
- No change to `f1d`, the API, `server.db`, the on-disk format or golden vectors. Copied and archived repositories carry no derived files, and lose nothing by it.
- `f1web` runs external programs on user content for the first time. This is the main new risk. Isolation (item 7) contains it, but its strength depends on the wrapper the administrator configures. Without one, a converter exploit runs as the `f1web` user, which holds the sessions and tokens of everyone signed in.
- `f1web` gains a disk cache to size and monitor, and a CPU load proportional to first views (or to commits, with prewarming).
- Previews are only as stable as their converters: upgrading LibreOffice can change a rendering. A document whose exact rendering matters is committed as a PDF (item 17).
- Per-commit outputs rely on conventions, not F1 features. Two namespaces, ACLs that must be kept consistent by hand, and cleanup by CI are the price of keeping F1 free of artifacts.

## Alternatives considered

- **Commit derived files by default**, regenerated by CI or by hooks on the client. Simple, and works offline and with every protocol. It has all the costs listed in Context, and with `retention = all` they are permanent.
- **A derivation store in F1** (outputs as objects in the source repository, under a hidden ref or in `cache.db`). Outputs would travel with the repository and be shared by all clients. But conversions would run in the storage server, or F1 would accept writes of objects not reachable from any commit. GC would need a new kind of root, and the ACL of an output would have to be computed from its inputs. ADR-0021 kept artifacts out of F1 for the same reasons.
- **Conversion in the browser** (pdf.js, JavaScript converters for office formats). It needs no server process, but it needs vendored scripts and a JavaScript build. Office formats have no browser converter good enough. Only PDFs benefit, and browsers already display them.
- **An external conversion service** (Gotenberg, Collabora Online, OnlyOffice). These are good converters, and an administrator can use one: a transformer's `command` can be `curl` to such a service. Making one a requirement would add a deployment for every installation.
- **Keys by path and commit instead of by content.** Simpler to read, but every commit would invalidate every preview, and the same file on two branches would be converted twice.
- **No access check on cache hits** (the cache key as a capability in the URL). It saves an API call per request, but a URL would keep working after its user lost read access, or after retention removed the source. 200 µs is cheap for a check that keeps D13 exact.
