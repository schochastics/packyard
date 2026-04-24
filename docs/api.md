# HTTP API reference

Authoritative machine-readable spec: `GET /api/v1/openapi.json` (or
`.yaml`) on any running packyard. The source document is
[openapi/openapi.yaml](../openapi/openapi.yaml). This page is the
curl-level companion — the spec covers schemas in full, but clients
and humans want copy-pasteable requests. The two stay in sync via
contract tests in CI.

All paths under `/api/v1/` are versioned. Breaking changes cut a new
prefix; additive changes happen in-place.

## Conventions

- **Base URL.** `http://$PACKYARD_SERVER` — everything documented here is
  relative to that.
- **Auth.** `Authorization: Bearer <token>` on every non-public endpoint.
  `/health`, `/metrics`, and the OpenAPI endpoints are unauthenticated.
  CRAN-protocol reads are anonymous by default — see
  [config.md](config.md) for the `allow_anonymous_reads` knob.
- **Content type.** JSON everywhere except publish (multipart) and
  CRAN-protocol reads (DCF text or `application/gzip`).
- **Timestamps.** ISO-8601 with millisecond precision and `Z` suffix
  (`2026-04-22T15:04:05.123Z`).
- **Pagination.** `limit` + `offset` for snapshot-style lists (channels,
  packages, cells); `since_id` cursor for the events log. Both surface
  an `X-Total-Count` header.
- **IDs.** `request_id` on every error envelope, also logged to stderr;
  quote it when filing issues.

## Error envelope

Every non-2xx response is one flat JSON object:

```json
{
  "error_code": "version_immutable",
  "message": "mypkg@1.0.0 already exists on immutable channel prod with different content",
  "hint": "bump the version, or republish with byte-identical content",
  "request_id": "019db1bf-6f40-7bea-91cf-785cbdb9606b"
}
```

`error_code` is the stable machine-readable identifier. `message` is a
human string (may change between versions). `hint` is optional. A 204
or 404 without a body is never returned from an API endpoint — errors
always carry this envelope.

### error_code catalog

| Code | Typical status | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed input: missing field, invalid package/version shape, bad multipart. |
| `unauthorized` | 401 | No bearer token, or the token isn't valid. |
| `insufficient_scope` | 403 | Token is valid but doesn't include the required scope (e.g. `publish:prod`). |
| `not_found` | 404 | Channel, package, version, cell, or token not present. |
| `conflict` | 409 | General conflict not covered by a more specific code. |
| `version_immutable` | 409 | Re-publishing the same `(channel, name, version)` on an immutable channel with different bytes. |
| `channel_immutable` | 409 | Delete attempt on an immutable channel; bump the version instead. |
| `payload_too_large` | 413 | Request body exceeded 2 GiB or the manifest part exceeded 1 MiB. |
| `internal_error` | 500 | Server-side failure; see logs + `request_id`. |
| `unavailable` | 503 | One or more subsystems failed their health probe; see `/health`. |

## Endpoints

### Health and metrics

#### `GET /health`

Public. Returns 200 when every subsystem passes its probe, 503 when one
or more fail. Body is always JSON:

```sh
curl -s http://localhost:8080/health | jq
```

```json
{
  "status": "ok",
  "version": "v0.3.1",
  "subsystems": {
    "db": "ok",
    "cas": "ok",
    "matrix": "ok"
  }
}
```

#### `GET /metrics`

