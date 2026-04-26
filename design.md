# Packyard — Design v0.1 (Architecture Sketch)

**Status:** Skeleton for iteration. Decisions and deferrals are explicit.
**See also:** [research.md](research.md) for prior art.

---

## 0. Positioning vs Posit Package Manager (PPM)

Packyard is not a PPM clone. PPM is a strong product; the honest goal is to fill the slots PPM doesn't serve well, not to out-Posit Posit.

**What PPM does well (packyard does not try to match in v1)**

- Linux binary build farm covering Ubuntu / Debian / RHEL / SUSE × many R minor versions.
- Dated snapshot URLs, operationally polished for 5+ years.
- `SystemRequirements` → `apt` / `dnf` / `zypper` translation.
- Raw mirror throughput at public-mirror scale.
- Web UI with CVE flags, dependency browsing, download stats.
- Integration with Workbench / Connect / RStudio.
- Professional support SLAs and enterprise admin tooling.
- Managed SaaS option.

**v1 in one sentence:** a small open-source server that hosts your organisation's **internal** R packages with a CRAN-protocol-compatible endpoint. Users point their R at both packyard (for internal pkgs) and CRAN / PPM / r-universe (for public pkgs), as two entries in `repos`.

**What packyard v1 ships as concrete differentiators**

| Gap in PPM | Packyard v1's angle |
|---|---|
| Closed-source, commercial licensing | Open source. Orgs that can't or won't buy PPM (defense, academic, consultancies, cost-sensitive ops) have no PPM-class server today. |
| Heavyweight self-host (Postgres + object store + multi-service) | Single Go binary, **SQLite + local filesystem by default**. `scp` and run for a small team. Postgres + object store are opt-in later. |
| Git-source workflow is bolt-on | Gitea / Git host + CI is the primary publishing path by design — CI pushes built tarballs to packyard. |
| Vendor lock-in concerns | Documented, curl-friendly HTTP API (OpenAPI spec shipped); portable CAS layout; exportable data. |

**Committed v1.x roadmap**

| Item | Why v1.x, not v1 |
|---|---|
| **Air-gap deploy + bundled CRAN / Bioconductor mirror** | The defining v1.x feature. Sync-bundle format, snapshot-update cadence, signing, and "updating CRAN on prod" workflow are a whole design project. v1 hosts internal packages only; users point R at upstream CRAN / PPM alongside packyard. See §10. |
| **Server-side binary build farm** | v1 receives pre-built artifacts from CI (§8). Adds a heavy runtime dependency and a lot of orchestration. Cell-identity machinery in v1 is already built to accommodate server-side builds later without a schema change. |
| **Sigstore signing / provenance attestations** | Parked until the basic publish / yank flow is proven. |

**Not planned as first-party — partner instead**

| Item | Packyard's role instead |
|---|---|
| Rust client CLI (`add` / `sync` / `lock` / `tree` / `run`) | **uvr** and **rv** already occupy this space. Packyard ships a well-documented HTTP API + OpenAPI spec (§7) so those tools can support packyard as a publish destination. Building a third R client would be a different project. |
| Content-addressable client cache with hardlinks | Client-side concern; inherited from whichever client the user picks. |
| PubGrub-style resolver errors | Same — client-side. |

**What packyard explicitly does NOT try to match ever**

- Breadth of Linux distro × R-version matrix. v1 targets a narrow default (Ubuntu LTS × recent R minors, amd64); everything else is BYO.
- UI polish and enterprise admin tooling. Functional, not beautiful.
- Public-mirror throughput. A 10k-engineer org probably still buys PPM.
- Managed SaaS. Self-host only.
- Becoming "the uv of R." Packyard is a server; the client story is uvr / rv / existing R tools.

**One-line positioning (v1)**

> An open-source, single-binary R package registry for **internal** R packages — think "private CRAN for your organisation". Users point their R at both packyard (for internal) and upstream CRAN / PPM / r-universe (for public). **Air-gap deploy with a bundled CRAN mirror is the committed v1.x roadmap**.

**Target users**

| User type | Fit | Why |
|---|---|---|
| **R consultancies** (5–30 staff, multi-client) | Primary v1 target | Internal toolkit packages shared across client engagements. Existing options (drat, `install_github` on a private gitea repo) work but lack binaries, auth scoping, and a channel model. |
| **Mid-size R shops** (20–100 devs) | Primary v1 target | Internal packages shared across teams. Not ready to buy PPM; outgrowing ad-hoc solutions. |
| **Regulated orgs** (pharma, finance, defense) | Natural v1.x target, blocked on air-gap roadmap | Need on-prem + no egress. v1 *runs* on a disconnected host but without CRAN mirroring the operational value is limited until v1.x. |
| **University research groups** | Nice-to-have, not core | Shared tooling across labs. Pain is often not acute enough to adopt a new system. |

**Not a target:**

