# Packyard v1 — Implementation Plan

## Context

The design phase is complete (see [design.md](design.md)). v1 scope is a single-binary Go server that hosts internal R packages, serves a CRAN-protocol-compatible read surface, accepts CI-pushed artifacts via HTTP multipart publish, supports dev/test/prod channels with per-channel overwrite policy, and runs from SQLite + local filesystem by default. Air-gap + CRAN mirroring is the committed v1.x roadmap (out of v1 scope).

Target shape: **8–12 weeks of focused solo work**, split across three phases — A (core), B (operability + adoption), C (launch). Phases gate on demo-ability, adoptability, and release-readiness respectively.

Critical design references:
- [design.md](design.md) §1 scope, §1.1 launch deliverables
- [design.md](design.md) §3 entities, §4 URL layout
- [design.md](design.md) §7 API contract (OpenAPI-normative)
- [design.md](design.md) §8 binary matrix + §8.4 reference CI workflow
- [design.md](design.md) §9 channels + `channels.yaml`
- [design.md](design.md) §11 auth

## Technology choices (pinned)

Keep dependencies minimal to match the lightweight ethos:

- **Go 1.22+** — for `slog`, improved `net/http` routing, `embed`.
- **HTTP server / router**: stdlib `net/http` + the 1.22 routing patterns. No chi, no gin. If routing gets painful, revisit.
- **SQLite driver**: `modernc.org/sqlite` (pure Go). Enables static cross-compiled binaries without cgo. Trade-off: slightly slower than `mattn/go-sqlite3`; fine at v1 scale.
- **SQL**: `database/sql` with a thin query package. No ORM. `sqlc` optional later if query count grows.
- **Migrations**: self-rolled — numeric `migrations/NNN_name.sql` files, applied at startup in a transaction.
- **Auth**: opaque 32-byte random tokens, stored in DB as `sha256(token)`. No password hashing library needed.
- **YAML**: `gopkg.in/yaml.v3` for `channels.yaml` + `matrix.yaml`.
- **Multipart**: stdlib `mime/multipart`.
- **Logging**: stdlib `slog`, JSON handler on stdout.
- **Metrics**: `github.com/prometheus/client_golang/prometheus`.
- **Templates**: stdlib `html/template`.
- **Embedding assets**: stdlib `embed`.
- **Testing**: stdlib `testing` + `testify/require` (optional, for readability).
- **Release**: GoReleaser for cross-compile + Docker image to ghcr.io.
- **Container base**: `gcr.io/distroless/static-debian12` for final image (no shell needed — pure Go binary).

## Project layout

```
packyard/
  cmd/
    packyard-server/
      main.go                 # entry point, flag parsing, server start
  internal/
    api/                      # HTTP handlers, middleware, routing
      publish.go
      yank.go
      channels.go
      packages.go
      binaries.go
      cells.go
      events.go
      admin.go
      health.go
      errors.go               # error envelope, request_id middleware
      middleware.go
    auth/                     # token + scope checks
      token.go
      scope.go
    cas/                      # content-addressable blob store
      store.go
      gc.go
    config/                   # channels.yaml + matrix.yaml loaders
      channels.go
      matrix.go
      server.go               # top-level server config
    db/                       # sqlite schema + queries
      db.go
      migrate.go
      queries.go              # or per-entity files
    events/                   # event log append
      events.go
    importers/                # migration tools (drat, git)
      drat.go
      git.go
    ui/                       # operator dashboard
      handlers.go
      templates/*.html
      static/*                # minimal CSS, favicon
    version/
      version.go              # version string set at build time
  migrations/
    001_init.sql
    ...
  examples/
    ci/
      publish.yml
      publish-gitea.yml       # only if divergent from GH
  docs/
    quickstart.md
    api.md
    config.md
    admin.md
    backup-restore.md
  openapi/
    openapi.yaml              # source of truth; served at /api/v1/openapi.json
  default-config/
    channels.yaml
    matrix.yaml
  go.mod
  go.sum
  Dockerfile
  .goreleaser.yaml
  Makefile                    # common dev tasks
  README.md
  LICENSE                     # MIT or Apache-2.0
  .github/workflows/ci.yml    # or .gitea equivalent; dogfood whichever
  implementation.md           # this file
  design.md
  research.md
```

## Test strategy

