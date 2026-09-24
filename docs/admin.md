# Admin reference

Every admin operation is either a CLI subcommand on `packyard-server` or
a scoped HTTP endpoint. The CLI path is what operators use on the
server host; the HTTP path is what CI and scripts reach for. Both are
covered below.

## Server lifecycle

### `packyard-server -init -data <dir>`

Bootstraps a data directory. Writes default `channels.yaml` +
`matrix.yaml` (if absent), creates `db.sqlite`, applies migrations,
and reconciles channels. Idempotent — safe to run against an existing
data dir.

### `packyard-server -data <dir>` (or `-config <path>`)

Starts the HTTP server. On start it re-reads `channels.yaml`,
reconciles against the DB (additions/updates only — never deletes),
and begins listening. SIGINT/SIGTERM triggers a 30-second graceful
shutdown; in-flight publishes get a chance to finish.

### `packyard-server -version`

Prints the version string and exits.

### `packyard-server -healthcheck [-healthcheck-url <url>]`

GETs `/health` and exits 0 on 200, 1 otherwise, with a 3 s timeout.
The distroless image has no curl, so container health checks run the
binary itself:

```yaml
healthcheck:
  test: ["CMD", "/usr/local/bin/packyard-server", "-healthcheck"]
```

Without `-healthcheck-url` it probes `127.0.0.1` on the port from the
listen address. Pass `-config` so it sees a non-default `listen`.
Over TLS, certificate verification is skipped, because the probe
targets loopback.

### `packyard-server -mint-token -data <dir> -scopes <csv> [-label <s>]`

Creates a token directly against the DB without touching the HTTP
surface. The plaintext token is printed on stdout (so it composes
with shell pipelines like `TOKEN=$(packyard-server -mint-token …)`);
human-oriented context goes to stderr. This is the bootstrap path —
after you have one `admin`-scoped token, prefer
`POST /api/v1/admin/tokens` for everything else.

```sh
ADMIN=$(packyard-server -mint-token -data ./data -scopes admin -label bootstrap)
```

## Admin CLI subcommands

Invocation grammar:

```
packyard-server admin [-data DIR] [-config PATH] <verb> [args…]
```

`-data` / `-config` resolve the same way as the top-level flags:
`-config` wins when set, otherwise `-data` picks the bootstrap dir
(`./data` by default). Positional args and `-flag` args can appear in
either order — the admin dispatcher reorders them internally so
`admin import drat <url> -channel dev` and
`admin import drat -channel dev <url>` both work.

### `admin import drat <repo-url> -channel <name>`

Walks a drat-shaped HTTP repo (`<repo-url>/src/contrib/PACKAGES` and
per-package tarballs), downloading each tarball and publishing it
in-process. Source-only — binaries are not part of the drat format.

Per-package failures go into a `failed` list but don't abort the run;
the command exits non-zero at the end if anything failed.

```sh
packyard-server admin -data ./data import drat https://drat.example.org -channel dev
```

Event actor is `import-drat`; each import row has its tarball URL in
the event note column.

### `admin import git <repo-url> [-branch <b>] -channel <name>`

Shallow-clones `repo-url` at `branch` into a temp dir, runs `R CMD
build`, then publishes the resulting tarball. Requires both `git` and
`R` on `PATH`.

```sh
packyard-server admin -data ./data import git \
  https://git.example.org/foo.git -branch main -channel dev
```

Package name + version are parsed from `DESCRIPTION` before the build
step so the output message is meaningful even if `R CMD build` fails.
Temp clone and build dirs are cleaned up on exit. `R CMD build` runs
code from the repository (it builds vignettes), so only import
repositories you trust.

### `admin import bundle <path-or-targz> -channel <name>`

Imports a `packyard-bundle/{1,2}` bundle (the format produced by
[`examples/bundler/build-bundle.R`](../examples/bundler/build-bundle.R))
into the named channel. The path argument may be a directory laid out
as documented in [design.md §10](../design.md) or a `.tar.gz` / `.tgz`
archive of one — archives are extracted to a tempdir for the duration
of the import.

```sh
packyard-server admin -data ./data import bundle \
  ./cran-r4.4-2026q1.tar.gz -channel cran-r4.4-2026q1
```

