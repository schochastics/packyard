# Packyard: implementation plan for internal-package hosting (v1.3 / v1.4)

## Context

v1.0–v1.2 shipped the core server: publish, channels, CAS, tokens, UI, bundle import and lazy proxy channels (see [implementation.md](implementation.md)). This plan narrows packyard to its first production use.

**Scope: a repository for an organisation's internal R packages.**

- CRAN packages keep coming from Posit Package Manager.
- Deployments are single-instance, one per client, run as a service in cynkra's managed infrastructure.
- **One Linux distribution per deployment**, either Ubuntu (`jammy`) or RHEL 9 (`rhel9`; our images are AlmaLinux 9). There is never more than one.
- **Every R version the client's images install** gets binaries: typically four to five R minor versions (currently 4.2–4.6). The list is rendered into `matrix.yaml` by the deployment tooling.
- Stock R, renv, pak, `remotes::install_version()` and Posit Connect must work unchanged: Linux binaries at Posit-style URLs, and a CRAN-style `Archive/`.
- The air-gap bundle import stays.
- **Frozen:** proxy channels (`kind: proxy`). They must keep compiling and passing their tests, but get no new features.
- **Out of scope:** CRAN mirroring features, dated snapshots, Windows/macOS binaries, server-side builds, SSO, HA.

Under the v1.x stability policy ([CLAUDE.md](CLAUDE.md)), breaking changes are fine if the release notes list them. This plan has several: `matrix.yaml` shape, URL layout, `PACKAGES` semantics, anonymous-read config. It adds **no** compatibility shims.

## Phase overview

| Phase | Theme | Estimate | Depends on | Release |
|---|---|---|---|---|
| 0 | Housekeeping | 0.5 d | — | — |
| 1 | R versions and single-distro `matrix.yaml` | 2–3 d | 0 | — |
| 2 | CRAN read surface v2 (`__linux__` routes, latest-only index, `Archive/`) | 4–5 d | 1 | — |
| 3 | `Meta/archive.rds` | 2–3 d | 2 | — |
| 4 | Publish surface for building every R version in CI | 2–3 d | 1 | — |
| 5 | End-to-end verification | 2–3 d | 2, 3, 4 | **v1.3.0** |
| 6 | Deployability for managed infrastructure | 4–5 d | 0 (independent of 1–5) | **v1.4.0** |
| 7 | Migration tooling, docs, release pipeline | 3–4 d | 5 (docs), CI decision (pipeline) | with v1.3 / v1.4 |

**Total:** about 4–5 weeks of focused work.

- Phases 1–5 are the critical path for the first client deployment.
- Phase 6 can run in parallel once Phase 0 is done.
- v1.3.0 is the first version a client can install from. v1.4.0 is the first that deploys cleanly as a managed-infra service.

---

## Phase 0: Housekeeping (0.5 d)

### 0.1 Make `make check` green again

- `make lint` fails on two gosec G124 findings in [internal/ui/ui.go](internal/ui/ui.go) (session cookie, logout cookie). They come from a newer golangci-lint, not from recent code changes.
- Set `HttpOnly: true` and `SameSite: http.SameSiteLaxMode` explicitly.
- `Secure` stays driven by `UISecureCookies`; Phase 6.3 reworks that input.
- Only add a `//nolint:gosec` with a reason if the linter can't see the conditional `Secure`.

### 0.2 Correct the URL-layout docs

- [design.md](design.md) §4 and implementation.md A5 describe a `/<channel>/<R-minor>/…` segment the code never had.
- Mark those sections as superseded by this plan; the full rewrite lands in Phase 7.2.

**Exit:** `make check` passes on `main`.

---

## Phase 1: R versions and single-distro matrix (2–3 d)

### 1.1 R version comparison: new package `internal/rversion`

- `Parse(string) (Version, error)` and `Compare(a, b) int`, following R's `package_version`:
  - split on `.` and `-`;
  - compare components as integers;
  - a longer version is greater when the prefix is equal.
