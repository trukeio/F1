# To do

Open items that are not part of the plan's phases. Each one ends in an ADR or a measurement in `doc/`.

- [ ] **Evaluate the overlap between G1 (`../g1`) and F1.** Find what both implement or could share: identity and OIDC (ADR-0005 already shares the provider), ACLs, storage, APIs and clients. Decide what to share, merge or keep apart.
- [ ] **Evaluate an SVN wire protocol.** It is a version 1 non-goal (`doc/plan.md`, "Non-goals"): SVN users move to the native client, Git or S3. Assess what serving `svn` clients directly would take (ra_svn or WebDAV/DeltaV), and what it would bring to SVN-style namespaces.
- [ ] **Measure fast file access for web sites.** Measure serving a site's files straight from F1 (native API and S3 GET, with the HTTP caching of ADR-0016): latency and throughput for many small files, warm and cold, and behind a caching proxy. Record the results in `doc/benchmarks.md`.
  - [x] Native GET of 10 kB, 100 kB and 500 kB files against Go's `http.FileServer`, warm, cold and conditional (`BenchmarkSite`; `doc/benchmarks.md`, "Web site files"). F1 is 3.6–4.6 times slower per file, mostly from a fixed cost of about 200 µs per request.
  - [ ] S3 GET, and behind a caching proxy.
  - [x] Prepare the per-request `server.db` queries: −19 % for 10 kB reads, −35 % for a 304.
  - [ ] Decide on the other findings: an in-memory cache of tokens and ACLs (it must notice changes written by other processes, for example through `PRAGMA data_version`), anonymous reads, `private` responses that shared caches do not store, and content types.