The target channel must already exist in `channels.yaml`. The
importer does not create channels, because `channels.yaml` is the
source of truth for their policy. That policy applies to every
imported version, as for a publish. On an **immutable** channel,
identical bytes are skipped, and different bytes for an existing
version fail that package. A **mutable** channel is overwritten. Put
air-gap snapshots in immutable channels. Migrations
([migration.md](migration.md)) may target any channel.

The output line annotates the bundle's `kind` (and `cell` for binary
bundles) so the operator can tell at a glance which run-mode it was:

```
imported=42 skipped=0 failed=0 snapshot=cran-r4.4-2026q1 kind=source
imported=42 skipped=0 failed=0 snapshot=cran-r4.4-2026q1 kind=binary cell=r-4.4
```

What happens, in order:

1. **Manifest validation.** Schema must be `packyard-bundle/1` or
   `packyard-bundle/2`; unknown schemas are rejected at the gate. v1
   manifests are normalised to v2's shape internally.
2. **Cell validation (binary bundles only).** The bundle's top-level
   `cell` must match a cell in `matrix.yaml`; if not, the import aborts
   before pre-flight.
3. **Pre-flight sha256 verification.** Every blob (source or binary)
   is hashed and compared to its manifest entry. **Any mismatch aborts
   before any side effects** — neither CAS nor the DB is touched.
4. **Per-package import.** Source bundles publish each tarball through
   the same path CI uses — CAS dedup, idempotent re-import, and
   immutable-policy enforcement all work the same way. Binary bundles
   attach binaries to the existing `(channel, name, version)` row;
   missing source rows surface as `failed=` with `source row not
   found; import the source bundle first` and don't block other
   packages.

Re-running the same bundle is cheap and idempotent: matching content
already in CAS counts as `skipped`, mismatched content on an immutable
channel surfaces as a per-package failure.