- **Unit tests** per package (`_test.go` alongside code). Target: handlers, config loaders, CAS, scope parsing, multipart parsing, DB queries.
- **Integration tests** in `internal/api/integration_test.go` using `httptest.NewServer` + a temp SQLite DB. Covers happy paths for each endpoint + the full publish → list → yank → delete cycle.
- **CRAN-protocol smoke tests**: a test that spins up the server, publishes a minimal R package, and shells out to `Rscript -e 'install.packages(...)'` in a containerised R to verify end-to-end works. Lives in `test/cran_protocol/`.
- **CI** runs all three tiers on every push. Unit + integration in seconds; CRAN-protocol smoke in ~30s.
- **Coverage target**: 75%+ for `internal/*`. Not a gate, but a signal.

Explicitly *not* targeting:
- 100% coverage (diminishing returns for glue code).
- Fuzz tests in v1 (multipart parsing is the most hostile-input surface; add fuzzing in v1.1).
- Load tests (premature; v1 scale is 100s of users, not 100000s).

## Phase A — Core server (MVP)

**Goal:** a demo-able server. Publish + yank + install-from-R work end-to-end. Auth is enforced. SQLite schema stable.

**Estimated effort:** ~4–6 weeks solo.

---

### A1. Bootstrap — ~3 days

**Deliverables:** Buildable Go module with CI running tests, a stub `packyard-server` binary that prints version and exits, and a container image.

**Tasks:**

1. `go mod init github.com/<owner>/packyard`; choose owner.
2. Create `cmd/packyard-server/main.go` with `-version` and `-config` flags.
3. Set up `Makefile` with `test`, `build`, `fmt`, `vet`, `lint`.
4. Add `golangci-lint` config (`.golangci.yaml`).
5. `.github/workflows/ci.yml` (or `.gitea/workflows/ci.yml`): run `go test ./...`, `golangci-lint run`, build binary.
6. `Dockerfile`: multi-stage, distroless final image.
7. `.goreleaser.yaml` with amd64 + arm64 for Linux; archive + Docker.
8. `LICENSE` (MIT or Apache-2.0 — pick one).
9. Initial `README.md` stub with positioning paragraph from [design.md](design.md) §0.

**Acceptance:** `make build` produces a binary. `make test` passes. CI is green. `docker build .` succeeds. `packyard-server -version` prints a version string.

---

### A2. Storage — ~5 days

**Deliverables:** SQLite schema in place with migration runner; CAS helpers implemented; tests for both.

**Tasks:**

1. Decide data directory layout: `${PACKYARD_DATA}/db.sqlite`, `${PACKYARD_DATA}/cas/<first-2-chars>/<remaining>`.
2. Write `internal/db/db.go` — open SQLite with WAL mode, foreign keys on, reasonable timeouts.
3. Write `internal/db/migrate.go` — apply `migrations/*.sql` in order inside a tx; record applied version in `schema_migrations` table.
4. Draft `migrations/001_init.sql` with tables:
   - `channels` (name PK, overwrite_policy, is_default, created_at)
   - `packages` (id PK, channel, name, version, source_sha256, source_size, published_at, published_by, yanked, yank_reason, UNIQUE(channel, name, version))
   - `binaries` (id PK, package_id FK, cell, binary_sha256, size, uploaded_at, UNIQUE(package_id, cell))
   - `events` (id PK monotonic, at, type, actor, channel, package, version, note)
   - `tokens` (id PK, token_sha256, scopes_csv, created_at, last_used_at, revoked_at, label)
5. Write `internal/cas/store.go` — `Write(r io.Reader) (sha256 string, size int64, err error)`, `Read(sha256 string) (io.ReadCloser, error)`, `Has(sha256 string) bool`, content-address path derivation, atomic write-to-temp-then-rename.
6. Write `internal/cas/gc.go` — given a set of "live" sha256s, remove all CAS entries not in the set. Atomic per-file removal.
7. Unit tests: migration idempotency, concurrent CAS writes of same content, GC doesn't remove live blobs.

**Acceptance:** `TestMigrate` passes; `TestCASRoundTrip` passes; `TestGC` passes. Running the binary with a fresh data dir creates schema and an empty `cas/` dir.

---

### A3. Config — ~3 days

**Deliverables:** `channels.yaml`, `matrix.yaml`, and top-level server config loaded and validated at startup.

**Tasks:**