- Verify against R the edge cases listed in the tests below and match them exactly. One of them is whether `1.0` and `1.0.0` compare equal; add the answer as a table row.
- Table tests:
  - `1.10.0 > 1.9.0`
  - `2.1.16 > 2.1.9`
  - `1.0-1 < 1.0.2`
  - `1.0-10 > 1.0-9`
  - invalid inputs
- **Not SQL:** this replaces reliance on `ORDER BY version`, which is lexical, in [internal/api/cran_index.go](internal/api/cran_index.go). Ordering happens in Go after the query.

### 1.2 Single-distro `matrix.yaml`

Rewrite [internal/config/matrix.go](internal/config/matrix.go). New shape:

```yaml
distro: jammy            # PPM codename, used verbatim in URLs: jammy | noble | rhel9 | …
arch: amd64
default_r_minor: "4.4"   # for clients whose User-Agent carries no R version
build_image_hint: "…"    # advisory, for CI only
cells:
  - { name: r-4.2, r_minor: "4.2" }
  - { name: r-4.3, r_minor: "4.3" }
  - { name: r-4.4, r_minor: "4.4" }
  - { name: r-4.5, r_minor: "4.5" }
  - { name: r-4.6, r_minor: "4.6" }
```

- **Validation:**
  - `distro` is required and matches `^[a-z0-9]+$`.
  - `arch` is in the existing enum.
  - Cell `name` values are unique, and `r_minor` values are unique and match `MAJOR.MINOR`.
  - `default_r_minor` must be one of the cells' `r_minor` values.
  - At least one cell.
- **Drop** per-cell `os`, `os_version`, `arch` and the `validOSes` enum (Linux only; the URL says `__linux__`).
- **Callers to update:**
  - `Lookup`
  - publish manifest validation ([internal/api/publish.go](internal/api/publish.go))
  - `GET /api/v1/cells` (new shape: `{distro, arch, default_r_minor, cells:[{name, r_minor}]}`)
  - UI cells page ([internal/ui/](internal/ui/))
  - `admin cells list|show` ([cmd/packyard-server/admin.go](cmd/packyard-server/admin.go))
  - proxy channel `upstream.binary_urls` validation: keys are still cell names, so existing validation keeps working with the new names
- **Default and bootstrap:**
  - Replace the embedded multi-distro default with a single-distro one.
  - `-init` takes `-distro <codename>`, defaulting to `jammy`.
  - The auto-bootstrap on `serve` uses the same default.
- **No DB migration.** Binaries uploaded under old cell names become unreachable once their cell leaves the matrix. The release notes say to re-publish. Adoption is effectively zero, so this is acceptable.

### 1.3 Tests

- Matrix decoding and validation tables in `internal/config/matrix_test.go`.
- Update every fixture that builds a `MatrixConfig`: api, ui and importer tests.

**Exit:** new `matrix.yaml` loads and validates; all existing tests pass against the new shape; `docs/config.md` §matrix updated.

---

## Phase 2: CRAN read surface v2 (4–5 d)

### 2.1 Latest-only `PACKAGES`

- `buildSource` / `buildBinary` in [internal/api/cran_index.go](internal/api/cran_index.go) emit **one row per package**: the highest non-yanked version according to `internal/rversion`.
- Packages whose versions are all yanked disappear from `PACKAGES`; they stay reachable through `Archive/` (2.4).
- Rewrite the file's header comment. It currently documents "all versions in PACKAGES", the lexical-max quirk, and `Yanked: yes` rows.
- **Proxy channels are unaffected:** upstream indexes pass through unchanged.

### 2.2 Posit-style binary routes

In [internal/api/server.go](internal/api/server.go):

```
GET /{channel}/__linux__/{distro}/{snapshot}/src/contrib/PACKAGES
GET /{channel}/__linux__/{distro}/{snapshot}/src/contrib/PACKAGES.gz
GET /{channel}/__linux__/{distro}/{snapshot}/src/contrib/{file}
GET /__linux__/{distro}/{snapshot}/src/contrib/…            # default-channel alias
```

