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

The data dir is picked in this order:

1. `-data <dir>` CLI flag (default `./data`).
2. `data_dir` in `server.yaml` if `-config` is set.
3. `WORKDIR /data` in the official Docker image, with `VOLUME /data`.

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

# Default channel's CRAN-protocol reads are public when true. Every
# non-CRAN endpoint still requires a token. The CLI flag
# -allow-anonymous-reads overrides this — see admin.md.
allow_anonymous_reads: false
```

### Validation

- `listen` must be non-empty.
- `data_dir` must be non-empty.
- `tls_cert` and `tls_key` must either both be set or both be empty.
- Relative paths in the YAML are resolved against the YAML file's
  directory (not packyard's CWD) so a config placed in `/etc/packyard/`
  behaves predictably regardless of how the server is launched.

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
  get source until binaries are backfilled. `packyard-server admin
  cells show r-4.7` prints the gap list.
- Subsequent CI runs that include the cell fill the gap over time.

**Change the distro** (e.g. a client moves from `jammy` to `noble`):
change `distro`, restart, and rebuild binaries for every cell. Clients
must switch their repo URLs to the new codename at the same time —
requests for any other distro return 404.

### Relationship to the CI workflow

Every `cell.name` that a client publishes a binary for must exist in
`matrix.yaml`. Cells referenced from a publish manifest but missing
from matrix.yaml fail publish with 400 `bad_request`. The reference CI
template at [examples/ci/publish.yml](../examples/ci/publish.yml)
matrix block is what operators adapt; the server's matrix is the
authoritative list.

## Environment variables

Packyard reads no environment variables for runtime config in v1 —
everything comes from YAML + CLI flags. The Docker image sets
`WORKDIR /data` and `VOLUME /data`; that's it.

## Changing config

Every config file is re-read at server start. There is no hot-reload
in v1 — SIGHUP is ignored. The reconcile logic is deliberately
simple: channels added, same channels are updated in place (policy or
default-flag changes apply), channels removed from YAML are NOT
removed from the DB. Start with a known-good YAML, restart once,
check `packyard-server admin channels list` before shutting the old
server down.