For the binary-bundle workflow (source first, then binaries-per-cell),
see [airgap.md §Pre-built binaries](airgap.md#pre-built-binaries-via-posit-public-package-manager).

### `admin channels list`

Aligned text table of every channel with policy, default flag, package
count, and most-recent publish time.

```sh
$ packyard-server admin -data ./data channels list
NAME  POLICY     DEFAULT  PACKAGES  LATEST PUBLISH
prod  immutable  yes      42        2026-04-18 14:23:11
dev   mutable             71        2026-04-22 09:02:44
test  mutable             42        2026-04-20 17:55:00
```

### `admin cells list`

Every cell declared in `matrix.yaml` with binary count, coverage
(distinct packages with a binary / total packages), and total
uploaded bytes.

```sh
$ packyard-server admin -data ./data cells list
distro jammy (amd64), default R 4.5

CELL   R    BINARIES  COVERAGE  SIZE
r-4.4  4.4  40        40/42     512 MiB
r-4.5  4.5  38        38/42     498 MiB
```

### `admin cells show <cell-name>`

The matrix entry, followed by every non-yanked package version that
has no binary for that cell. `admin missing-binaries` narrows this to
the current version of each package. Targets the "added a new cell, which packages
still need to build?" workflow.

```sh
$ packyard-server admin -data ./data cells show r-4.5
cell r-4.5
  distro jammy
  arch   amd64
  r      4.5

CHANNEL  PACKAGE  VERSION  PUBLISHED
prod     foo      1.0.0    2026-04-18 14:23:11
prod     bar      0.2.1    2026-04-19 10:00:02
…
```

### `admin gc [-dry-run] [-min-age 1h] [-force]`

Reclaims CAS blobs that no longer appear in any package or binary
row. Walks the CAS tree, checks each blob's sha256 against a live set
built from `packages.source_sha256 ∪ binaries.binary_sha256`, removes
the orphans.

```sh
# Preview:
packyard-server admin -data ./data gc -dry-run
# Reclaim:
packyard-server admin -data ./data gc
```

Output format:

```
live blobs referenced by DB: 284
scanned=292 removed=8 freed=17.3 MiB skipped_young=1 skipped_stray=0 tmp_removed=0
```

- `scanned` — total blob files walked (matching the `<aa>/<rest>`
  shape).
- `removed` — deleted in this run.
- `freed` — bytes reclaimed (sum of removed sizes).
- `skipped_young` — unreferenced blobs newer than `-min-age`, kept.
- `skipped_stray` — files under the CAS root that don't look like
  valid blobs. These are left alone (likely operator probes) and
  counted here for visibility.
- `tmp_removed` — abandoned partial uploads in `cas/tmp/` older than
  `-min-age` (left behind when the server was killed mid-upload).

When to run: after overwrites on mutable channels, after a batch of
`DELETE /api/v1/packages/…`, or on a schedule (e.g. weekly cron).
Yanked packages' blobs are retained — yank is a visibility op, not a
deletion.

Safety:

- **Running against a live server is safe.** A publish writes its
  blobs before the DB row referencing them commits. `-min-age`
  (default 1h) keeps every unreferenced blob newer than that, and a
  publish that reuses an existing blob refreshes its mtime. Raise it if
  uploads can take longer than an hour; `-min-age 0` is only safe with
  the server stopped.
- **Refuses an empty live set.** If the DB references no blobs at all
  but the CAS holds some, gc stops: that almost always means the wrong
  `-data` dir or a replaced `db.sqlite`. Pass `-force` if the
  repository really is empty.
- Like every admin verb, gc fails if `<data>/db.sqlite` doesn't exist
  instead of creating an empty one.

### `admin reindex`

Verifies that every sha256 the DB references has a matching blob in
CAS, and fills in DESCRIPTION metadata (the dependency fields in
`PACKAGES`, a binary's `Built:`) for rows that lack it. The server does
the same metadata backfill once at startup. Packyard doesn't persist
a `PACKAGES` index: it is built from the DB on request and cached in
memory, so this is the actual recovery op after a DB or CAS restore.

```sh
packyard-server admin -data ./data reindex
```

Missing blobs are printed as a table:

```
CHANNEL  PACKAGE  VERSION  COLUMN                          SHA256
prod     foo      1.0.0    source                          abc…
prod     foo      1.0.0    binary/r-4.5                    def…
```

Non-zero exit when any mismatches are found, so the command composes
in healthcheck scripts.

### `admin backup -out <dir>` / `admin backup -verify <dir>`

`-out` writes a consistent snapshot of the DB, every referenced blob
and the config files into `<dir>`. It is safe while the server runs,
and it is incremental when `<dir>` is reused. `-verify` re-hashes
every blob in a backup, checks that the DB's references are all
present, and runs `PRAGMA integrity_check`; it exits non-zero on any
problem. See [backup-restore.md](backup-restore.md).

### `admin restore -from <dir> [-data <dir>] [-force]`

Verifies a backup and writes it into the data dir. The server must be
stopped. It refuses a data dir that already has a DB or blobs unless
given `-force`.

### `admin token-gen`

Prints a new token on line 1 and its sha256 on line 2, and touches no
database. Use it for [tokens provisioned from config](config.md#tokens-from-config).
The CI secret gets the token; the server gets a `sha256_file` with
the hash.

### `admin missing-binaries -channel <name> [-cell <cell>]`

Lists, for the current version of every package on the channel, the
cells with no binary. This is the work list after adding an R version
to `matrix.yaml`.

## Admin HTTP endpoints

All under `/api/v1/admin/`. Every endpoint requires the `admin` scope.
See [api.md](api.md) for full request/response schemas.

### `POST /api/v1/admin/tokens`

Mint a token. Plaintext is returned once.

```sh
curl -X POST http://packyard.corp/api/v1/admin/tokens \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"label":"ci","scopes":["publish:dev","read:*"]}'
```

### `GET /api/v1/admin/tokens`

List tokens. `last_used_at` tells you whether a token is still in use.
`source` is `api` for minted tokens and `config` for tokens from
`server.yaml`.

### `DELETE /api/v1/admin/tokens/{id}`

Revoke. Immediate effect — packyard resolves the token against the DB on
every request. Config tokens are refused with 409; remove them from
`server.yaml` and restart instead.

## Observability

### `/health`

Public. Returns 200 with per-subsystem status when everything is up,
503 `unavailable` when any subsystem fails its probe.

```sh
curl -s http://packyard.corp/health | jq
```

Subsystem checks:

- **`db`** — `SELECT 1` against SQLite.
- **`cas`** — create + remove a file under `cas/tmp/`.
- **`matrix`** — matrix config loaded and has at least one cell.

### `/metrics`

Public Prometheus text format, on the main listener, or only on
`metrics_listen` when that is set ([config.md](config.md#separate-metrics-listener)). A hermetic registry is used, so only
packyard-owned metrics appear (no Go stdlib metrics leaking through).

| Metric | Labels | Meaning |
|---|---|---|
| `packyard_http_requests_total` | `method`, `status` | Request counter at the HTTP layer. URL path is intentionally not a label — cardinality discipline. |
| `packyard_http_request_duration_seconds` | `method`, `status` | Histogram, buckets `[5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s, 30s]`. |
| `packyard_publish_total` | `channel`, `result` | Publishes: `created` / `overwrote` / `already_existed`. Binary attaches: `binary_attached` / `binary_overwrote` / `binary_already_existed`. |
| `packyard_yank_total` | `channel` | Counter. |
| `packyard_delete_total` | `channel` | Counter. |
| `packyard_cas_bytes` | — | Gauge of `SUM(source_size) + SUM(size)` across the DB. Logical, not physical — use `du -sh <data>/cas` for on-disk. |
| `packyard_proxy_fetch_total` | `channel`, `kind`, `outcome` | Proxy-channel upstream fetches. `kind` is `source` / `binary` / `index`; `outcome` is `ok` / `upstream_error` / `stale` (served a cached index after an upstream failure). |
| `packyard_token_create_total` | — | Token mints through the HTTP endpoint. `-mint-token` runs in its own process and isn't counted. |
| `packyard_token_revoke_total` | — | Token revokes through the HTTP endpoint. |

### Access log

Every request produces one structured log line on stderr (slog
`INFO`): `method`, `path`, `status`, `bytes`, `duration_ms`,
`remote`, `user_agent`, `request_id`. Behind a proxy listed in
`trusted_proxies`, `remote` is the client address from
`X-Forwarded-For`. The same `request_id` appears in every error
envelope body so a 500 can be correlated with its log line.

### Audit log

Every publish, yank, delete, token create, token revoke, and import
writes a row to the `events` table. Read it via
`GET /api/v1/events` (admin-scope) or the
[operator dashboard](../internal/ui) at `/ui/events`.

## Upgrade procedure

1. `admin backup -out <dir>`, then `admin backup -verify <dir>`.
2. Pull the new image (or swap the binary) and restart. Migrations
   are applied on startup, by the server only: admin commands refuse
   to run against a DB whose schema doesn't match their binary
   exactly, so run them with the same version as the server.
3. There are no down migrations. An older binary refuses to start on
   a DB a newer one has migrated. To roll back, restore the backup
   onto the old version.

Read the release notes first. Until adoption picks up, v1.x releases
may contain breaking changes, and the notes call each one out.

## Permissions

The image runs as uid/gid 65532 (distroless `nonroot`). A bind-mounted
data dir must be owned accordingly:

```sh
chown -R 65532:65532 /srv/packyard/data
```

Named Docker volumes get the right ownership automatically on first
use. Config files and token secrets only need to be readable by that
uid, and can be mounted read-only.

## Restarts and config changes

- `server.yaml`, `channels.yaml` and `matrix.yaml` are read at start.
  Changes take effect on restart.
- Channel reconcile only adds. A channel removed from `channels.yaml`
  keeps its DB row and packages, and the server warns at startup.
- Removing a cell from `matrix.yaml` makes its binaries unreachable,
  and clients on that R version fall back to source. The blobs stay
  referenced until the rows are deleted.
- Adding a cell: clients on that R version get source until CI
  backfills binaries (`admin missing-binaries`,
  `examples/ci/packyard-backfill.sh`).
- Config tokens are reconciled on every start (see
  [config.md](config.md#tokens-from-config)).

## Running as a service

A production deployment typically has:

- **Config mounted read-only:** `server.yaml` with `channels_file`
  and `matrix_file` pointing into the mount, `public_url`,
  `trusted_proxies` for the reverse proxy, `metrics_listen` on an
  internal address, and `tokens:` for CI and read-only clients.
- **Data dir** on a volume owned by 65532.
- **Health check:** `packyard-server -config … -healthcheck`.
- **Scheduled backup:** `admin backup -out` hourly or daily into
  storage outside the data volume, plus a periodic `-verify`.

[examples/compose/production/](../examples/compose/production/) puts
all of these together.