- **`{distro}`:** must equal `matrix.distro`, otherwise 404 with the message `distro "<x>" not served; this repository serves "<distro>"`.
- **`{snapshot}`:** must be `latest`, otherwise 404 with the message `snapshots are not supported; use latest`. Keeping the segment leaves room for dated snapshots later without changing the URL shape.
- **Resolving the build:**
  - New helper `rMinorFromUserAgent(ua string) (string, bool)` parses R's User-Agent, `R (4.4.3 x86_64-pc-linux-gnu x86_64 linux-gnu)` → `4.4`.
  - Also handle the renv, pak and curl-via-R forms; collect real samples during Phase 5.
  - With no R version, use `default_r_minor`. With an R minor that has no cell, there are no binaries and everything resolves to source.
- **Combined index:** for each package's latest version, emit the binary entry if a binary for the resolved cell exists, otherwise the source entry. Add a cache key `linux:<channel>:<cell|source>` and invalidate it with the existing `InvalidateChannel`.
- **Serving `{file}`:** the binary for the resolved cell if present, otherwise the source tarball.
- **Cache headers:** set `Vary: User-Agent` on index and tarball responses, so an intermediate proxy can't serve an R 4.4 binary to R 4.6.
- **Proxy channels under `__linux__`:** a proxy channel resolves its upstream binary URL via the resolved cell name (`binary_urls[cell]`). This keeps the existing behaviour reachable. No new proxy features.

### 2.3 Remove `/bin/linux/{cell}/…`

- Remove the six routes, their handlers in [internal/api/cran_binary.go](internal/api/cran_binary.go), their OpenAPI entries and their tests.
- Keep the serving core (`lookupBinaryBlob`, `serveBlob`) for 2.2.

### 2.4 `Archive/` routes

```
GET /{channel}/src/contrib/Archive/{pkg}/{file}
GET /{channel}/__linux__/{distro}/{snapshot}/src/contrib/Archive/{pkg}/{file}
+ default-channel aliases
```

- **Lenient:** `src/contrib/{file}` and `Archive/{pkg}/{file}` both serve **any** non-deleted version, including yanked ones. Only the indexes are strict.
- `{pkg}` must equal the name parsed from `{file}`, otherwise 404.
- Under `__linux__`, apply the binary-else-source rule from 2.2.

### 2.5 Per-channel anonymous reads

- Add `anonymous_reads: true|false` per channel in `channels.yaml` ([internal/config/channels.go](internal/config/channels.go)), persisted through channel reconcile.
- `requireReadScope` ([internal/api/cran_source.go](internal/api/cran_source.go)) checks the channel's flag instead of `deps.Server.AllowAnonymousReads && isDefaultChannel`.
- Remove `allow_anonymous_reads` from `server.yaml` and the `-allow-anonymous-reads` flag.
- Update `examples/compose/docker-compose.yml` and the quickstart.

### 2.6 Tests

In [internal/api/cran_protocol_test.go](internal/api/cran_protocol_test.go) and alongside:

- Latest-only index, including the newest-yanked and all-yanked cases.
- Version ordering, e.g. `1.10.0` vs `1.9.0` in the same channel.
- `__linux__` resolution:
  - an R User-Agent per cell;
  - no User-Agent → default;
  - an R minor without a cell → source;
  - a mix of packages with and without binaries in one index.
- Wrong distro → 404; non-`latest` snapshot → 404.
- `Archive/` on both path families; `{pkg}`/`{file}` mismatch → 404; deleted → 404.
- Anonymous vs token reads per channel.
- `Vary: User-Agent` present.
- OpenAPI spec updated, and `make openapi-lint` passes.

**Exit:** R can `install.packages()` a binary from `…/{channel}/__linux__/<distro>/latest` in a local manual check; all protocol tests pass.

---

## Phase 3: `Meta/archive.rds` (2–3 d)

### 3.1 New package `internal/rds`: a minimal R serializer (write-only)

