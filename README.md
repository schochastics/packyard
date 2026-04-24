# packyard

[![CI](https://github.com/schochastics/packyard/actions/workflows/ci.yml/badge.svg)](https://github.com/schochastics/packyard/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/schochastics/packyard?include_prereleases&sort=semver)](https://github.com/schochastics/packyard/releases)

**An open-source, single-binary R package registry for *internal* R
packages** — think "private CRAN for your organisation". Users point
their R installations at both packyard (for internal packages) and
upstream [CRAN](https://cran.r-project.org/) / [Posit Package
Manager](https://packagemanager.posit.co/) / [r-universe](https://r-universe.dev/)
(for public packages), as two entries in `repos`. Packyard serves a
CRAN-protocol-compatible read surface, so existing R tooling
(`install.packages`, [renv](https://rstudio.github.io/renv/),
[pak](https://pak.r-lib.org/)) works out of the box.

## Quickstart

```sh
docker run --rm -d --name packyard -p 8080:8080 -v packyard-data:/data \
  ghcr.io/schochastics/packyard:latest
```

Full walkthrough (Docker → admin token → publish → install from R):
**[docs/quickstart.md](docs/quickstart.md)** — takes about five
minutes including the Docker pull.

## What it is

- **One Go binary.** No Postgres, no Redis, no object store.
- **SQLite + local filesystem** by default. `scp` the binary and run.
- **CRAN-protocol read endpoints** so any R client works unmodified.
- **Channels** (`dev`, `test`, `prod`, or any names you configure) with
  per-channel overwrite policy and scoped tokens.
- **Publish via curl.** The
  [reference CI workflow](examples/ci/publish.yml) builds per-cell
  binaries and POSTs them multipart — no maintained action needed.
- **OpenAPI 3 spec shipped** at `/api/v1/openapi.json`.
- **Operator dashboard** at `/ui/` with channel cards, events, cells
  coverage, and storage stats.

## Who it's for

- **R consultancies** (5–30 staff, multi-client) sharing toolkit
  packages across client engagements.
- **Mid-size R shops** (20–100 devs) sharing packages across teams,
  not ready to buy PPM.
- **Regulated orgs** (pharma, finance, defense) — natural target,
  blocked on the air-gap roadmap (v1.x).

Not targeted: enterprises with ≥100 R devs and an existing Posit
relationship (PPM is the right answer), individual users (use
[drat](https://github.com/eddelbuettel/drat) or
[r-universe](https://r-universe.dev/)), teams whose only need is
`install_github` from a private repo.

## What it is NOT

- **Not a [Posit Package Manager](https://docs.posit.co/rspm/)
  replacement.** PPM is a strong commercial product with a Linux binary
  farm, dated snapshots, `SystemRequirements` translation, and
  enterprise features packyard does not try to match. Packyard fills the
  slot PPM doesn't: open-source, lightweight, self-hosted for internal
  packages.
- **Not a client CLI.** Existing R tools drive packyard.
  [uvr](https://github.com/nbafrank/uvr) and
  [rv](https://github.com/A2-ai/rv) occupy the uv-shaped client space;
  packyard aims to be a well-documented publish destination for them.
- **Not a CRAN mirror in v1.** Configure R with packyard **alongside**
  upstream CRAN; air-gap deploy with a bundled CRAN mirror is the
  committed v1.x roadmap (see below).
- **Not server-side building in v1.** CI builds per cell and uploads;
  packyard indexes and serves. The cell-identity machinery in v1 already
  supports server-side builds later without a schema change.

## Install

### Docker (recommended)

```sh
docker run --rm -d --name packyard -p 8080:8080 -v packyard-data:/data \
  ghcr.io/schochastics/packyard:latest
```

Images are published for each tagged release. The container initialises
on first start (creates DB, CAS, default configs) and runs as a
non-root user.

### Binary

Download the matching tarball from
[GitHub releases](https://github.com/schochastics/packyard/releases), or:

```sh
go install github.com/schochastics/packyard/cmd/packyard-server@latest
packyard-server -init -data ./data
packyard-server -data ./data
```

The binary is a single static executable; no C runtime required.

## Documentation

| Doc | Covers |
|---|---|
| [quickstart.md](docs/quickstart.md) | Five-minute zero-to-publish walkthrough. |
| [api.md](docs/api.md) | HTTP API reference, curl examples, error codes. |
| [config.md](docs/config.md) | `channels.yaml`, `matrix.yaml`, server config. |
| [admin.md](docs/admin.md) | `packyard-server admin …` commands. |
| [backup-restore.md](docs/backup-restore.md) | Snapshots, rsync cadence, and restore verification. |
| [migration.md](docs/migration.md) | Moving from drat or git to packyard. |
| [design.md](design.md) | Architecture and scope. |
| [implementation.md](implementation.md) | Phased build plan and status. |
| [examples/ci/README.md](examples/ci/README.md) | Reference CI workflow. |
| [examples/compose/README.md](examples/compose/README.md) | `docker compose up` template and production hardening. |

The OpenAPI spec is also served at `/api/v1/openapi.json` (and
`.yaml`) from any running packyard.

## Roadmap

### v1 (shipping)

- Publish + yank + delete over HTTP.
- CRAN-protocol source + binary reads.
- Channels with mutable/immutable policy.
- Operator dashboard.
- Admin CLI: import (drat, git), channels/cells list, gc, reindex.
- Prometheus metrics + structured access logs.
- Reference CI workflow for GitHub Actions + Gitea Actions.

### v1.x

- **Air-gap deploy with a bundled CRAN / Bioconductor mirror.** The
  defining post-v1 feature and the gating requirement for regulated-org
  targets. Sync-bundle format, signing, snapshot cadence, and
  "updating CRAN on prod" workflow are a whole design project.
- **Server-side binary build farm.** v1 receives pre-built artifacts
  from CI; v1.x adds the option to build server-side for teams that
  don't want a CI pipeline per package. Cell schema is already ready.
- **Sigstore signing / provenance attestations.** Parked until the
  basic publish flow proves itself in production.

### Explicitly not planned

- First-party Rust client CLI. uvr/rv/existing R tools are the client
  story.
- Breadth-of-distro build farm matching PPM.
- Managed SaaS.
- Public-mirror-scale throughput.

## Contributing

Packyard follows a plan-driven workflow: every feature lands as a
numbered task in [implementation.md](implementation.md) so the scope
and ordering are visible. Before opening a PR, please skim the design
and implementation docs for context. Issues and discussions are open
on GitHub.

## License

MIT. See [LICENSE](LICENSE).