1. `internal/config/server.go` — top-level config struct (listen addr, data dir, paths to channels.yaml and matrix.yaml, TLS paths, token issuance mode). Loaded from `-config` flag or env vars.
2. `internal/config/channels.go` — parse `channels.yaml` into `[]ChannelConfig`. Validate: names are DNS-safe, no duplicates, exactly one `default: true`, overwrite_policy ∈ {mutable, immutable}.
3. `internal/config/matrix.go` — parse `matrix.yaml` into `[]CellConfig`. Validate: name uniqueness, DNS-safe, (os, os_version, arch, r_minor) combos sane.
4. On startup, load all three; fail fast with a legible error pointing to the line if any check fails.
5. On startup, sync channel rows in DB with `channels.yaml`: insert new ones, warn (don't remove) on channels present in DB but not in file (operator removed a channel that still has packages — manual cleanup needed).
6. Ship `default-config/channels.yaml` and `default-config/matrix.yaml` baked into the binary via `embed`. First-run bootstraps these into the data dir if missing.
7. Unit tests for validation errors (duplicate names, no default channel, invalid overwrite policy, etc.).

**Acceptance:** `packyard-server` with a fresh data dir writes default config files and starts. With a malformed `channels.yaml`, it exits 1 with a clear error message.

---

### A4. Publish + yank + delete — ~7 days

**Deliverables:** The three mutation endpoints working with full validation and event emission.

**Tasks:**

1. `internal/api/middleware.go` — request ID (UUIDv7), panic recovery, access logging.
2. `internal/api/errors.go` — error envelope `{error_code, message, hint, request_id}`, helper `writeError(w, status, code, msg, hint)`.
3. Auth stub: middleware that reads `Authorization: Bearer <token>`, looks up `token_sha256`, attaches scopes to request context. Full auth in A6; this lets us wire middleware now.
4. `internal/api/publish.go` — multipart parser, manifest JSON parsing, validation:
   - caller holds `publish:<channel>`
   - channel exists in DB
   - every `manifest.binaries[].cell` exists in matrix.yaml
   - `part` names resolve to actual multipart parts
   - DESCRIPTION/Version from manifest
5. Publish logic: compute sha256 of source, check immutable-channel idempotency (identical bytes → 200 with `already_existed: true`; different bytes → 409 `version_immutable`), write to CAS, insert `packages` + `binaries` rows in a tx, append event. For mutable channel overwrite: update row, leave old CAS blob for GC.
6. `internal/api/yank.go` — JSON body, scope check (`yank:<channel>`), set `yanked=true`, append event.
7. `internal/api/delete.go` — `DELETE /api/v1/packages/{channel}/{name}/{version}`, scope check, reject on immutable channels with 409 `channel_immutable`, delete rows in tx, append event.
8. Integration tests: publish happy path, publish idempotency, publish with unknown cell (400), publish without scope (401/403), yank, delete on mutable, delete on immutable (409).

**Acceptance:** curl-driven publish of a real `jsonlite_x.y.z.tar.gz` succeeds. Replaying the same curl returns 200 with `already_existed: true`. A tampered re-publish returns 409. Yank marks the version. Delete on dev works; delete on prod is refused.

---

### A5. CRAN-protocol read surface — ~5 days

**Deliverables:** R clients (base, renv, pak) can install published packages from packyard.

**Tasks:**

1. `internal/api/cran.go` — route `/{channel}/{r-minor}/src/contrib/PACKAGES` and `/{r-minor}/src/contrib/PACKAGES` (default-channel alias).
2. `PACKAGES` generation from DB: one stanza per `(channel, name, version)` row that matches `r_minor`. Include `Yanked: true` for yanked entries. Cache the generated file in memory with a TTL + invalidate on publish/yank.
3. Source tarball download: `/{channel}/{r-minor}/src/contrib/<name>_<version>.tar.gz` → CAS read by sha256 looked up from DB.
4. Binary tarball download: `/{channel}/{r-minor}/bin/linux/<os>-<os_version>-<arch>/<name>_<version>_R_*.tar.gz` (name pattern matches R's conventions).
5. `/{channel}/{r-minor}/bin/linux/<os>-<os_version>-<arch>/PACKAGES` with binary-side fields.
6. CRAN-protocol smoke test in `test/cran_protocol/` — spin up server, publish test package, `Rscript -e 'install.packages("pkg", repos="...")'` in a rocker container, assert package is installed.
7. `read:<channel>` scope check on reads. Default channel: accepts both tokens and anonymous reads only if a server flag allows it (default: auth required).

**Acceptance:** The CRAN-protocol smoke test passes in CI. `install.packages("testpkg", repos="http://localhost:8080/")` from a real R session works.

---

### A6. Auth — ~4 days

**Deliverables:** Token issuance, scope enforcement, admin endpoints for managing tokens.

**Tasks:**

1. `internal/auth/token.go` — generate opaque 32-byte random token, store `sha256(token)` + label + scopes.
2. `internal/auth/scope.go` — scope parser: `publish:prod`, `publish:*`, `read:dev`, `yank:test`, `admin`. Match function `Has(scopes, required)`.
3. Middleware reads bearer token, looks up by sha256, populates request context with scopes. Rejected tokens return 401.
4. Endpoint handlers call `auth.Require(ctx, "publish:"+channel)` etc.; return 403 with `error_code=insufficient_scope`.
5. `/api/v1/admin/tokens` endpoints (admin scope required):
   - `POST /api/v1/admin/tokens` — create; returns the token ONCE (never again); body has label + scopes.
   - `GET /api/v1/admin/tokens` — list; response omits the token itself, just `{id, label, scopes, created_at, last_used_at, revoked_at}`.
   - `DELETE /api/v1/admin/tokens/{id}` — revoke.
6. Bootstrap: on empty DB, `packyard-server admin create-admin-token` CLI subcommand writes an admin token to stdout. Operator rotates via API afterwards.
7. Integration tests: unauth → 401, wrong scope → 403, admin-only endpoints require admin scope, revoked token fails.

**Acceptance:** `packyard-server admin create-admin-token` issues a token. Publishing without a token fails with 401. Publishing with a `publish:dev` token succeeds on dev, fails on prod. Revoked tokens are rejected on next request.

---

**Phase A Definition of Done:**

- Server binary builds, starts with a fresh data dir, loads default config.
- Publish via curl works (source + binaries, manifest-driven).
- Yank + hard-delete (mutable only) work.
- `install.packages()` from a real R session works against the server.
- Auth is enforced end-to-end.
- All unit + integration + CRAN-protocol smoke tests pass in CI.

---

## Phase B — Operability + adoption

**Goal:** v1 is something a new user can adopt in their first hour. Dashboard, migration tooling, reference CI template, OpenAPI spec, metrics, structured logs.

**Estimated effort:** ~3–4 weeks solo.

---

### B1. JSON API surface + pagination + error shape polish — ~4 days

**Deliverables:** All JSON read endpoints from [design.md](design.md) §7.

**Tasks:**

1. `GET /api/v1/channels` — list with stats (`package_count` from a SQL count).
2. `GET /api/v1/packages` — with `?channel=&package=&limit=&offset=`; response includes `binaries` list-column (the §7 exception). Join in SQL.
3. `GET /api/v1/cells` — dump of matrix.yaml as JSON.
4. `GET /api/v1/events` — `?since_id=&limit=&channel=&package=&type=`. `X-Total-Count` header. Default limit 100, max 500.
5. Pagination helper + `X-Total-Count` middleware.
6. Error envelope audit: every handler returns consistent shape on error paths.
7. Integration tests for all list endpoints including filters and pagination.

**Acceptance:** Scripted walkthrough against a running server produces expected shapes for every endpoint. `jq`-able, data.frame-able in R.

---

### B2. OpenAPI 3 spec — ~3 days

**Deliverables:** `openapi/openapi.yaml` describing every v1 endpoint; served at `/api/v1/openapi.json`.

**Tasks:**

1. Hand-write `openapi/openapi.yaml` from [design.md](design.md) §7. All paths, all params, all request/response schemas, error schema with `error_code` enum.
2. Validate in CI: `go run github.com/pb33f/libopenapi/cmd/vacuum lint openapi/openapi.yaml` (or `redocly lint`).
3. Serve at `/api/v1/openapi.json` via `embed.FS`. Convert YAML → JSON at build or startup.
4. Contract test: for a handful of endpoints, assert that live responses conform to the spec schema (e.g., using `kin-openapi` validator).
5. Link to `/api/v1/openapi.json` from README and quickstart.

**Acceptance:** `curl .../api/v1/openapi.json | jq .` returns a valid OpenAPI 3 doc. `vacuum lint` passes with no errors. Contract test matches responses to schemas.

---

### B3. Observability — ~2 days

**Deliverables:** `/health`, `/metrics`, structured logs.

**Tasks:**

1. `internal/api/health.go` — runs DB ping + CAS directory writability check. `{status: ok}` on success, `503` with degraded subsystems listed otherwise.
2. `internal/api/metrics.go` — Prometheus registry with counters: `packyard_publish_total{channel}`, `packyard_yank_total{channel}`, `packyard_delete_total{channel}`, `packyard_http_requests_total{path,method,status}`, `packyard_http_request_duration_seconds` histogram, `packyard_cas_bytes_total`. Plus default Go runtime metrics.
3. `slog` JSON handler for stdout. Middleware logs each request with `request_id`, method, path, status, duration, user (from token label).
4. Acceptance test: scrape `/metrics` with promtool.

**Acceptance:** `curl /api/v1/health` returns 200 on a healthy server, 503 when DB is stopped. `promtool check metrics /metrics` passes. Logs are JSON, parseable by jq.

---

### B4. Operator dashboard — ~5 days

**Deliverables:** Server-rendered HTML UI at `/ui/` showing channels, events, cells, storage.

**Tasks:**

1. `internal/ui/handlers.go` with routes `/ui/`, `/ui/channels`, `/ui/channels/{name}`, `/ui/events`, `/ui/cells`, `/ui/storage`.
2. `internal/ui/templates/*.html` — layout base template + page templates. Go `html/template` with `{{define}}` + `{{template}}`.
3. `/ui/` dashboard: card per channel with package count, recent events, storage used.
4. `/ui/channels/{name}`: list of packages + versions with yank status.
5. `/ui/events`: paginated table of recent events.
6. `/ui/cells`: list of configured cells with coverage % per channel.
7. `/ui/storage`: CAS bytes total, top-N largest packages.
8. Minimal CSS — plain, no framework. Embed as static assets.
9. Auth: UI requires `read:*` or any valid token; simple session cookie issued after token login at `/ui/login`. OR: HTTP Basic auth over the bearer token. Simplest: just require a bearer token via cookie named `packyard_token`, set by `/ui/login` form. Session storage: signed cookie, no server-side sessions.
10. Integration test: unauthenticated request to `/ui/` redirects to `/ui/login`; valid token gets the dashboard.

**Acceptance:** Log into `/ui/` with the admin token. See live channels, recent publishes, storage numbers. No JavaScript framework; no Node toolchain.

---

### B5. Reference CI workflow — ~2 days

**Deliverables:** Working publish workflow template in `examples/ci/`.

**Tasks:**

1. Copy the YAML from [design.md](design.md) §8.4 into `examples/ci/publish.yml`.
2. Verify it works on real GitHub Actions against a deployed dev packyard (use a public package like `jsonlite` for the test, or a tiny scaffolded pkg).
3. Test on Gitea Actions too; note any divergences in `examples/ci/README.md`.
4. Write `examples/ci/README.md` explaining the template, what to customise, where secrets go.

**Acceptance:** A fresh repo with the template (plus a DESCRIPTION file) publishes successfully on both GitHub Actions and Gitea Actions. The README is enough to adapt without outside help.

---

### B6. Migration tools — ~5 days

**Deliverables:** `packyard admin import --from drat` and `--from git`.

**Tasks:**

1. `internal/importers/drat.go`: given a drat repo URL, walk `src/contrib/PACKAGES`, download each tarball, call the local publish path with source-only (no binaries).
2. `internal/importers/git.go`: given a git URL and branch, shell out to `git clone`, run `R CMD build`, publish source-only (binaries via CI is the long-term path).
3. CLI subcommands `packyard-server admin import drat <url> --channel <name>` and `admin import git <url> --branch <b> --channel <name>`.
4. Local publish path: avoid HTTP round-trip; call the same publish logic in-process with an internal admin identity.
5. Progress output with per-package status.
6. Integration tests: mock drat repo served via local HTTP; mock git repo in a temp dir.
7. Doc: `docs/migration.md` — from drat to packyard, from install_github to packyard.

**Acceptance:** Point `admin import drat` at a real drat repo (there are public ones for demo) → all packages appear in the target channel. Point `admin import git` at a local R-package repo → package appears in the channel.

---

### B7. Admin ops — ~2 days

**Deliverables:** Admin CLI commands beyond importers.

**Tasks:**

1. `packyard-server admin channels list` — lists channels with stats (count, overwrite_policy, default flag).
2. `packyard-server admin cells list` — lists cells with coverage stats.
3. `packyard-server admin cells show <name>` — detail view including packages missing binaries for this cell.
4. `packyard-server admin gc` — walks DB, computes live sha256 set, calls `cas.GC(liveSet)`. Reports freed bytes.
5. `packyard-server admin reindex` — regenerates PACKAGES caches from DB (recovery op).
6. These share code paths with the API handlers; mostly thin CLI shims.
7. Integration tests per command against a temp DB.

**Acceptance:** Each admin command runs cleanly against a live or temp DB. `admin gc` correctly reclaims blobs from a known-orphaned state.

---

**Phase B Definition of Done:**

- `/ui/` dashboard shows live state.
- OpenAPI spec lints and matches live responses.
- `/metrics` scrapes cleanly; logs are JSON.
- Reference CI workflow publishes successfully on GH and Gitea.
- Migration from a real drat repo works end-to-end.
- `admin gc` reclaims orphaned blobs.

---

## Phase C — Launch

**Goal:** v1.0.0 is tagged, installable, and someone who has never seen packyard succeeds with the quickstart on first try.

**Estimated effort:** ~1–2 weeks solo.

---

### C1. 5-minute quickstart — ~2 days

**Deliverables:** `docs/quickstart.md` that takes a brand-new user from zero to first published package in 5 minutes.

**Tasks:**

1. Draft the 5-command path: `docker run packyard-server` → get admin token → create publish token → build sample R package → curl publish → see it in dashboard.
2. Test the flow on a clean machine (a VM or fresh container) with no prior state.
3. Explicit success criterion to verify: all steps copy-pasteable from the doc; total wall-clock time under 5 minutes including Docker pull.
4. Include screenshots of dashboard after publish.
5. Link from README.

**Acceptance:** An internal reviewer who has never touched packyard follows the quickstart and succeeds. Measured end-to-end time under 5 minutes.

---

### C2. README — ~1 day

**Deliverables:** Professional, honest README.

**Tasks:**

1. One-paragraph positioning from [design.md](design.md) §0.
2. Target users callout (R consultancies, mid-size R shops).
3. "What it is not" section: not a PPM replacement, not a client CLI, not a CRAN mirror in v1.
4. Install instructions (Docker + binary).
5. Link to quickstart.
6. Roadmap section naming the v1.x air-gap feature explicitly (per the positioning work).
7. Badges: license, CI status, latest release.

**Acceptance:** README reads like a real OSS project, not an internal TODO.

---

### C3. Reference docs — ~2 days

**Deliverables:** `docs/api.md`, `docs/config.md`, `docs/admin.md`.

**Tasks:**

1. `docs/api.md`: references the OpenAPI spec, shows curl examples for each endpoint, covers the error shape, enumerates `error_code` values.
2. `docs/config.md`: `channels.yaml`, `matrix.yaml`, server config; validation rules; examples for common patterns (dev/test/prod, adding a RHEL cell).
3. `docs/admin.md`: all admin commands with examples.
4. `docs/migration.md`: already written in B6, ensure linked.
5. Cross-link all docs from README.

**Acceptance:** Each doc page stands alone. No dead links.

---

### C4. Backup & restore runbook — ~1 day

**Deliverables:** `docs/backup-restore.md`.

**Tasks:**

1. Procedure for consistent SQLite snapshot (use `VACUUM INTO` or the sqlite backup API; warn about plain `cp` under WAL).
2. CAS directory backup (rsync-compatible; safe with concurrent writes because content-addressed).
3. Cadence recommendations.
4. Disaster recovery walkthrough: restore from backup, verify integrity, reissue tokens if needed.
5. Test the runbook: kill the data dir, restore from backup, verify service resumes.

**Acceptance:** Running through the runbook on a test deploy recovers it to a known good state.

---

### C5. Release engineering — ~2 days

**Deliverables:** `v1.0.0` tagged and released; Docker image on `ghcr.io/<owner>/packyard:v1.0.0` and `:latest`.

**Tasks:**

1. Tighten `.goreleaser.yaml`: Linux amd64 + arm64 binaries; Darwin amd64 + arm64 (best-effort, not a primary target); checksums; SBOM; GitHub release notes template.
2. CI step to run GoReleaser on tag push.
3. Docker image push to ghcr.io.
4. Version stamping: `-ldflags "-X github.com/<owner>/packyard/internal/version.Version=$VERSION"`.
5. Smoke-test the released artifacts: download a binary from the release page, run it, confirm `-version` matches.
6. Announcement draft — where (Hacker News? r-bloggers? Posit Community?); hold off publishing until the quickstart has been verified by one external tester.

**Acceptance:** `git tag v1.0.0 && git push --tags` produces a release with binaries + a Docker image. Installing from the release works.

---

**Phase C Definition of Done:**

- `v1.0.0` tagged; release artifacts present and verified.
- Quickstart tested end-to-end by someone who didn't build packyard.
- README, api.md, config.md, admin.md, backup-restore.md all published.
- Docker image runnable as-is.

---

## Global concerns

### Risks

Status updated after the v1.0.x cut:

1. **CRAN-protocol gotchas**: R's expectations of `PACKAGES` file structure and URL shapes have undocumented corners. Mitigation: the CRAN-protocol smoke test in A5 is the early-warning system. Expect 1–2 days of debugging this during A5. — **Mostly mitigated.** HTTP-level compliance is covered by [`cran_protocol_test.go`](internal/api/cran_protocol_test.go) and a real R client has installed packages against a deployed packyard. The rocker-based `Rscript install.packages()` CI job never landed; tracked as a v1.1 item.
2. **Multipart edge cases**: giant uploads, slow clients, malformed manifests. Mitigation: integration tests for common failure modes in A4. — **Mitigated.** `http.MaxBytesReader` caps publish uploads at 2 GiB ([`publish.go`](internal/api/publish.go)); `publish_test.go` covers happy and sad paths. Fuzz deferred to v1.1 as planned.
3. **SQLite WAL + concurrency**: write-heavy periods could contend. Mitigation: keep writes in short tx, use `BEGIN IMMEDIATE` for writes. At v1 scale this is unlikely to bite. — **Partial — live gap.** WAL + `busy_timeout(5000)` + foreign keys are set in [`db.go`](internal/db/db.go), but every write site still uses `BeginTx(ctx, nil)` which starts a deferred tx. Under concurrent writes this can surface `SQLITE_BUSY_DEADLOCK` that `busy_timeout` alone can't resolve. Tracked as v1.1 item 1.
4. **Distroless image missing things**: sometimes packages want `ca-certificates` or similar. Mitigation: build test — curl the release image against real TLS endpoints during CI. — **Mitigated.** Released images run clean on arm64 + amd64; `-version` smoke-tested in the release workflow.
5. **Solo maintenance bandwidth post-launch**: the v1.x air-gap feature is the next big thing. Don't over-commit to post-launch features until v1 adoption is real. — **Active concern.** Manage by keeping v1.1 scope to the five follow-ups below.

### What's intentionally not in v1 (forward references)

Restating from [design.md](design.md) §12 so the implementation plan matches:

- Server-side builds (deferred to v1.x).
- Air-gap + CRAN mirror (committed v1.x).
- Runtime cell/channel management APIs.
- Multi-tenant isolation, OIDC/SAML, Postgres backend.
- Rust client, content-addressable client cache, PubGrub — **not planned**, partner with uvr/rv.

### Sequencing notes

- A1 → A2 → A3 are hard prereqs; no parallelism.
- A4 → A5 → A6: A4 needs A2; A5 needs A4; A6 can run in parallel with A5 if needed.
- B1 depends on A4+A5 being stable.
- B2 (OpenAPI) can start in parallel with B1 once endpoints are locked.
- B3, B4, B5, B6, B7 are largely independent; can reorder based on energy.
- C phase depends on all of B being stable.

### Definition of "v1 ships"

All of:
- Phase A, B, C DoDs above.
- README reads well to an outsider.
- Quickstart verified by someone who didn't build packyard.
- The author can replace their current "share internal R packages" workflow with packyard on their own machine.

---

## Verification

After implementation, verify end-to-end by:

1. Running `make build && make test` — all tests pass.
2. `docker run -v $(pwd)/data:/data ghcr.io/<owner>/packyard:v1.0.0` — server starts.
3. Following `docs/quickstart.md` from zero — first publish succeeds in under 5 minutes.
4. Running the reference CI workflow from `examples/ci/publish.yml` against a real R package on both GitHub Actions and Gitea Actions.
5. From an RStudio session: `install.packages("myInternal", repos = "http://localhost:8080/")` succeeds and the package loads.
6. Opening `http://localhost:8080/ui/` and seeing the published package + the publish event.
7. Running `packyard-server admin gc` after a couple of dev-channel overwrites and seeing disk reclaimed.

If all seven steps pass, v1 is real.

---

## Post-v1 follow-ups (v1.1)

Items surfaced by the v1.0.x cut that didn't block release but should land before we attempt v1.x feature work (air-gap + CRAN mirror). Each is small — half-day to one day of focused work. Sequencing is independent; pick on energy.

Breaking changes are fine during this window — see [CLAUDE.md](CLAUDE.md).

### F1. `BEGIN IMMEDIATE` for write transactions

The Risks section flagged this during planning but the mitigation never shipped. Every write site uses `db.BeginTx(ctx, nil)` which starts a *deferred* transaction; the writer only takes `RESERVED` on its first write, which under concurrency with other writers can drop into `SQLITE_BUSY_DEADLOCK` that `busy_timeout(5000)` cannot recover from.

- Code sites: [`internal/api/publish.go`](internal/api/publish.go), [`internal/api/yank.go`](internal/api/yank.go), [`internal/api/delete.go`](internal/api/delete.go), [`internal/db/migrate.go`](internal/db/migrate.go).
- Fix: wrap write tx in a helper that runs `BEGIN IMMEDIATE` explicitly (sqlite `database/sql` doesn't map `sql.LevelSerializable` onto the right BEGIN verb on `modernc.org/sqlite`; cheapest reliable fix is a raw `_, err := db.ExecContext(ctx, "BEGIN IMMEDIATE")` then build a `*sql.Tx` via the existing connection).
- Tests: add a concurrent-publish test that fires N parallel unique versions at the same channel and asserts zero 500s. Confirm it deadlocks on today's code before landing the fix.

### F2. Fuzz targets for multipart publish

Committed to this phase by the original plan. Multipart parsing is the most hostile-input surface we ship.

- New file: `internal/api/publish_fuzz_test.go`.
- Targets: `FuzzPublishMultipartBody` (raw multipart bytes into `handlePublish`), `FuzzPublishManifest` (just the manifest JSON, with a pre-built valid multipart envelope around it).
- Run under `-fuzz=. -fuzztime=30s` in CI on a nightly schedule — not on every push.

### F3. Release-image smoke test in CI

The v1.0.1 auto-bootstrap bug slipped to GHCR and was caught by hand. A simple post-release job would have caught it in the release pipeline.

- New workflow: `.github/workflows/post-release.yml`, `workflow_run: Release (completed)` trigger.
- Steps: pull `ghcr.io/schochastics/packyard:${{ github.event.release.tag_name }}`, `docker run -d -p 8080:8080 … -allow-anonymous-reads`, `curl /health` with retry-until-200, `docker run --rm <image> -version` and grep the tag.
- On failure: open a release-regression issue automatically.

### F4. `Rscript install.packages()` CI job

Closes the other half of Risk #1 — our current CRAN-protocol test is HTTP/byte-level, not a real R client round-trip. Low probability of surprise given real users already install from a live packyard, but cheap and definitive.

- New job in `.github/workflows/ci.yml` using `rocker/r-ver:4.4`.
- Boot a packyard container, publish a minimal test package with curl, then `R -e 'install.packages("testpkg", repos="http://packyard:8080/")'` and assert the install succeeded and the library loads.
- Isolate in a docker-compose-shaped network; no host R install needed.

### F5. Doc drift cleanup

Small stuff the release surfaced:

- [`docs/quickstart.md`](docs/quickstart.md) "From source" path tells the reader to run `-init` separately. Auto-bootstrap on `runServe` landed in v1.0.1 — decide: keep `-init` as explicit (doc accurate either way) or drop it to match the Docker one-liner. Pick one, update.
- Add a one-paragraph "Container tag convention" note to [`README.md`](README.md) or [`docs/api.md`](docs/api.md): git tag is `vX.Y.Z`, GHCR tag is `X.Y.Z` (no `v`). Saves a future user hitting `manifest unknown`.
- [`CLAUDE.md`](CLAUDE.md) already has this for future Claude sessions; the user-facing equivalent is what's missing.

### F6 (open question — no action yet). Drop `-allow-anonymous-reads` from the shipped compose default

The `examples/compose/docker-compose.yml` ships with anonymous reads on to match the quickstart's zero-friction promise. A production-shaped default would force the operator to mint a token before any install works. Breaking change for evaluators; clearer signal for anyone running this for real.

Don't unilaterally change. If we see anyone using compose in anger and hitting 401s, flip it.

---