- **Output:** gzip-wrapped R serialization, XDR format version 2 (header `X\n`, version 2, writer R version, minimum reader `2.3.0`).
- **Types covered:**
  - `VECSXP`, `STRSXP` (UTF-8 `CHARSXP`), `REALSXP`, `INTSXP`, `LGLSXP`, `NILVALUE_SXP`
  - attribute pairlists: `names`, `class`, `row.names`
- **API:** small constructors (`List`, `Strings`, `Doubles`, `Ints`, `Logicals`, `WithAttr`) plus `Write(w io.Writer, obj) error`. No reader.

### 3.2 Archive listing

- **Structure:** a named list, one element per package that has archived versions, sorted by name (C locale).
- **Each element** is a `data.frame` matching `file.info()`:
  - `row.names`: `"<pkg>/<pkg>_<ver>.tar.gz"`, sorted by `rversion` ascending
  - columns, in order: `size` (double), `isdir` (logical `FALSE`), `mode` (int `0644`, class `octmode`), `mtime` / `ctime` / `atime` (double, class `POSIXct`/`POSIXt`, `published_at` as seconds since the epoch in UTC), `uid` / `gid` (int `0`), `uname` / `grname` (`"packyard"`)
- **Contents:** every non-deleted version that isn't in `PACKAGES`. Yanked versions are included.
- **Routes:** `…/src/contrib/Meta/archive.rds` on the source path, the `__linux__` path and the default aliases. Same content for all.
- **Caching:** cache key `archive:<channel>` in `Index`; invalidated like the other keys; same TTL.

### 3.3 Tests

- **Golden fixture:** `internal/rds/testdata/gen-archive.R` builds a reference `archive.rds` from a synthetic `file.info()`-shaped list. Commit its output. The Go test compares the **decompressed** bytes, because gzip headers can differ.
- **Edge cases:** an empty archive (`list()` with empty names), a package with a single archived version, a large version count.
- **API test:** `archive.rds` rownames and sizes match the DB after publish, yank and delete.

**Exit:** the golden test passes; manually, `readRDS(url(".../Meta/archive.rds"))` in R returns the expected structure, and `remotes::install_version()` installs an archived version.

---

## Phase 4: Publish surface for building every R version in CI (2–3 d)

### 4.1 Attach a binary to an existing version

- **Endpoint:** `POST /api/v1/packages/{channel}/{name}/{version}/binaries/{cell}`
  - Multipart with one part, `binary`.
  - Scope: `publish:<channel>`.
  - Refused on proxy channels (409 `channel_is_proxy`, as publish already does).
- **Implementation:** calls the existing `store.AttachBinary` ([internal/store/store.go](internal/store/store.go)).
- **Responses:**

| Case | Result |
|---|---|
| cell absent | 201 |
| same bytes already present | 200 with `already_existed: true` |
| different bytes on an immutable channel | 409 `version_immutable` |
| mutable channel | 201, or 200 with `overwritten: true` |
| source row missing | 404 |
| cell not in the matrix | 400 |

- **Side effects:** emits `binary_attach` events (reusing the existing event insert with its own type), invalidates the index, increments the `publish_total` metric with a label for the operation.

### 4.2 Report binaries still missing

- **API:** `GET /api/v1/channels/{channel}/missing-binaries[?cell=<cell>]` → `[{name, version, cell}]`, for the **latest non-yanked version** of each package only. Scope: `publish:<channel>` or `admin`. CI uses it to drive backfill jobs.
- **CLI:** `packyard-server admin missing-binaries -channel <ch> [-cell <cell>]` with the same query, table output.

### 4.3 Publish behaviour for partial binaries

- Keep the current semantics: a publish with source plus any subset of cells is valid.
- Add a `missing_cells` field to the publish response, so CI logs show exactly which R versions still need a build or backfill.

### 4.4 Reference CI workflow

- Update [examples/ci/publish.yml](examples/ci/publish.yml) to:
  1. read `GET /api/v1/cells`;
  2. build one binary per `r_minor`;
  3. publish source plus the successful binaries;
  4. print `missing_cells`.