- Enterprises with ≥100 R devs and an existing Posit relationship — PPM is the right answer.
- Individual R users or solo hackers — use [drat](https://github.com/eddelbuettel/drat) or [r-universe](https://r-universe.dev).
- Teams whose only need is "install from a gitea repo" — `install_github` already works.

Packyard is a focused OSS project serving a real but narrow niche. It is not trying to be uv or to replace PPM. It aims to be the obvious choice for the first-tier targets above, and to have a clear on-ramp to the second tier via the v1.x air-gap roadmap.

---

## 1. Scope fixed so far

| Area | Decision |
|---|---|
| Client side | **Out of scope for v1.** Existing R tools (base, renv, pak, uvr, rv) drive packyard via CRAN-protocol compat. A Rust CLI is roadmap, not v1. |
| Server side | Go; on-prem + air-gap friendly; single binary. |
| Storage (default) | **SQLite + local filesystem.** Postgres + object store are opt-in for larger deploys. |
| Ingestion | **CI pushes built artifacts** via `POST /api/v1/publish`. Server does not pull from gitea and does not build. Gitea actions (or any CI) build per cell and upload. |
| Binary builds | **Out of scope for v1.** Server receives pre-built binaries from CI. Matrix YAML defines which cells the server *accepts* uploads for. Server-side build farm is roadmap. |
| CRAN / Bioc handling | **Not hosted by packyard in v1.** Users configure R with packyard + upstream CRAN/PPM/Bioc as separate `repos`. Mirroring + air-gap is on the roadmap. |
| Internal package layout | **Channels**: named repos on the same server (e.g. `dev`, `test`, `prod`, or any names). No first-class staging or promotion workflow — CI decides what goes where. |
| Versioning | Versions are **immutable once published** (Cargo/PyPI model). Iteration during development uses pre-release suffixes (`1.2.3-dev.4`) or a channel with a lenient overwrite policy. |
| Cross-channel deps | Any channel may depend on any channel. If teams want a "prod pkgs may not depend on dev" rule, it's a **CI lint**, not a PM enforcement. |
| Web UI | Server-rendered Go templates + static pages. No SPA, no Node toolchain. |
| Observability | stdout structured logs + `/health` + `/metrics` (Prometheus text format). No log aggregator or metrics store required. |
| Tenancy | Single-tenant v1; design seams for multi-team later. |

### 1.1 v1 launch deliverables

> **Status (April 2026):** all eight deliverables shipped in v1.0.x. See [implementation.md](implementation.md) for the phase-by-phase build record and `§Post-v1 follow-ups (v1.1)` for residual items. The committed v1.x air-gap feature is now spec'd in §10 — bundle format and bundler ship today; the `admin import bundle` command is the next implementation round.

A full v1 release is more than the server binary. First-hour friction kills adoption faster than missing features, so the v1 bundle includes adoption-critical tooling alongside the server:

1. **`packyard-server`** — single Go binary with embedded static assets (HTML templates, CSS, OpenAPI spec). Linux amd64 at launch; arm64 close behind.
2. **OpenAPI 3 spec** served at `/api/v1/openapi.json` and published in the repo. See §7.
3. **Reference CI workflow** at `/examples/ci/publish.yml` — GitHub Actions + Gitea Actions compatible. See §8.4.
4. **Operator dashboard** — server-rendered Go templates under `/ui/`. Pages: channels, recent events, cell coverage, storage usage. No SPA, no JavaScript build step.
5. **Migration tooling** — `packyard admin import --from drat <url>` to ingest an existing drat repo into a channel; `packyard admin import --from git <url> --branch <b>` to ingest a gitea/GitHub R-package source (runs `R CMD build` locally and publishes the source tarball). Adoption is gated on a clean migration from whatever users run today.
6. **5-minute quickstart documentation** — from `docker run packyard-server` to publishing the first package via curl. README + `/docs/quickstart.md`. Explicit success criterion: a new user runs five commands and sees their package in the channel list.
7. **Backup & restore runbook** — documented procedure for snapshotting the data directory (SQLite-aware copy), cadence recommendations, disaster-recovery walkthrough. Single-binary + single-filesystem means single point of failure; the runbook makes this honest and survivable.
8. **Default `channels.yaml` and `matrix.yaml`** — shipped with sensible starting values (dev/test/prod channels; Ubuntu LTS × R 4.3/4.4/4.5 × amd64 cells).

These are not "nice to have" — they are the difference between a project someone tries once and a project someone adopts.

---

## 2. High-level architecture

```
    ┌─────────────────────┐
    │   gitea  (source)   │
    └──────────┬──────────┘
               │ push
               ▼
    ┌─────────────────────┐
    │  CI (gitea actions, │
    │  external builders) │
    │  builds per cell    │
    └──────────┬──────────┘
               │ POST /publish (source + binaries)
               ▼
    ┌───────────────────────────────────────────────┐
    │              packyard-server (Go)               │
    │   single static binary — no runtime deps      │
    │                                               │
    │   ┌─────────────┐  ┌──────────────────────┐   │
    │   │ publish API │  │ registry (CRAN-compat│   │
    │   │ yank API    │  │  + JSON API + UI)    │   │
    │   └─────────────┘  └──────────────────────┘   │
    │           │                   │               │
    │           ▼                   ▼               │
    │   ┌───────────────────────────────────────┐   │
    │   │  CAS blobs on local filesystem        │   │
    │   │  SQLite metadata DB                   │   │
    │   │  (Postgres + object store opt-in)     │   │
    │   └───────────────────────────────────────┘   │
    └───────────────────────┬───────────────────────┘
                            │ HTTPS (CRAN-compat)
                            ▼
          ┌─────────────────────────────────────────┐
          │  existing R tools on user hosts         │
          │  base R · renv · pak · uvr · rv         │
          │  configured with BOTH packyard AND        │
          │  upstream CRAN / PPM / r-universe       │
          └─────────────────────────────────────────┘
```

**Deployment shape (v1)**

- **One process**: the Go server binary.
- **One writable directory**: CAS blobs + SQLite DB. Backup = copy the directory.
- **No required runtime dependencies**: no Docker, no Postgres, no Redis, no object store.
- **Observability**: stdout structured logs + `/health` + `/metrics` (Prometheus text format).
- **UI**: server-rendered Go templates and static files embedded in the binary. No Node toolchain.
- **Upgrade**: replace the binary, restart. Schema migrations run on startup.

The target is a ten-minute `scp`-and-`systemctl-start` deploy on a single VM. Scale-up (Postgres, object storage, remote build workers) is opt-in, not required.

---

## 3. Entities

- **Package** — identified by name.
- **Version** — a version string; unique per (package, channel). Follows R's existing DESCRIPTION grammar with optional pre-release suffix.
- **Build** — a concrete artifact (source tarball + set of platform binaries) keyed by `sha256(source) + build-env fingerprint`. A (pkg, version) normally has exactly one build per channel. Rebuilding bumps the version.
- **Channel** — a named repo on the server (`dev`, `test`, `prod` shipped by default; operators can add more). The unit of publication. Each channel has a small config: name, `overwrite_policy` (either `mutable` or `immutable`), and `default: true` on exactly one channel (shipped on `prod`). Full semantics in §9.
- **Publication** — the act of writing (pkg, version, build) into a channel. Append-only. Replaced by **yank** (marks a version unavailable for new resolves) — never by in-place overwrite on `immutable` channels.
- **Mirror channel** — *(roadmap, not v1)* a channel whose contents come from an external source (CRAN, Bioconductor) via pre-mirrored bundles. Mechanism designed so that when added, it's just another channel with a read-only policy from the publish API's point of view.

**Invariants:**
- Once `(pkg, version)` is published to an `immutable` channel, bytes never change. New bytes require a new version.
- A package name is **not** globally unique across channels — `mypkg 1.2.3` can exist in `dev` and `prod` with different bytes (because they were built from different commits). This is intentional: it matches the "branches in gitea" workflow without forcing version bumps across channels.

---

## 4. URL layout

CRAN-protocol-compatible so any R client (base, renv, pak, uvr, rv) works out of the box:

```
https://packyard.corp/
  <channel>/<R-major.minor>/src/contrib/PACKAGES      # one endpoint per channel
  <channel>/<R-major.minor>/bin/linux/<distro>/...    # platform binaries

  # e.g. (channel names are arbitrary — operator's choice)
  dev/4.4/src/contrib/PACKAGES
  prod/4.4/src/contrib/PACKAGES

  # the default channel (flagged `default: true` in channels.yaml) is also
  # served as a URL alias at the root — so clients can omit the channel
  # for the common case:
  4.4/src/contrib/PACKAGES        # alias → /prod/4.4/... (see §9.3)

  api/v1/channels                                     # JSON API
  api/v1/channels/{name}/packages
  api/v1/publish                                      # POST new build into a channel
  api/v1/yank                                         # mark a version unavailable
  api/v1/events                                       # audit log (publish, yank)
  api/v1/openapi.json                                 # OpenAPI 3 spec
```

**Channels do not overlay each other on the server.** Clients decide composition — they configure an ordered list of channel URLs in `repos =` (packyard alongside upstream CRAN / PPM), exactly like standard R against multiple CRAN mirrors. This keeps the server dumb and moves "which sources this project uses" into the project config, where it belongs.

When the CRAN-mirror feature lands later, it'll be just another channel — typically named `cran` — served from the same URL pattern.

---

## 5. Publishing flow

```
  gitea branch / tag
        │
        │  CI (gitea actions, or anywhere else) decides
        │  target channel based on branch / tag / label
        ▼
  CI builds per cell:
    - R CMD build      → source tarball
    - R CMD INSTALL    → one binary per cell in matrix.yaml
        │
        │  multipart POST /api/v1/publish
        │     { channel, source.tar.gz, binaries: [ {cell, file.tar.gz}, ... ] }
        ▼
  packyard-server
        ├── validates: channel exists, declared cells are in matrix.yaml,
        │             caller has publish:{channel} scope, version not
        │             already present on immutable channel
        ├── stores source + binaries in CAS (keyed by sha256)
        ├── appends Publication event
        └── refreshes channel index (PACKAGES files per R-minor / per-OS)
```

- **Publish is idempotent per (channel, pkg, version)**. Retry-safe for CI.
- A publish may include **source only** (no binaries) — it's valid and clients on other platforms will compile from source on their end, as is CRAN's Linux default today.
- A publish may include **binaries for a subset of cells**. Missing cells are not an error — clients for those cells fall back to source compile.
- **Yank** is the one post-publish mutation: `POST /api/v1/yank {channel, pkg, version}` marks a version as unavailable for new resolves. Existing `PACKAGES` entries stay but carry a `Yanked: true` field so lockfiles referencing it can still fetch.
- No promotion API. "Promoting" a package to another channel = CI publishes it again, to that channel.
- Advisory cross-channel lint is out of the server; it lives in CI tooling or a later client-side command.

**Trust model (v1):** the server trusts CI to have run the build honestly. Auth tokens scope *who can publish where*. Signed attestations (Sigstore) are roadmap. For a single-tenant on-prem deploy where CI is on the same LAN as packyard, this is acceptable. Operators who need stronger guarantees can gate publish behind an approval step in CI — that's a CI concern, not a packyard concern.

---

## 6. Clients (v1)

**Guiding principle: the HTTP API is the publishing UX.** No bespoke packyard CLI, no R-side helper package, no IDE extension in v1. Anyone publishing uses `curl` (or any HTTP client); anyone consuming uses existing R tooling against the CRAN-compat endpoints. Wrappers in R, Rust, VS Code, etc. are things the community or a later version can build on top of a stable, documented API.

This only works if the API is genuinely curl-friendly. v1 commits to:

- Bearer-token auth in an `Authorization:` header. No cookies, no CSRF, no session state.
- Multipart form for publish. Every field plainly nameable on a `curl` command line.
- Predictable status codes; human-readable error JSON (`{error, hint}`).
- Idempotent by `(channel, pkg, version)` on immutable channels.
- An **OpenAPI 3 spec** shipped with the server at `/api/v1/openapi.json` and in the repo, so any wrapper (R package, shell CLI, VS Code extension) can be generated from it later.

### Consumption — existing R tools against the CRAN-compat endpoints

```r
# Typical v1 setup: packyard for internal pkgs, upstream CRAN/PPM for public pkgs
options(repos = c(
  PACKYARD = "https://packyard.corp/prod/",
  CRAN   = "https://packagemanager.posit.co/cran/latest"   # or any CRAN mirror / upstream Bioc
))
install.packages(c("ggplot2", "myInternal"))   # dplyr → CRAN; myInternal → packyard
```

```r
# renv
renv::init(repos = c(
  PACKYARD = "https://packyard.corp/prod/",
  CRAN   = "https://packagemanager.posit.co/cran/latest"
))
```

```r
# pak
pak::repo_add(PACKYARD = "https://packyard.corp/prod/")
pak::pkg_install("myInternal")
```

Auth, when needed, follows the same pattern as PPM — HTTP Basic or bearer tokens via `.Renviron` / `~/.netrc` / environment variables.

**Publishing from CI** is a plain `curl` or language-agnostic HTTP POST — no client needed:

```sh
curl -X POST https://packyard.corp/api/v1/publish \
  -H "Authorization: Bearer $PACKYARD_TOKEN" \
  -F "channel=dev" \
  -F "source=@mypkg_1.2.3.tar.gz" \
  -F "binary=@mypkg_1.2.3_R_x86_64-pc-linux-gnu-ubuntu-22.04.tar.gz;type=application/gzip;filename=cell=ubuntu-22.04-amd64-r4.4"
```

A helper gitea-actions / GitHub-actions workflow shipping with packyard will wrap this. A first-party `packyard` CLI (Rust, uv-shaped) is roadmap once the server stabilises — see §12.

---

## 7. API contract

### 7.1 Conventions

- **Responses are flat arrays of flat objects** by default. `jsonlite::fromJSON()` returns a `data.frame` directly.
- `snake_case` field names.
- Times: ISO 8601 UTC strings with `Z` suffix (e.g. `2026-04-19T10:23:45Z`).
- IDs: int64 monotonic.
- Missing values: explicit `null` (becomes `NA` in R), never omitted keys.
- Pagination: `?limit=N&offset=N` with `X-Total-Count` response header. `/events` also supports `?since_id=N` for polling.
- JSON only for API responses under `/api/v1/*`. `/metrics` is Prometheus text format.
- **One intentional exception to "no nesting":** `GET /packages` rows embed a `binaries` array so "show me a version and all its binaries" is one request. In R, `tidyr::unnest(binaries)` flattens it.

### 7.2 Error shape

All `4xx` / `5xx` responses use the same JSON body:

```json
{
  "error_code": "channel_not_found",
  "message":    "Channel 'foo' does not exist.",
  "hint":       "List channels with GET /api/v1/channels.",
  "request_id": "0199a4b7-e7d1-7f2d-9b3c-1234567890ab"
}
```

`error_code` is a stable snake_case string enumerated in the OpenAPI spec; callers switch on it. `message` is human. `hint` is optional (empty string when not useful). `request_id` appears in server logs for support triage.

### 7.3 Auth

- `Authorization: Bearer <token>`. Tokens are opaque 32-byte base64 strings.
- Scope syntax: `read:<channel>`, `publish:<channel>`, `yank:<channel>`, `admin`. Wildcard `*` in the channel slot (e.g. `publish:*`).
- Token issuance, listing, and revocation via admin-scoped endpoints under `/api/v1/admin/tokens`.
- No sessions, no cookies, no CSRF. State is per-request only.

### 7.4 Endpoints

Full parameter and error tables live in the OpenAPI spec at `/api/v1/openapi.json`. Response shapes below are illustrative.

**`GET /api/v1/channels`** — list all channels.

```json
[
  {"name":"dev",  "overwrite_policy":"mutable",   "default":false, "package_count":23, "created_at":"2026-01-12T09:00:00Z"},
  {"name":"test", "overwrite_policy":"mutable",   "default":false, "package_count":7,  "created_at":"2026-01-12T09:00:00Z"},
  {"name":"prod", "overwrite_policy":"immutable", "default":true,  "package_count":12, "created_at":"2026-01-12T09:00:00Z"}
]
```

**`GET /api/v1/packages?channel=<name>[&package=<name>][&limit=N&offset=N]`** — one row per `(channel, name, version)`, with embedded `binaries` list-column (the one flatness exception).

```json
[
  {
    "channel":"prod", "name":"myInternal", "version":"1.2.3",
    "published_at":"2026-04-19T10:23:45Z", "published_by":"ci-bot",
    "source_sha256":"abc123...", "source_size_bytes":123456,
    "yanked":false, "yank_reason":null,
    "binaries":[
      {"cell":"ubuntu-22.04-amd64-r4.4","sha256":"def456...","size_bytes":1234567,"uploaded_at":"..."},
      {"cell":"ubuntu-24.04-amd64-r4.5","sha256":"789abc...","size_bytes":1234678,"uploaded_at":"..."}
    ]
  }
]
```

**`GET /api/v1/cells`** — the configured build matrix.

```json
[{"name":"ubuntu-22.04-amd64-r4.4","os":"ubuntu","os_version":"22.04","arch":"amd64","r_minor":"4.4","build_image_hint":"ghcr.io/rocker-org/r-ver:4.4"}]
```

**`GET /api/v1/events?[since_id=N][&limit=N][&channel=<name>][&package=<name>]`** — single flat audit feed.

```json
[
  {"id":1, "at":"...", "type":"publish", "actor":"ci-bot@gitea", "channel":"prod", "package":"myInternal", "version":"1.2.3", "note":null},
  {"id":2, "at":"...", "type":"yank",    "actor":"david",        "channel":"prod", "package":"myInternal", "version":"1.1.0", "note":"CVE-2026-1234"}
]
```

`type` is an extensible enum: `publish`, `yank`, `unyank` in v1. Future additions (`channel_created`, `mirror_sync`, `token_issued`) extend without schema change.

**`POST /api/v1/publish`** — multipart with a JSON `manifest` part plus named file parts.

```
Content-Type: multipart/form-data; boundary=BOUND
Authorization: Bearer <token>

--BOUND
Content-Disposition: form-data; name="manifest"
Content-Type: application/json

{"channel":"prod", "package":"myInternal", "version":"1.2.3",
 "binaries":[
   {"cell":"ubuntu-22.04-amd64-r4.4", "part":"bin_jammy"},
   {"cell":"ubuntu-24.04-amd64-r4.5", "part":"bin_noble"}
 ]}

--BOUND
Content-Disposition: form-data; name="source"; filename="myInternal_1.2.3.tar.gz"
Content-Type: application/gzip

<source tarball>

--BOUND
Content-Disposition: form-data; name="bin_jammy"; filename="..."
Content-Type: application/gzip

<binary tarball>

--BOUND
Content-Disposition: form-data; name="bin_noble"; filename="..."
Content-Type: application/gzip

<binary tarball>
--BOUND--
```

Validation:
- Caller must hold `publish:<channel>`.
- Channel must exist.
- Every `manifest.binaries[].cell` must be in `matrix.yaml`, else `400 unknown_cell`.
- On an immutable channel, if `(channel, package, version)` already exists with identical source bytes: `200 OK` with `"already_existed": true` (idempotent retry).
- On an immutable channel, differing bytes for the same triple: `409 version_immutable`.
- Source-only publishes (no `binaries` in manifest, no binary parts) are valid.

Response `201 Created` (or `200 OK` on idempotent retry):

```json
{
  "channel":"prod", "name":"myInternal", "version":"1.2.3",
  "published_at":"2026-04-19T10:23:45Z",
  "source_sha256":"abc123...",
  "binaries_uploaded":2,
  "already_existed":false
}
```

**`POST /api/v1/yank`** — JSON body.

Request:
```json
{"channel":"prod", "name":"myInternal", "version":"1.2.3", "reason":"CVE-2026-1234"}
```

Response `200 OK`:
```json
{"channel":"prod", "name":"myInternal", "version":"1.2.3", "yanked_at":"...", "reason":"CVE-2026-1234"}
```

A yanked version stays in `PACKAGES` (existing lockfiles still resolve) but gains a `Yanked: true` field. An `POST /api/v1/unyank` endpoint (admin-scoped) mirrors this.

**`GET /api/v1/health`** — liveness.

```json
{"status":"ok"}
```

`200` healthy; `503` when a subsystem fails (DB, CAS, filesystem). `503` response lists the degraded subsystems in the same body.

**`GET /api/v1/metrics`** — Prometheus text format, not JSON. Counters for publish / yank rates per channel, CAS bytes stored, request durations, plus standard process metrics.

**`GET /api/v1/openapi.json`** — OpenAPI 3 spec describing every endpoint, parameter, response, and error code above. The source of truth for any wrapper (R package, shell CLI, IDE extension) built later.

---

## 8. Binary matrix & artifact upload

v1 does **not** run builds on the packyard host. CI builds artifacts per cell and uploads them. The matrix YAML defines which cells packyard *accepts* uploads for — the same identity flows into CAS keys, lockfile platform markers, and download URLs whether the artifact was built by server-side Docker (future) or by CI (v1).

### 8.1 Cell

A **cell** is the atomic unit of the matrix. Identity: `(os, os_version, arch, r_minor, builder_image_digest)`. This identity flows into:

- the CAS key for the built binary,
- the `packyard.lock` platform marker,
- the download URL (`/<channel>/<R-minor>/bin/linux/<os>-<os_version>-<arch>/...`).

Making the cell identity part of the lockfile and CAS key **from day 1** is free now; retrofitting later would be a schema migration. Glibc/ABI concerns are collapsed into `(os, os_version)`: two cells with the same `(os, os_version)` are considered binary-compatible.

### 8.2 Matrix config

Static YAML at `/etc/packyard/matrix.yaml` (path configurable). Loaded at startup; changes require a server restart. Source of truth for the cells the server will index and serve.

```yaml
cells:
  # shipped defaults: Ubuntu LTS × recent R minors, amd64 only
  - name: ubuntu-22.04-amd64-r4.4
    os: ubuntu
    os_version: "22.04"
    arch: amd64
    r_minor: "4.4"
    build_image_hint: ghcr.io/rocker-org/r-ver:4.4    # advisory — used by CI, not by the server

  - name: ubuntu-24.04-amd64-r4.5
    os: ubuntu
    os_version: "24.04"
    arch: amd64
    r_minor: "4.5"
    build_image_hint: ghcr.io/rocker-org/r-ver:4.5

  # operator-added
  - name: rhel9-amd64-r4.4
    os: rhel
    os_version: "9"
    arch: amd64
    r_minor: "4.4"
    build_image_hint: internal-registry/r-builder-rhel9:4.4
```

**Default shipped matrix (v1):** Ubuntu 22.04 + 24.04 × R 4.3, 4.4, 4.5 × amd64. Everything else (RHEL / Rocky / Alma / SUSE, arm64, Windows, macOS, older R) is BYO cell.

`build_image_hint` is advisory metadata: it tells CI what image to build in so uploads are binary-compatible with the declared cell. The server does not execute it.

### 8.3 Upload path

On `POST /api/v1/publish` with one or more binaries, the server:

1. Verifies the caller holds `publish:{channel}` scope.
2. For each declared binary, verifies its `cell` parameter matches a cell name in `matrix.yaml`. Unknown cells = 400 error.
3. Writes source tarball + binaries into the CAS, keyed by `sha256`.
4. Appends a Publication event with the full `(channel, pkg, version, source-sha256, [(cell, binary-sha256)])` tuple.
5. Refreshes the `PACKAGES` index for the affected channel / R-minor / OS-arch tuples.

A publish with no matching cell binaries is still valid — it's a source-only publication. Clients on unsupported cells fall back to source compile, which is CRAN's Linux default today anyway.

**What CI does (not packyard):**

1. Build per cell using `build_image_hint` (or equivalent).
2. Produce `source.tar.gz` (from `R CMD build`) and per-cell binaries (from `R CMD INSTALL --build`).
3. POST to `/api/v1/publish` with all artifacts in one multipart request.

A reference workflow showing the full flow ships in `/examples/ci/` in the repo — see §8.4.

### 8.4 Reference CI workflow

v1 ships a reference workflow template (not a maintained action) at `/examples/ci/publish.yml` in the packyard repository. Users copy it into `.github/workflows/` (or `.gitea/workflows/`) and adapt the matrix + channel mapping. The publish step is plain `curl` against `/api/v1/publish` — no maintained packyard action artifact, no additional CLI dependency.

**Assumptions baked in (all simplest-thing choices):**

- One R package per repository; DESCRIPTION at repo root.
- Cell images already contain everything the package needs to build. The workflow does not parse `SystemRequirements` or run `apt-get install`. Heavier packages (GDAL, Stan) are the operator's problem: register a fatter cell with a specialised image in `matrix.yaml`.
- Channel mapping is a hardcoded `case` statement — `main` → `prod`, everything else → `dev`. Users fork and edit the block.
- GitHub Actions syntax; Gitea Actions is syntactically compatible and runs it as-is (runner labels and action mirror paths may need adjustment in locked-down gitea setups).

**Structure — three jobs:**

1. **`build-source`** — runs once in any R container. `R CMD build .` → source tarball artifact. Emits `package` and `version` as job outputs (read from `DESCRIPTION` with `awk`, no R needed downstream).
2. **`build-binary`** — matrix job, one instance per cell, runs in the cell's container. Downloads the source artifact, runs `R CMD INSTALL --build`, uploads the binary. Cells fail independently (`fail-fast: false`).
3. **`publish`** — plain Ubuntu runner (no R), downloads all artifacts, builds the manifest JSON with `jq`, curls multipart to packyard. Uses `--fail-with-body` so a 4xx from the server surfaces the `error_code` / `message` / `hint` from §7.2.

**Reference YAML:**

```yaml
# /examples/ci/publish.yml — reference template for publishing an R package to packyard.
# Works on GitHub Actions and Gitea Actions. Requires:
#   - Repo secret PACKYARD_TOKEN with publish:<channel> scope.
#   - Repo variable PACKYARD_SERVER (e.g. https://packyard.corp).
#   - Cells below must match the server's matrix.yaml (GET /api/v1/cells).

name: Publish to packyard

on:
  push:
    branches: [main, develop]
  workflow_dispatch:

jobs:
  build-source:
    runs-on: ubuntu-latest
    container: { image: ghcr.io/rocker-org/r-ver:4.4 }
    outputs:
      source_file: ${{ steps.build.outputs.source_file }}
      package:     ${{ steps.meta.outputs.package }}
      version:     ${{ steps.meta.outputs.version }}
    steps:
      - uses: actions/checkout@v4
      - id: meta
        run: |
          echo "package=$(awk '/^Package:/ {print $2}' DESCRIPTION)" >> "$GITHUB_OUTPUT"
          echo "version=$(awk '/^Version:/ {print $2}' DESCRIPTION)" >> "$GITHUB_OUTPUT"
      - id: build
        run: |
          R CMD build .
          echo "source_file=$(ls *.tar.gz | head -n1)" >> "$GITHUB_OUTPUT"
      - uses: actions/upload-artifact@v4
        with: { name: source, path: "*.tar.gz" }

  build-binary:
    needs: build-source
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        cell:
          - { name: ubuntu-22.04-amd64-r4.4, image: ghcr.io/rocker-org/r-ver:4.4 }
          - { name: ubuntu-24.04-amd64-r4.5, image: ghcr.io/rocker-org/r-ver:4.5 }
    container: { image: "${{ matrix.cell.image }}" }
    steps:
      - uses: actions/download-artifact@v4
        with: { name: source }
      - run: R CMD INSTALL --build ${{ needs.build-source.outputs.source_file }}
      - uses: actions/upload-artifact@v4
        with:
          name: binary-${{ matrix.cell.name }}
          path: "*_R_*.tar.gz"

  publish:
    needs: [build-source, build-binary]
    runs-on: ubuntu-latest
    steps:
      - id: channel
        run: |
          case "${{ github.ref }}" in
            refs/heads/main) echo "name=prod" >> "$GITHUB_OUTPUT" ;;
            *)               echo "name=dev"  >> "$GITHUB_OUTPUT" ;;
          esac
      - uses: actions/download-artifact@v4
        with: { path: artifacts }
      - env:
          PACKYARD_SERVER: ${{ vars.PACKYARD_SERVER }}
          PACKYARD_TOKEN:  ${{ secrets.PACKYARD_TOKEN }}
          PKG:           ${{ needs.build-source.outputs.package }}
          VER:           ${{ needs.build-source.outputs.version }}
          CHANNEL:       ${{ steps.channel.outputs.name }}
        run: |
          set -euo pipefail
          SOURCE=$(ls artifacts/source/*.tar.gz)
          MANIFEST=$(jq -n --arg c "$CHANNEL" --arg p "$PKG" --arg v "$VER" \
            '{channel:$c, package:$p, version:$v, binaries:[]}')
          CURL_FILES=(-F "source=@$SOURCE;type=application/gzip")
          for dir in artifacts/binary-*; do
            CELL=${dir##*/binary-}
            PART="bin_$(echo "$CELL" | tr -c 'A-Za-z0-9' _)"
            FILE=$(ls "$dir"/*.tar.gz)
            MANIFEST=$(echo "$MANIFEST" | jq \
              --arg cell "$CELL" --arg part "$PART" \
              '.binaries += [{cell:$cell, part:$part}]')
            CURL_FILES+=(-F "$PART=@$FILE;type=application/gzip")
          done
          echo "$MANIFEST" > manifest.json
          curl --fail-with-body -X POST \
            -H "Authorization: Bearer $PACKYARD_TOKEN" \
            -F "manifest=@manifest.json;type=application/json" \
            "${CURL_FILES[@]}" \
            "$PACKYARD_SERVER/api/v1/publish"
```

**What users customise:**

- The `matrix.cell` list — must match packyard's `matrix.yaml`.
- The `case` block in `publish.channel` — pick `prod` / `dev` / other on whatever branch or tag convention the team uses.
- The triggers (`on:` block) — add tag pushes, PR events, schedule, etc.
- Secrets/variables — `PACKYARD_TOKEN` and `PACKYARD_SERVER` live in the repo's CI settings.

**Explicitly out of scope for v1 reference:**

- Automatic `SystemRequirements` → apt translation. Use a fatter cell image if needed.
- Monorepo support (multiple packages per repo). Add a `path:` input in v1.x.
- A maintained `packyard-project/publish@v1` action. Wrapping the curl step into an action is left as a later polish; the template stays curl-based to keep the HTTP API as the canonical publish surface (§6).
- Smart retries beyond what HTTP client behaviour provides — the server-side idempotency guarantee on immutable channels (§7.4 publish) already makes naive retry safe.

### 8.5 Admin commands

- `packyard admin cells list` — configured cells and their stats (count of pkgs with binaries for this cell, coverage %).
- `packyard admin cells show <name>` — details including packages with missing binaries.
- `packyard admin reindex` — rebuild `PACKAGES` indices from CAS (recovery op).

No `rebuild --cell` — there's nothing to rebuild server-side. If a cell is added, CI takes care of building and uploading going forward; backfill is a separate CI job.

### 8.6 Out of scope for v1

- **Server-side builds.** The biggest chunk of §8 as originally designed. Deferred until v1.x once the upload flow is proven. All the cell-identity machinery is built to accommodate server-side builds later without a schema change.
- **Runtime cell management API.** Static file only. API parity comes once the file-based flow is proven.
- **Per-package cell opt-out** (e.g., a `packyard:` block in DESCRIPTION saying "skip `ubuntu-20.04` cells"). Useful, but not before the basic flow is solid.
- **Reproducible-across-rebuilds binary guarantee.** Not enforced — CI builds them.
- **Windows and macOS cells.** Not in the default matrix.

---

## 9. Channels

Channels are the unit of publication and packyard's primary namespace. The `Channel` entity is defined in §3; publish / yank / delete endpoints live in §7. This section covers channel configuration, mutability policy, defaulting, and deletion semantics.

### 9.1 Channel config

Static YAML at `/etc/packyard/channels.yaml` (path configurable). Loaded at startup; changes require restart. Shipped default:

```yaml
channels:
  - name: dev
    overwrite_policy: mutable
  - name: test
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
```

Operators add channels (e.g. `staging`, `qa`, `hotfix`, per-team namespaces) by editing the file. Channel names must be DNS-safe. Exactly one channel carries `default: true` — enforced at startup.

### 9.2 Overwrite policy

Per-channel:

- **`immutable`** — `(name, version)` is write-once. A publish with identical source bytes to an existing entry returns `200` with `"already_existed": true` (idempotent). Differing bytes returns `409 version_immutable`. Recommended for `prod`.
- **`mutable`** — a re-publish to `(name, version)` replaces the prior bytes. Old source and binary blobs in the CAS become orphaned; `packyard admin gc` removes them. Only the latest bytes are retained. The `/events` feed (§7.4) is the audit trail of what was published when; prior bytes are not preserved. Recommended for `dev`, `test`.

There is no per-version policy in v1 (no "pre-releases mutable, stable immutable" special case). The channel is the boundary.

### 9.3 Default channel & implicit-prod URL

The channel flagged `default: true` is served as a URL alias at the root so clients can point at packyard without specifying a channel:

```
https://packyard.corp/4.4/src/contrib/PACKAGES        # aliased → /prod/4.4/...
https://packyard.corp/prod/4.4/src/contrib/PACKAGES   # explicit — identical bytes
```

Non-default channels are addressed at `/<channel>/`. Client config becomes:

```r
# Common case: implicit prod
options(repos = c(
  PACKYARD = "https://packyard.corp/",
  CRAN   = "https://packagemanager.posit.co/cran/latest"
))

# Package authors iterating in dev:
options(repos = c(
  PACKYARD_DEV = "https://packyard.corp/dev/",
  PACKYARD     = "https://packyard.corp/",
  CRAN       = "https://packagemanager.posit.co/cran/latest"
))
```

### 9.4 Delete (mutable channels only)

`DELETE /api/v1/packages/{channel}/{name}/{version}`:

- Allowed only when `overwrite_policy: mutable`. On immutable channels the only mutation is yank.
- Requires `yank:<channel>` scope (the permission sibling of yank).
- Removes the channel index entry entirely. CAS blobs become orphaned; `packyard admin gc` reclaims them.
- Response `200` with `{channel, name, version, deleted_at}`; `409 channel_immutable` on prod; `404 package_not_found` otherwise.

Use case: cleaning up a failed dev experiment you don't want surviving in the `/events` log as an active reference.

### 9.5 Client lockfile implication (informational)

Mutable channels overwrite without changing the version string. A client lockfile pinning `(channel=dev, name=mypkg, version=1.2.3, source_sha256=abc...)` can go stale when `dev` is overwritten. Recommended client behaviour:

- Verify the source hash on resolve.
- On mismatch: fail loudly by default (safe); refetch behind an explicit `--update-mutable` flag (convenient for active development).

This is a client contract, not a server one. Packyard just serves whatever bytes are currently bound to the channel entry.

### 9.6 Typical access pattern (convention, not enforcement)

Suggested token-scope assignment for a three-channel deploy:

- `publish:dev` — broadly available to most devs and any CI job.
- `publish:test` — CI on merge to the integration branch.
- `publish:prod` — CI only, on merge to `main` or release-tag push. Humans do not hold this scope.

Expressed via §11 tokens; the server does not enforce the pattern.

### 9.7 Admin commands

- `packyard admin channels list` — configured channels with their policies and stats.
- `packyard admin gc` — garbage-collect orphaned CAS blobs. On-demand, not scheduled.
- Channel creation/removal: edit `channels.yaml` and restart the server.

### 9.8 Out of scope for v1

- **Per-channel retention policies** (`retention_days`, `keep_n_latest`). Only-latest is simple and enough for v1.
- **Runtime channel management API.** File-edit + restart only.
- **Public-read / anonymous channels.** All channels require `read:<channel>` scope.
- **Per-channel quotas or size limits.**
- **Scheduled GC.** Operator runs `packyard admin gc` when disk pressure matters.

---

## 10. CRAN mirror + air-gap deploy (roadmap, not v1)

Not in v1. Users configure R to hit packyard for internal packages and upstream CRAN / PPM / r-universe for public ones — see §6. v1.x adds the air-gap path: pre-built CRAN bundles that operators carry across an air-gap and import into packyard. Operator playbook lives in [docs/airgap.md](docs/airgap.md); the bundler is in [examples/bundler/](examples/bundler/).

### 10.1 Design inputs from target users

- Organisations typically pin **one fixed CRAN snapshot per supported R version** and never mutate it. Reproducibility of past analyses depends on this.
- "Latest R" is the exception: its snapshot is a moving target pointing at the most recent upstream state. Moving targets don't work in air-gap.
- Updating a snapshot is an operator-initiated, infrequent event (weeks or months, not hours). A regular cadence (e.g. quarterly) is plausible.
- **Most regulated orgs need 50–200 packages**, not all of CRAN. A "subset of CRAN" workflow is a first-class case, not a footnote.

### 10.2 The bundle format is the API

The on-disk bundle layout matches CRAN's URL structure exactly so that bundles are useful even outside packyard. Operators can audit them with standard tools, sign them with whatever their security policy mandates, and — if they don't want to run packyard — drop a bundle behind any static web server and `install.packages()` works. That's the whole "tarball CRAN" story: the bundle *is* a CRAN mirror, packyard just adds the audit trail and channel scoping on top.

```
bundle/
  src/contrib/
    PACKAGES
    PACKAGES.gz
    PACKAGES.rds
    <pkg>_<ver>.tar.gz
    ...
  manifest.json
```

`manifest.json` is the only non-stock-CRAN file. Two schemas are accepted: `packyard-bundle/2` is the current shape; `packyard-bundle/1` is the legacy source-only shape, retained on the read side so older archives continue importing without rebuilds.

```json
{
  "schema": "packyard-bundle/2",
  "snapshot_id": "cran-r4.4-2026q1",
  "r_version": "4.4",
  "source_url": "https://cloud.r-project.org",
  "mode": "subset",
  "kind": "source",
  "created_at": "2026-04-25T07:44:08Z",
  "tool": "examples/bundler/build-bundle.R (miniCRAN 0.3.2, R 4.5.2)",
  "input_packages": ["ggplot2", "dplyr", ...],
  "packages": [
    { "name": "ggplot2", "version": "3.5.1",
      "source": { "path": "src/contrib/ggplot2_3.5.1.tar.gz",
                  "sha256": "abc123...", "size": 4123456 } },
    ...
  ]
}
```

`kind` discriminates between source bundles (carry `src/contrib/` tarballs) and binary bundles (carry pre-built `bin/linux/<cell>/` tarballs from a binary mirror like Posit Public Package Manager). A binary bundle has `kind: "binary"`, a top-level `cell`, and per-package `binaries: [{ cell, path, sha256, size }]` instead of `source`. One bundle = one kind = (for binary bundles) one cell — operators who need binaries for multiple cells run the bundler once per cell.

Bundles compose. To populate a channel with both source and binaries: import the source bundle first to create the `packages` rows, then import each binary bundle. The binary import attaches into existing `binaries` rows without touching the source — and refuses (per package) if the source row is absent, so an out-of-order import surfaces clearly in `failed=` rather than silently no-op.

Decoupling the format from the producer means operators who already have rsync-of-CRAN, PPM CLI exports, or a hand-rolled pipeline can keep using them — they just need to write a `manifest.json` alongside whatever directory they produce.

### 10.3 Bundle producer = R script, not a packyard binary

The bundler is an R script at [`examples/bundler/build-bundle.R`](examples/bundler/build-bundle.R) that wraps `miniCRAN::makeRepo()`. Two reasons:

- The R-side dependency resolver already understands `Imports` / `Depends` / `LinkingTo`, version constraints in DESCRIPTION, and `Priority: base/recommended` exclusions. Reimplementing in Go is a year of edge cases that miniCRAN's maintainers track for free.
- The connected build host needs network and R; for the regulated-org segment, that's a CI runner or a dev laptop — already available.

Three modes, same script:

| Mode | Input | Produces |
|---|---|---|
| Subset (`--packages packages.txt`) | one package name per line | the dependency closure of those packages, sources only |
| Full (`--full`) | _(none)_ | every CRAN package available for the requested R minor, sources only |
| Binary (`--binary-cell` + `--binary-repo`) | a packages list + a P3M-style URL | the dependency closure as precompiled tarballs for one cell, laid out under `bin/linux/<cell>/` |

Binary mode runs from any host (including macOS or Windows): Posit Public Package Manager's `__linux__/<distro>/<snapshot>` URLs serve precompiled tarballs based on the URL path, not on the requesting client's OS or User-Agent.

Operators who already produce CRAN-shaped output by other means write their own `manifest.json` and feed it to `import bundle` directly.

### 10.4 Mirror channels are just channels

No new channel type. A "mirror channel" is a regular [§3](#3-entities) channel with `overwrite_policy: immutable` and a name that starts `cran-` by convention. Per-(R-minor, snapshot) naming is recommended:

```
cran-r4.4-2026q1
cran-r4.5-2026q2
cran-internal-baseline
```

Multiple mirror channels per server is expected. R configurations enumerate the relevant snapshot in `repos =`. Old channels stay forever — pharma audit trails depend on "version X of analysis was reproducible against snapshot Y." A "moving" mirror channel (`overwrite_policy: mutable`, re-imported regularly) is *possible* but not blessed; the operator owns the policy.

The data model and CAS already handle this — an imported CRAN tarball goes through the same publish path as a CI-pushed package, just batched and from a different source.

### 10.5 Importing — `admin import bundle`

```sh
packyard-server admin import bundle ./cran-r4.4-2026q1.tar.gz \
  --channel cran-r4.4-2026q1
```

Behaviour:

1. Create the channel with `overwrite_policy: immutable` if it doesn't exist.
2. Verify the bundle's `manifest.json` against schema `packyard-bundle/{1,2}`.
3. For each entry in `manifest.packages`, sha256-verify the referenced blob(s) against the manifest, then write to CAS. Blobs already in CAS (from a previous snapshot) are deduplicated automatically.
4. For source bundles: insert per-package DB rows referencing the CAS blobs. For binary bundles: look up the existing package row by `(channel, name, version)` and attach a `binaries` row for the bundle's cell. Binary imports fail per-package with `source row not found` if the matching source bundle hasn't been imported first.
5. Append an audit event per package imported (`publish` for source, `import_binary` for binary).

Mismatched sha256 aborts the whole import — partial imports are not allowed. Per-package failures during the import phase (e.g. missing source row for a binary entry) surface in `failed=` and don't block the rest of the bundle.

### 10.6 Update model

Bundles are full snapshots. There are no diff bundles in v1.x: CAS dedup makes overlap-heavy re-imports cheap (95%-overlapping bundle adds only the new packages to disk), and the operational simplicity of "every bundle is a complete snapshot" outweighs the disk-saving optimization.

Quarterly cadence is the typical shape: build a fresh full bundle on the connected side, transport, `import bundle --channel cran-r4.4-<NEW>`. The old channel stays. R configs that pinned to it still work forever.

Diff bundles, dated channel snapshots backed by retention policies, and rotating-channel automation are v1.y once we see real operational telemetry.

### 10.7 Signing — required hashes, optional crypto

Per-package sha256 in `manifest.json` is mandatory. Bundle-level cryptographic signing (cosign / minisign / gpg over `manifest.json`) is opt-in and documented but not enforced by `import bundle`. Key management — who owns the key, how it crosses the air-gap, rotation policy — varies per org and isn't packyard's call to make.

In v1.y, native signature verification (sigstore-friendly bundle-level attestations) might land if there's demand. The current shape is forward-compatible: the signature lives next to `manifest.json` and `import bundle` could opt-in-verify it without a format change.

### 10.8 What this preserves from v1

The channel model, URL layout, CAS, and publish path are unchanged. Adding CRAN mirror channels later doesn't change any existing contract — the import path is a new admin command that uses existing primitives (channel reconcile, CAS write, event append). Operators who don't need air-gap aren't affected at all.

---

## 11. Auth (v1, minimal)

- Long-lived tokens with scopes: `read:{channel}`, `publish:{channel}`, `yank:{channel}`, `admin`.
- Tokens bound to service accounts (users or CI).
- **Recommended operational pattern: one token per CI pipeline, not one shared token**, scoped narrowly (e.g. `publish:dev` only). Combined with per-token listing + revocation under `/api/v1/admin/tokens`, a compromised or rotated CI runner can be revoked without rotating everything.
- OIDC/SAML **deferred** but the token model is built to sit behind an SSO layer later.
- Per-package ACLs **deferred** — single-tenant assumption.

---

## 12. What's explicitly deferred

**Committed v1.x roadmap:**

- **Air-gap deploy + bundled CRAN / Bioconductor mirror** (see §10). The defining v1.x feature; without it, the defense / pharma / regulated-org segment is blocked.
- **Server-side binary build farm** (in-process Docker, then remote workers). v1 receives pre-built artifacts from CI. All cell-identity + CAS machinery is built to accommodate server-side builds later without schema change.
- **Sigstore / PGP signing and provenance attestations.** Parked until the basic publish/yank flow is proven.
- **Dated channel snapshots** (à la PPM). Likely needed for air-gap prod pins but parked until channel model settles. See §10 for design inputs.

**Deferred (not yet scheduled):**

- **Runtime cell management API.** Static `matrix.yaml` only in v1.
- **Runtime channel management API.** `channels.yaml` + restart only in v1 (§9.8).
- **Per-channel retention policies.** Only-latest in v1 (§9.8).
- **Multi-tenant isolation** (per-team namespaces, per-package ACLs). Single-tenant v1.
- **OIDC / SAML SSO.** Token model designed to sit behind it later.
- **Vulnerability / audit database integration.**
- **Postgres + object-store backend.** SQLite + local FS only in v1; backend is abstracted for later.

**Decided out — not planned (partner or out of scope):**

- **First-party Rust client CLI** (`add` / `sync` / `lock` / `tree` / `run`). [uvr](https://github.com/nbafrank/uvr) and [rv](https://github.com/A2-ai/rv) already occupy this space. Packyard's contribution is to be a first-class, well-documented publish destination via the HTTP API + OpenAPI spec. Building a third R client would be a different project.
- **Content-addressable client-side store with hardlinks.** Client-side concern; whichever client the user picks inherits it (pnpm / uv-style is the reference). Not packyard's responsibility.
- **PubGrub / pak-style resolver.** Same — client-side.
- **R task runner** (`packyard run`-style). Out of scope; users have `targets`, `make`, or the R client of their choice.
- Any promotion workflow / approval gates / stage-aware resolver — CI owns this, not packyard.
- Windows / macOS cells in the default matrix — BYO only, ever.
- SPA-based web UI — server-rendered templates, ever.
- Prometheus / log-aggregator / external metrics store as required runtime deps — stdout + `/health` + `/metrics`, ever.
- Managed SaaS — self-host only, ever.
- Trying to be "the uv of R." Packyard is a server; the client story lives with uvr / rv / existing R tools.

---

## 13. Biggest open questions to tackle next

Resolved:

- ~~Build farm platform matrix~~ — §8: static YAML, narrow rocker-based default, operator BYO; server-side builds deferred entirely; v1 is upload-only.
- ~~Ingestion path~~ — CI pushes via `POST /api/v1/publish`. No gitea-pull in v1.
- ~~Client scope~~ — no bespoke client in v1; existing R tools drive via CRAN-protocol compat; HTTP API is the publishing UX.
- ~~Air-gap / CRAN-mirror scope~~ — deferred to roadmap; §10 captures design inputs.
- ~~API contract~~ — §7: flat JSON, rich error shape, single events feed, multipart publish with JSON manifest, `/packages` with `binaries` list-column.
- ~~Reference CI~~ — §8.4: reference workflow template at `/examples/ci/publish.yml`, curl-based, no maintained action.
- ~~Channel semantics~~ — §9: configurable via `channels.yaml` (dev/test/prod shipped); `mutable` vs `immutable` overwrite policy; `default: true` channel aliased at root; hard-delete allowed on mutable channels via `DELETE`; GC on-demand.

Still active:

_None — v1 scope is locked as far as the current design doc goes. Next iterations will be implementation planning (data model, build, release pipeline) rather than further design sections._

Deferred to later iterations:

- Air-gap sync bundle format (§10 stub).
- Resolver (client-side; defers with the client).

Each active item expands into its own section as we iterate.

---

## 14. Non-goals for v1

- Replacing Gitea or any source-control system.
- Hosting a public package registry.
- **Mirroring CRAN / Bioconductor** (roadmap, §10).
- **Air-gap-first deploy** as a v1 feature (roadmap, §10). v1 can still run on a disconnected host, but without upstream pre-mirroring the operational value is limited.
- Building R packages server-side (CI does this).
- Shipping a bespoke client CLI or R-side publish helper (HTTP API is the publishing UX).
- Cross-repo R-package refactoring tools.
- Being a general-purpose artifact repository (only R packages).
- Requiring Docker, Postgres, Redis, object storage, or a metrics stack on the host.
- Managed SaaS.
