# Configuration reference

Packyard reads three YAML files: `server.yaml` (optional), `channels.yaml`,
and `matrix.yaml`. The latter two are bootstrapped on first start with
sensible defaults and live under the data dir; `server.yaml` is entirely
optional and only needed once you outgrow the command-line flags.

## Layout

```
<data-dir>/
  db.sqlite             # SQLite (WAL mode)
  cas/                  # content-addressable blob store, <aa>/<rest> shards
  channels.yaml         # channel definitions
  matrix.yaml           # binary matrix (cells)
  ui-session-key        # 32-byte HMAC key (auto-generated, 0600)
```

The data dir is:

1. `data_dir` from `server.yaml` when `-config` is set (`-data` is
   then ignored);
2. otherwise the `-data <dir>` flag (default `./data`, relative to the
   working directory). The image's working directory is `/`, so in
   the container the default is the `/data` volume.

`channels_file` / `matrix_file` in `server.yaml` move those two files
out of the data dir, e.g. onto a read-only config mount.

## `server.yaml`

Optional. Point at it with `packyard-server -config /etc/packyard/server.yaml`.
Every field is optional; omitted keys fall back to defaults. Strict
parsing: **unknown keys fail** so a typo can't silently lose a setting.

```yaml
listen: ":8080"               # host:port, default ":8080"
data_dir: "/var/lib/packyard"   # default "./data"

# Paths resolve relative to the directory of this YAML file if relative.
channels_file: "channels.yaml"    # default <data_dir>/channels.yaml
matrix_file:   "matrix.yaml"      # default <data_dir>/matrix.yaml

# TLS is all-or-nothing; setting only one of these fails validation.
tls_cert: ""
tls_key:  ""

# Behind a reverse proxy (see below).
public_url: "https://packages.example.org"
trusted_proxies: ["10.0.0.0/8"]

# Serve /metrics on its own listener instead of the main one.
metrics_listen: "127.0.0.1:9090"

# API tokens provisioned from secret files (see below).
tokens:
  - label: ci-publish-prod
    scopes: publish:prod,yank:prod
    token_file: /run/secrets/packyard/ci-publish-prod
  - label: workbench-read
    scopes: read:*
    sha256_file: /run/secrets/packyard/workbench-read.sha256
```

Anonymous CRAN-protocol reads are set per channel in `channels.yaml`
(`anonymous_reads`), not here.

### Running behind a reverse proxy

- **`public_url`** is the external base URL: a scheme and host, no
  path. Packyard needs a dedicated hostname because the UI templates
  hard-code `/ui/…`, so subpath deployments aren't supported. An
  `https://` value marks UI cookies `Secure` even when TLS terminates
  at the proxy. The UI also builds its copy-paste `repos =` snippets
  from it; without it, they use the request's `Host`.
- **`trusted_proxies`** lists CIDRs or bare IPs whose
  `X-Forwarded-For` (or, failing that, `X-Real-IP`) is believed. The
  access log then records the client address instead of the proxy's.
  The chain is read right to left, skipping trusted hops, so a client
  can't spoof its address by sending its own header. Requests from
  any other peer are logged with their socket address.

### Separate metrics listener

With `metrics_listen` set, `/metrics` is served only on that address
and removed from the main listener. Use it to keep the scrape
endpoint on a container-internal network while the main port is
published. Without it, `/metrics` is on the main listener, as before.

### Tokens from config

`tokens:` declares API tokens whose secrets come from files, typically
secrets mounted by the orchestrator. Each entry needs a `label`,
`scopes` (comma-separated, the same scopes `-mint-token` takes) and
exactly one of:

- **`token_file`** holds the plaintext token (`pkm_…`). Surrounding
  whitespace is ignored.
- **`sha256_file`** holds the hex sha256 of the token, so the
  plaintext never reaches the server host.

`packyard-server admin token-gen` prints a fresh token on line 1 and
its sha256 on line 2. Store the token in CI and the hash on the server.