Public Prometheus text format. See
[admin.md](admin.md#observability) for the metric names and labels.

### OpenAPI spec

- `GET /api/v1/openapi.json` — the spec rendered as JSON.
- `GET /api/v1/openapi.yaml` — the canonical YAML source.

Both are unauthenticated so SDK generators can pull without a token.

### Admin tokens

Admin-scope required on all three. Plaintext tokens are returned **once**
on create and never again; packyard stores sha256(token) only.

#### `POST /api/v1/admin/tokens`

```sh
curl -X POST http://localhost:8080/api/v1/admin/tokens \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"label":"ci","scopes":["publish:*","read:*"]}'
```

Response:

```json
{
  "id": 7,
  "label": "ci",
  "scopes": ["publish:*","read:*"],
  "token": "pkm_9f3a…",
  "created_at": "2026-04-22T15:04:05.123Z"
}
```

#### `GET /api/v1/admin/tokens`

Lists tokens (no plaintext, no sha). `last_used_at` is updated on every
authenticated request that resolves to this token.

#### `DELETE /api/v1/admin/tokens/{id}`

Revokes the token. Existing `/ui/` sessions using the revoked token
become anonymous on the very next request (packyard does not cache
identity — every request hits the tokens table).

### JSON read surface

All admin-gated in v1. Loosening to scoped reads is a Phase C follow-up.

#### `GET /api/v1/channels`

```sh
curl -s -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/api/v1/channels | jq
```

Returns `{"channels":[{name, overwrite_policy, default, created_at, package_count, latest_publish_at}, …]}`.

#### `GET /api/v1/packages`

Filters: `channel`, `package`; pagination: `limit` (default 100, max
500), `offset`. Header `X-Total-Count` carries the unfiltered match
count.

```sh
curl -s -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/api/v1/packages?channel=prod&limit=10"
```

Each package carries an inline `binaries` array — this is the one
documented exception to packyard's otherwise-flat response shape.

#### `GET /api/v1/cells`

Dumps `matrix.yaml` as JSON. Same admin-only scope caveat applies.

#### `GET /api/v1/events`

Audit log with `since_id` cursor — returns events whose `id > since_id`
in ascending order. Filters: `channel`, `package`, `type`. `limit`
defaults to 100, caps at 500. `X-Total-Count` reflects the full
filtered set regardless of cursor.

```sh
# First page
curl -s -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/api/v1/events?limit=100" | jq '.events | last'
# Subsequent polls: remember max id seen, pass as since_id
curl -s -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/api/v1/events?since_id=742&limit=100"
```

### Publish

#### `POST /api/v1/packages/{channel}/{name}/{version}`

Multipart/form-data. Required parts:

- `manifest` — JSON describing the upload (see below).
- Exactly one source tarball part named by `manifest.source`
  (conventionally `"source"`).
- Zero or more binary parts named by `manifest.binaries[].part`.

Manifest schema (strict — unknown fields are rejected):

```json
{
  "source": "source",
  "description_version": "1.0.0",
  "binaries": [
    {"cell": "ubuntu-24.04-amd64-r-4.4", "part": "bin_r_44"},
    {"cell": "ubuntu-24.04-amd64-r-4.5", "part": "bin_r_45"}
  ]
}
```

`description_version` is optional; when set it must equal the URL
version (a cross-check against stale CI artifacts).

Curl:

```sh
curl --fail-with-body -X POST \
  http://localhost:8080/api/v1/packages/prod/mypkg/1.0.0 \
  -H "Authorization: Bearer $PUB" \
  -F 'manifest={"source":"source"};type=application/json' \
  -F "source=@mypkg_1.0.0.tar.gz;type=application/gzip"
```

Required scope: `publish:<channel>`. Response is JSON with
`source_sha256`, `source_size`, a `binaries` array, and `created` /
`overwritten` / `already_existed` flags so CI can tell at a glance what
the server did.

Behavior by channel policy:

- **mutable** — always replaces, reports `overwritten: true`.
- **immutable, same bytes** — 200, `already_existed: true` (idempotent).
- **immutable, different bytes** — 409 `version_immutable`.

Size caps: 2 GiB total request body; 1 MiB manifest part.

### Yank

#### `POST /api/v1/packages/{channel}/{name}/{version}/yank`

```sh
curl --fail-with-body -X POST \
  http://localhost:8080/api/v1/packages/prod/mypkg/1.0.0/yank \
  -H "Authorization: Bearer $YANK" \
  -H "Content-Type: application/json" \
  -d '{"reason":"security: CVE-xxxx-yyyy"}'
```

Yank is reversible (a future endpoint will unyank); bytes stay in CAS.
Required scope: `yank:<channel>`.

Yanked packages still appear in `PACKAGES` but with a `Yanked: true`
field so R clients can warn. `install.packages()` against a yanked
version currently still installs — R has no native yank semantics.

### Delete

#### `DELETE /api/v1/packages/{channel}/{name}/{version}`

Hard delete: removes the row, removes binaries rows, orphans the CAS
blobs for later `admin gc`.

```sh
curl --fail-with-body -X DELETE \
  http://localhost:8080/api/v1/packages/dev/mypkg/1.0.0 \
  -H "Authorization: Bearer $ADMIN"
```

Required scope: `admin`. On immutable channels this returns 409
`channel_immutable` — bump the version and yank the old one instead.

### CRAN-protocol reads

These endpoints exist to make packyard indistinguishable from a CRAN
repo to standard R tooling. No bearer token required unless
`allow_anonymous_reads` is false on the channel.

#### Source

- `GET /{channel}/src/contrib/PACKAGES` — DCF index.
- `GET /{channel}/src/contrib/PACKAGES.gz` — gzipped DCF.
- `GET /{channel}/src/contrib/<file>.tar.gz` — source tarball stream.

#### Binary (Linux-cell-specific)

- `GET /{channel}/bin/linux/{cell}/PACKAGES`
- `GET /{channel}/bin/linux/{cell}/PACKAGES.gz`
- `GET /{channel}/bin/linux/{cell}/<file>.tar.gz`

#### Default-channel alias

Everything above is also mounted at the root so
`options(repos="http://packyard.corp/")` works unmodified:

- `GET /src/contrib/PACKAGES` → default channel's source index.
- `GET /bin/linux/{cell}/PACKAGES` → default channel's binary index.

R configuration:

```r
# Point only at the default channel (simplest):
options(repos = c(PACKYARD = "http://packyard.corp/", getOption("repos")))

# Explicit channel:
options(repos = c(PACKYARD_DEV = "http://packyard.corp/dev/", getOption("repos")))
```

## Scopes

Tokens carry a CSV of scopes. Format: `<verb>:<channel>` or a bare
`admin`. `*` wildcards the channel segment.

| Scope | Grants |
|---|---|
| `admin` | Everything, including minting and revoking tokens, and deleting packages. |
| `publish:<channel>` | Publish to that channel. `publish:*` for any. |
| `yank:<channel>` | Yank in that channel. `yank:*` for any. |
| `read:<channel>` | Bearer-auth read of admin JSON endpoints filtered to that channel (v1 treats all JSON reads as admin; reserved for future loosening). |

CRAN-protocol reads don't consult scopes — they consult the channel's
`allow_anonymous_reads` flag.

## Idempotency and retries

- **Publish** is safe to retry on immutable channels: identical bytes
  return `already_existed: true` without creating duplicate events. On
  mutable channels, retries overwrite and log a `publish_overwrite`
  event each time.
- **Yank** is safe to re-run; the `yanked` flag just stays set.
- **Delete** is a hard op; don't retry blindly after a timeout without
  checking state.
- **Admin token create** is not idempotent — each call mints a new
  row. Debounce at the client.

## Rate limits

None in v1. Publish caps body size (2 GiB) but does not throttle
request rate. Running behind a reverse proxy that rate-limits is the
recommended pattern if abuse is a concern.