- Add `examples/ci/backfill.sh`, which takes `missing-binaries` and runs build plus attach for each entry.
- Both stay generic: plain shell plus `curl`/`jq`, no CI-vendor specifics beyond the example wrapper.

### 4.5 Optional: publish to several channels at once

- `POST …/packages/{channel}/{name}/{version}?also=<ch>,<ch>`.
- Requires `publish:` on every target and writes all rows in one transaction.
- Only if CI time or bandwidth makes three uploads a problem. Defer by default.

**Exit:** API tests cover every row of the 4.1 table; the `missing-binaries` result is correct after partial publishes and attaches; OpenAPI updated.

---

## Phase 5: End-to-end verification (2–3 d) → release v1.3.0

### 5.1 An end-to-end harness that runs anywhere

- The existing nightly `cran-e2e.yml` is a GitHub Actions workflow. The repo has moved to Gitea and the CI platform is undecided, so the scenarios move into a **`make e2e` target**:
  - Docker only;
  - it starts packyard, publishes fixtures, runs R containers;
  - it is independent of the CI vendor.
- The CI job only calls `make e2e`.

### 5.2 Matrix

| Job | Server `distro` | Client image | R minors exercised |
|---|---|---|---|
| ubuntu | `jammy` | Ubuntu 22.04 with R 4.4 and R 4.5 (e.g. rig-installed) | 4.4 (binary published), 4.5 (source only) |
| rhel | `rhel9` | AlmaLinux 9 with R 4.4 and R 4.5 | same |

### 5.3 Scenarios

The fixture package has three versions published, the newest yanked, and a binary only for R 4.4. The client then:

1. runs `available.packages()` on the `__linux__` URL and sees exactly one row, the highest non-yanked version;
2. runs `install.packages()` on R 4.4 and gets the **binary** (assert via the installed `Built:` field and no compiler invocation);
3. runs `install.packages()` on R 4.5 and gets source, which installs successfully;
4. reads `Meta/archive.rds` with `readRDS` and gets the expected data frame;
5. runs `remotes::install_version()` for an archived version;
6. runs `renv::restore()` with a lockfile pinning an archived version, via both the plain `src/contrib` URL and the `__linux__` URL;
7. runs `pak::pkg_install("pkg@<archived>")`. *Result:* pak resolves `@version` against CRAN's metadata service only and silently installs the current version from any other repository, so the suite checks the current version plus a `url::…/Archive/…` pin instead (documented in tests/e2e/README.md);
8. installs the yanked version by exact pin, which must succeed;
9. requests the wrong distro in the URL and gets a clear failure (R warns that it can't access the index).

**Also collect** the real User-Agent strings from base R, renv, pak and curl as test fixtures for `rMinorFromUserAgent`.

### 5.4 Release v1.3.0

- Release notes list every breaking change:
  - module path (already committed);
  - `matrix.yaml` shape;
  - `/bin/linux/` removal;
  - latest-only `PACKAGES`;
  - `anonymous_reads` moving to `channels.yaml`.
- Until Phase 7.3 lands, cut the release from whichever pipeline is available at the time.

**Exit:** `make e2e` is green for both jobs; v1.3.0 is tagged.

---

## Phase 6: Deployability for managed infrastructure (4–5 d) → release v1.4.0

Independent of Phases 1–5; can run in parallel after Phase 0.

### 6.1 `-healthcheck` flag

- `packyard-server -healthcheck [-url http://127.0.0.1:8080/health]`: GET with a 3 s timeout; exit 0 on 200, otherwise 1.
- Needed because the distroless image has no curl.
- Update `examples/compose/docker-compose.yml`, which currently runs `-version`.

### 6.2 Tokens provisioned from config

Add to `server.yaml`:

```yaml
tokens:
  - label: ci-publish-prod
    scopes: publish:prod,yank:prod
    token_file: /run/secrets/packyard/ci-publish-prod   # plaintext; or:
    # sha256_file: /run/secrets/packyard/ci-publish-prod.sha256
```

- **Migration `003_token_source.sql`:** add `tokens.source TEXT NOT NULL DEFAULT 'api'`, with values `api` or `config`.
- **Sync on startup, in one transaction:**
  - insert missing `config` tokens;
  - update the scopes of existing ones;
  - revoke `config` tokens no longer listed.
  - `api` tokens are never touched.
- **Validation:**
  - exactly one of `token_file` or `sha256_file`;
  - a plaintext token must carry the `pkm_` prefix;
  - the file must be readable at startup;
  - labels must be unique among `config` tokens.
- The token list endpoint and the UI show `source`. Revoking a `config` token through the API is refused with a hint to edit the config.

### 6.3 Running behind a reverse proxy

- **`public_url: https://packages.example.org`**
  - Implies secure cookies. It replaces the current `UISecureCookies: cfg.TLSEnabled()` wiring in [cmd/packyard-server/main.go](cmd/packyard-server/main.go), which is wrong when TLS is terminated upstream.
  - Used for absolute URLs in the UI (e.g. a "configure R" snippet on the channel page).
- **`trusted_proxies: [10.0.0.0/8, …]`:** honour `X-Forwarded-For` / `X-Real-IP` only from those peers, for the request logs and the `actor` context on events.
- **Document:** a dedicated hostname is required. UI templates hard-code `/ui/…`, and subpath deployments aren't supported.

### 6.4 Separate metrics listener

- `metrics_listen: 127.0.0.1:9090` (or a container-network address).
- When set, `/metrics` is served only there and removed from the main mux. When unset, current behaviour.

### 6.5 Backup, restore and verify

- **`admin backup -out <dir>`:**
  1. `VACUUM INTO <dir>/db.sqlite`;
  2. hardlink (or copy across filesystems) every CAS blob referenced by the DB into `<dir>/cas/<aa>/<rest>`, skipping blobs already present. CAS is immutable, so repeated backups into a directory synced with `rsync --link-dest` stay incremental;
  3. copy `server.yaml`, `channels.yaml`, `matrix.yaml`;
  4. write `manifest.json`: time, packyard version, schema version, blob count and total bytes.
- **`admin backup -verify <dir>`:** re-hash every blob, check all DB-referenced blobs are present, run `PRAGMA integrity_check`.
- **`admin restore -from <dir> -data <dir>`:** refuses to overwrite a non-empty data dir unless given `-force`.
- **Tests:** backup → restore → serve round trip in `cmd/packyard-server`.
- Update [docs/backup-restore.md](docs/backup-restore.md) and replace its manual `sqlite3 .backup` + rsync runbook.

### 6.6 Operator docs

In [docs/admin.md](docs/admin.md):

- **Upgrade procedure:** `admin backup`, then pull the new image and restart. Migrations are applied on startup; there's no downgrade.
- **Permissions:** the image runs as uid/gid 65532, so bind-mounted data dirs must be owned accordingly.
- **Restarts:** `channels.yaml` and `matrix.yaml` changes need a restart. Channel reconcile only ever adds. Removing a cell makes its binaries unreachable (candidates for `admin gc`).
- **Deploying as a service:** health check command, metrics listener, token provisioning, backup schedule.

**Exit:** a compose deployment with a read-only config mount, provisioned tokens, the separate metrics port and a scheduled backup works end to end; v1.4.0 is tagged.

---

## Phase 7: Migration tooling, docs, release pipeline (3–4 d)

### 7.1 Importing from an S3-hosted CRAN-like repository

- **`examples/bundler/s3-cranlike-to-bundle.R`:**
  - takes a base URL of a CRAN-like repo (e.g. a public-read S3 bucket laid out as `latest/src/contrib/` + `Archive/`);
  - downloads the current and archived source tarballs;
  - writes a `packyard-bundle/2` **source** bundle with sha256 per file.
  - It doesn't import binaries: they get rebuilt for every configured cell via 4.2 plus 4.4.
- **Bundle import into mutable channels.** The `-channel` help text and the comment on `adminImportBundle` ([cmd/packyard-server/admin.go](cmd/packyard-server/admin.go)) say the target "must exist with immutable policy". The code doesn't enforce that: rows go through the store and follow the channel's own policy.
  - Fix the help text and comment so they say this.
  - Add a test that pins the behaviour for both policies.
  - Migrating into mutable dev/test channels needs no code change.
- **Test:** a bundle round trip from a fixture repo served by `httptest`, for both policies.

### 7.2 Documentation

- [design.md](design.md): rewrite §4 (URL layout) and §8 (matrix), and add a scope note (internal packages only; proxy channels frozen).
- [docs/api.md](docs/api.md): new route table, `archive.rds`, the attach and missing-binaries endpoints, User-Agent resolution.
- [docs/config.md](docs/config.md): new `matrix.yaml`, `anonymous_reads`, `tokens`, `public_url`, `trusted_proxies`, `metrics_listen`.
- [docs/migration.md](docs/migration.md): the S3/CRAN-like → bundle path.
- [README.md](README.md): scope statement, R configuration example using the `__linux__` URL.
- [docs/proxy.md](docs/proxy.md): add a "frozen" note.

### 7.3 Release pipeline on Gitea (needs the CI platform decision)

- Port `ci.yml` (vet, lint, test, openapi-lint), the release job and `make e2e` to the chosen CI: Woodpecker as in managed-infra, or Gitea Actions.
- **GoReleaser:**
  - switch `release.github` to `release.gitea` (`gitea_urls` for `gitea.cynkra.com`);
  - image `gitea.cynkra.com/<owner>/packyard:X.Y.Z` and `:latest`;
  - update the OCI `image.source` label.
- Update the tag-convention docs (README, CLAUDE.md "Release cutting").
- Decide whether GHCR images keep being published. Recommendation: stop. The artifacts live where the source lives.

**Exit:** a tag push on Gitea produces a release and an image in the Gitea registry with no manual steps.

---

## Cross-cutting

### Breaking changes (collected for release notes)

| Change | Phase | Release |
|---|---|---|
| Go module path `gitea.cynkra.com/david.schoch/packyard` | done | v1.3.0 |
| `matrix.yaml`: single `distro`, cells keyed by `r_minor`; per-cell `os`/`os_version`/`arch` removed | 1.2 | v1.3.0 |
| `GET /api/v1/cells` response shape | 1.2 | v1.3.0 |
| `/bin/linux/{cell}/…` removed in favour of `/__linux__/{distro}/latest/src/contrib/…` | 2.2–2.3 | v1.3.0 |
| `PACKAGES` lists only the latest non-yanked version; older versions go under `Archive/` | 2.1 | v1.3.0 |
| `allow_anonymous_reads` / `-allow-anonymous-reads` removed, replaced by per-channel `anonymous_reads` | 2.5 | v1.3.0 |
| Secure cookies driven by `public_url` rather than local TLS | 6.3 | v1.4.0 |
| `/metrics` moves off the main listener when `metrics_listen` is set | 6.4 | v1.4.0 |

### Risks

| Risk | Mitigation |
|---|---|
| `archive.rds` not byte-compatible with what R tools expect | Golden fixture generated by R (3.3); `readRDS` round trip in end-to-end tests (5.3) |
| User-Agent formats differ across base R, renv, pak and Connect | Collect real samples in 5.3; fall back to `default_r_minor`; source always works |
| Intermediate caches serving the wrong R version's binary | `Vary: User-Agent` on every `__linux__` response (2.2) |
| Proxy channels break under the route and matrix changes | Keep their tests green in every phase; binary resolution via `binary_urls[cell]` (2.2) |
| CI platform undecided, blocking releases | `make e2e` is vendor-independent (5.1); v1.3.0 can be cut manually if 7.3 lags |

### Deliberately not in this plan

- Dated snapshots: the URL segment is reserved (2.2).
- LDAP/OIDC for the UI.
- Windows/macOS binaries.
- S3 storage for CAS.
- Server-side builds.
- Retention and scheduled GC.
- New proxy-channel features.