On every start the server reconciles config tokens in one
transaction:

- a new entry is inserted;
- an entry whose label or scopes changed is updated;
- a token no longer listed is revoked. This includes the old secret
  of a rotated entry.

Tokens minted with `-mint-token` or the API are never touched. A
missing or malformed secret file fails startup. Config tokens show
`"source": "config"` in `GET /api/v1/admin/tokens`, and the API
refuses to revoke them (409): remove them from the config instead.

### Validation

- `listen` must be non-empty.
- `data_dir` must be non-empty.
- `tls_cert` and `tls_key` must either both be set or both be empty.
- `public_url` must be an absolute `http(s)` URL without a path.
- `trusted_proxies` entries must be IPs or CIDRs.
- `metrics_listen` must be `host:port` and differ from `listen`.
- `tokens`: labels are required and unique; `scopes` must be
  non-empty; set exactly one of `token_file` or `sha256_file`.
- Relative paths in the YAML, token files included, are resolved
  against the YAML file's directory, not packyard's CWD. A config
  placed in `/etc/packyard/` behaves the same however the server is
  launched.

## `channels.yaml`

Defines the channels packyard serves. A channel is the top-level grouping
for versioned packages; think "environment" (`dev`, `test`, `prod`) or
"scope" (`internal`, `contrib`).

Default shipped by `packyard-server -init`:

```yaml
channels:
  - name: dev
    overwrite_policy: mutable
    default: false

  - name: test
    overwrite_policy: mutable
    default: false

  - name: prod
    overwrite_policy: immutable
    default: true
```

