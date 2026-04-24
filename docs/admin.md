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

### `packyard-server -data <dir> -allow-anonymous-reads`

Starts the server with `allow_anonymous_reads` forced on regardless of
what `server.yaml` says. Opens the **default channel only** to
unauthenticated CRAN-protocol reads — useful for local smoke tests
and for deployments that want `install.packages()` to work without R
clients carrying a bearer token. Non-default channels stay scoped.
See [config.md](config.md) for the YAML equivalent.

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
Temp clone and build dirs are cleaned up on exit.

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
CELL                       OS                  ARCH   R    BINARIES  COVERAGE  SIZE
ubuntu-24.04-amd64-r-4.5   linux ubuntu-24.04  amd64  4.5  40        40/42     512 MiB
ubuntu-24.04-arm64-r-4.5   linux ubuntu-24.04  arm64  4.5  38        38/42     498 MiB
```

### `admin cells show <cell-name>`

The matrix entry, followed by every live package that does NOT have a
binary for that cell. Targets the "added a new cell, which packages
still need to build?" workflow.

```sh
$ packyard-server admin -data ./data cells show rhel-9-amd64-r-4.5
cell rhel-9-amd64-r-4.5
  os     linux rhel-9
  arch   amd64
  r      4.5

CHANNEL  PACKAGE  VERSION  PUBLISHED
prod     foo      1.0.0    2026-04-18 14:23:11
prod     bar      0.2.1    2026-04-19 10:00:02
…
```

### `admin gc [-dry-run]`

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
scanned=292 removed=8 freed=17.3 MiB skipped_stray=0
```

- `scanned` — total blob files walked (matching the `<aa>/<rest>`
  shape).
- `removed` — deleted in this run.
- `freed` — bytes reclaimed (sum of removed sizes).
- `skipped_stray` — files under the CAS root that don't look like
  valid blobs. These are left alone (likely operator probes) and
  counted here for visibility.

When to run: after overwrites on mutable channels, after a batch of
`DELETE /api/v1/packages/…`, or on a schedule (e.g. weekly cron).
Yanked packages' blobs are retained — yank is a visibility op, not a
deletion.

Safety: gc is an admin-invoked op, not a background task. Running it
against a live server is not a goal of the v1 design — stop the
server or accept that a concurrent publish-and-gc can race (the
window is small and the worst case is a freshly-published blob
getting deleted, which the publish then notices via its own CAS
write — still not desirable).

### `admin reindex`

Verifies that every sha256 the DB references has a matching blob in
CAS. Packyard doesn't persist a `PACKAGES` index — the file is rebuilt
from the DB on every request and cached in memory for 5 minutes — so
this is the actual recovery op after a DB or CAS restore.

```sh
packyard-server admin -data ./data reindex
```

Missing blobs are printed as a table:

```
CHANNEL  PACKAGE  VERSION  COLUMN                          SHA256
prod     foo      1.0.0    source                          abc…
prod     foo      1.0.0    binary/ubuntu-24.04-amd64-r-4.5  def…
```

Non-zero exit when any mismatches are found, so the command composes
in healthcheck scripts.

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

### `DELETE /api/v1/admin/tokens/{id}`

Revoke. Immediate effect — packyard resolves the token against the DB on
every request.

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

Public Prometheus text format. A hermetic registry is used, so only
packyard-owned metrics appear (no Go stdlib metrics leaking through).

| Metric | Labels | Meaning |
|---|---|---|
| `packyard_http_requests_total` | `method`, `status` | Request counter at the HTTP layer. URL path is intentionally not a label — cardinality discipline. |
| `packyard_http_request_duration_seconds` | `method`, `status` | Histogram, buckets `[5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s, 30s]`. |
| `packyard_publish_total` | `channel`, `result` | `result` is `created` / `overwrote` / `already_existed`. |
| `packyard_yank_total` | `channel` | Counter. |
| `packyard_delete_total` | `channel` | Counter. |
| `packyard_cas_bytes` | — | Gauge of `SUM(source_size) + SUM(size)` across the DB. Logical, not physical — use `du -sh <data>/cas` for on-disk. |
| `packyard_token_create_total` | — | Token mints (both CLI and HTTP). |
| `packyard_token_revoke_total` | — | Token revokes. |

### Access log

Every request produces one structured log line on stderr (slog
`INFO`): `method`, `path`, `status`, `bytes`, `duration_ms`,
`remote`, `request_id`. The same `request_id` appears in every error
envelope body so a 500 can be correlated with its log line.

### Audit log

Every publish, yank, delete, token create, token revoke, and import
writes a row to the `events` table. Read it via
`GET /api/v1/events` (admin-scope) or the
[operator dashboard](../internal/ui) at `/ui/events`.

## Upgrade procedure

Packyard ships a single binary; upgrade is SIGTERM the old one, swap
the binary, start the new one. Migrations run on start if needed.
Always back up `<data-dir>/` before a non-patch upgrade (the phased
rollout of schema changes in implementation.md §A2 means this should
be low-risk, but a snapshot is cheap).
