# Lazy proxy channels

Packyard supports two kinds of channel:

- **local** (the default) — packages get into the channel via CI
  `publish`, the bundle importer, or the drat/git importers.
- **proxy** — packages get into the channel by being asked for. On
  the first read, packyard fetches the tarball from an upstream
  CRAN-protocol server (CRAN cloud, Posit Package Manager, r-universe),
  writes it into the local CAS, and serves it. Subsequent reads hit
  the local cache; tarballs are content-addressed and never re-fetched.

This is Verdaccio's "uplinks" model adapted to the CRAN protocol. The
operator playbook is below; the design rationale lives in
[design.md §15](../design.md).

## When to use a proxy channel

- Your team should have a **single URL** in `options(repos =)` for
  both internal and public packages. With a proxy channel, packyard
  becomes the team's one point of contact with the R ecosystem;
  firewall rules and audit logs only need to know about packyard's
  domain.
- You want **CRAN-outage resilience**. A warm cache keeps installs
  working when `cran.r-project.org` is slow or down.
- You want a free, lazy snapshot of every public dependency the team
  has actually touched — handy for backups and for spotting
  "what does this codebase depend on" without grepping every package.

For **offline / air-gap** environments where the server can't reach
upstream at all, use the bundle importer instead — see
[airgap.md](airgap.md). The two paths compose: a server can have
some channels backed by lazy proxy and others by imported bundles.

## Minimal config

```yaml
# channels.yaml
channels:
  - name: prod
    overwrite_policy: immutable
    default: true

  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
```

Restart the server, then point R at packyard:

```r
options(repos = c(
  PACKYARD = "https://packyard.corp/",        # default channel (prod)
  CRAN     = "https://packyard.corp/cran/"    # proxy channel
))
install.packages("ggplot2")
```

The first install hits CRAN through packyard; the second time anyone
on the team runs the same install, the tarball comes straight from
packyard's local CAS.

## Pinning a snapshot (PPM-style)

Packyard doesn't have a separate `pin` field — operators encode the
snapshot in the upstream URL. Posit Package Manager serves dated
URLs for CRAN:

```yaml
- name: cran-2026-q1
  overwrite_policy: immutable
  kind: proxy
  upstream:
    source_url: https://packagemanager.posit.co/cran/2026-01-15
```

Every `install.packages()` against this channel resolves to the
CRAN state as of 2026-01-15. To roll the snapshot forward, change
the date in `channels.yaml` and restart — there is no migration
needed because packyard refuses kind changes on populated channels
but allows freely updating the upstream URL on an existing proxy
channel.

## Per-cell binaries

Source tarballs work out of the box. To proxy precompiled Linux
binaries (PPM-style), add a `binary_urls` map keyed by cell:

```yaml
- name: cran
  overwrite_policy: immutable
  kind: proxy
  upstream:
    source_url: https://packagemanager.posit.co/cran/latest
    binary_urls:
      ubuntu-22.04-amd64-r-4.4: https://packagemanager.posit.co/cran/__linux__/jammy/latest
      ubuntu-24.04-amd64-r-4.5: https://packagemanager.posit.co/cran/__linux__/noble/latest
```

The cell name on the left **must** appear in `matrix.yaml`. On
startup packyard validates the cross-config and refuses to start if
a `binary_urls` cell isn't declared. Cells missing from the map fall
through to source — clients on those cells compile locally.

For r-universe, point at the org's binary tree:

```yaml
binary_urls:
  ubuntu-22.04-amd64-r-4.4: https://my-org.r-universe.dev/bin/linux/jammy/4.4
```

## Knobs

The full `upstream:` block:

```yaml
upstream:
  source_url: https://cloud.r-project.org    # required, https://
  binary_urls: {}                            # optional, per cell
  index_ttl: 5m                              # PACKAGES cache TTL (default 5m)
  timeout: 5m                                # per-fetch HTTP timeout (default 5m)
  tarball_max_size: 524288000                # cap per tarball (default 500 MiB)
```

- `index_ttl` controls how often packyard re-fetches the upstream
  `PACKAGES` file. Shorter = fresher view of upstream new releases;
  longer = fewer upstream round-trips. 5 minutes is a reasonable
  middle ground.
- `timeout` is the deadline for a single upstream HTTP request.
  Tarballs are streamed straight to CAS; the timeout covers the
  whole transfer.
- `tarball_max_size` is a safety cap so a hostile upstream can't
  fill the disk with one giant blob.

## Security and operational notes

- **Forbidden combinations.** `kind: proxy` + `default: true` is
  rejected at startup — that combination would expose an open
  cache-fill relay if anonymous reads are also enabled. Use a named
  proxy channel and reference it explicitly from `repos =`.
- **Scope.** Proxy fetches happen as a side effect of read. The
  caller needs `read:<channel>` (or anonymous reads, for non-default
  proxy channels with `allow_anonymous_reads: true`). No separate
  `proxy:<channel>` scope today.
- **Writes are rejected.** Publish, yank, delete, and bundle import
  on a proxy channel return `409 channel_is_proxy`. Proxy channels
  mirror upstream; to override a public package, create a local
  channel for it.
- **Stale-while-error.** If the upstream `PACKAGES` fetch fails and
  packyard has a cached body (from any prior fetch, even expired),
  it serves the stale body and writes a `proxy_index_stale_served`
  event row. Tarball misses on upstream-down return `502/504` —
  there's no stale fallback for "this exact tarball" because the
  CAS is the cache.
- **GC.** Proxied content lives in the same CAS as locally-published
  content. `packyard admin gc` runs unchanged. There is no
  "expire cache" command — once a tarball is cached, it stays.
- **Events.** `proxy_tarball_fetch`, `proxy_index_stale_served`, and
  the per-package `import_binary` events from `AttachBinary` all
  show up in `/ui/events` for audit.
- **Metrics.** `packyard_proxy_fetch_total{channel, kind, result}`
  counters track upstream activity. See `/metrics` for the live shape.

## Composing with the air-gap bundle path

A single server can mix the two:

```yaml
channels:
  - name: prod
    overwrite_policy: immutable
    default: true                # internal packages, CI publishes

  - name: cran                   # online lazy proxy
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org

  - name: cran-2026-q1           # offline imported snapshot
    overwrite_policy: immutable  # populated via `admin import bundle`
```

The proxy channel handles "we have internet, just don't want every
developer hitting CRAN directly." The bundle channel handles "we
have no internet at all" or "we need a frozen snapshot for audit."
Picking which channel each project uses is a `repos =` decision in
the project's R config.