### Field reference

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | `[a-z0-9]([a-z0-9-]*[a-z0-9])?`, max 63 chars. Appears in URLs and tokens, so keep it short. |
| `overwrite_policy` | yes | `mutable` or `immutable` (see below). |
| `default` | no | Bool. Exactly one channel must be `true` — that's the channel served at the `/src/contrib/…` alias. |
| `kind` | no | `local` (default) or `proxy`. See [proxy.md](proxy.md) (frozen feature). |
| `upstream` | proxy only | Upstream settings for `kind: proxy`; see [proxy.md](proxy.md#knobs). |
| `anonymous_reads` | no | Bool, default `false`. When `true`, the channel's CRAN-protocol reads (`PACKAGES`, tarballs, `Archive/`, `Meta/archive.rds`) need no token. Everything else, including the JSON API, still does. Not allowed on proxy channels. |

### Overwrite policy

- **`mutable`** — publishing the same `(channel, name, version)` replaces
  the stored content. Previous blobs orphan and are reclaimed by
  `admin gc`. Right default for `dev` where engineers iterate on a
  feature branch without bumping the version string every push.
- **`immutable`** — re-publishing fails with 409 `version_immutable`
  unless the bytes are byte-identical (in which case the response is
  200 `already_existed: true`, idempotent). Right default for `prod`
  so downstream consumers never see a version string that means two
  different things at two different times.

`DELETE` is rejected with 409 `channel_immutable` on immutable
channels; bump + yank instead.

### Adding, renaming, removing

Channels reconcile on every server start. Adding a channel is a pure
addition — restart and it shows up. Renaming or removing a channel is
**never** auto-destructive: the old DB row stays in place and packyard
logs a warning at startup. This keeps operator mistakes recoverable.
Clean up with `packyard-server admin channels list` and a manual DB
edit if you're sure.

### Common patterns

**dev / test / prod (default shipped):**

```yaml
channels:
  - { name: dev,  overwrite_policy: mutable,   default: false }
  - { name: test, overwrite_policy: mutable,   default: false }
  - { name: prod, overwrite_policy: immutable, default: true  }
```

**Single internal channel:**

```yaml
channels:
  - { name: internal, overwrite_policy: immutable, default: true }
```

**Per-team channels:**

```yaml
channels:
  - { name: prod,              overwrite_policy: immutable, default: true  }
  - { name: team-analytics,    overwrite_policy: mutable,   default: false }
  - { name: team-epi,          overwrite_policy: mutable,   default: false }
  - { name: team-ml,           overwrite_policy: mutable,   default: false }
```

Scope tokens per team: `publish:team-analytics`, `publish:team-epi`, …

## `matrix.yaml`

Declares which binaries publishers may upload. A deployment serves
**exactly one Linux distribution**; cells vary only by R minor
version. List every R minor your Workbench/Connect images install —
clients on an R version without a cell fall back to source packages.

Default shipped by `packyard-server -init` (`-init -distro rhel9`
writes `distro: rhel9` instead):

```yaml
distro: jammy
arch: amd64
default_r_minor: "4.5"
cells:
  - { name: r-4.4, r_minor: "4.4" }
  - { name: r-4.5, r_minor: "4.5" }
  - { name: r-4.6, r_minor: "4.6" }
```

### Field reference

| Field | Required | Valid values |
|---|---|---|
| `distro` | yes | Posit Package Manager codename, `[a-z0-9]+`: `jammy`, `noble`, `rhel9`, …. Appears verbatim in client URLs (`/{channel}/__linux__/{distro}/latest`). |
| `arch` | yes | `amd64`, `arm64`. |
| `default_r_minor` | yes | The `r_minor` of one of the cells. Used for clients whose User-Agent carries no R version. |
| `build_image_hint` | no | Free text for CI; the server ignores it. |
| `cells[].name` | yes | `[a-z0-9]([a-z0-9.-]*[a-z0-9])?`, max 127 chars, unique. Referenced from publish manifests. **Renames require re-publishing.** |
| `cells[].r_minor` | yes | `MAJOR.MINOR` (`"4.4"`, not `"4.4.1"` — R binaries are minor-version-pinned), unique. Quote it to keep YAML from parsing a float. |

### Common patterns

**Add a new R minor** (R 4.7 alongside the existing ones):

```yaml
cells:
  - { name: r-4.6, r_minor: "4.6" }
  - { name: r-4.7, r_minor: "4.7" }
```

After restarting:

- Existing packages have no binary for the new cell; clients on R 4.7
  get source until binaries are backfilled.
- `packyard-server admin missing-binaries -channel prod -cell r-4.7`
  (or `GET /api/v1/channels/prod/missing-binaries?cell=r-4.7`) lists
  the gap, and
  [examples/ci/packyard-backfill.sh](../examples/ci/packyard-backfill.sh)
  builds and attaches those binaries. Future publishes include the
  cell automatically, because the CI scripts read the cell list from
  the server.

**Change the distro** (e.g. a client moves from `jammy` to `noble`):
change `distro`, restart, and rebuild binaries for every cell. Clients
must switch their repo URLs to the new codename at the same time —
requests for any other distro return 404.

### Relationship to the CI workflow

`matrix.yaml` is the authoritative list of what CI builds. The
reference scripts in [examples/ci/](../examples/ci/) read it from
`GET /api/v1/cells` and build one binary per cell, with the matching
R under `/opt/R/<version>`. A binary for a cell missing from
`matrix.yaml` is rejected with 400 `bad_request`. A publish may omit
cells; the response lists them in `missing_cells`.

## Environment variables

Packyard reads no environment variables for runtime config;
everything comes from YAML + CLI flags. The Docker image sets
`WORKDIR /data` and `VOLUME /data`; that's it.

## Changing config

Every config file, including token secret files, is re-read at server
start. There is no hot-reload; SIGHUP is ignored. The reconcile logic is deliberately
simple: channels added, same channels are updated in place (policy or
default-flag changes apply), channels removed from YAML are NOT
removed from the DB. Start with a known-good YAML, restart once,
check `packyard-server admin channels list` before shutting the old
server down.
