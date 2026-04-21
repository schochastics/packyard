# pakman

An open-source, single-binary R package registry for **internal** R packages — think "private CRAN for your organisation".

Users point their R installations at both pakman (for internal packages) and upstream [CRAN](https://cran.r-project.org/) / [Posit Package Manager](https://packagemanager.posit.co/) / [r-universe](https://r-universe.dev/) (for public packages). Pakman serves a CRAN-protocol-compatible read surface, so existing R tooling ([`install.packages`](https://stat.ethz.ch/R-manual/R-devel/library/utils/html/install.packages.html), [renv](https://rstudio.github.io/renv/), [pak](https://pak.r-lib.org/)) works out of the box.

> **Status:** pre-v1 under active development. The design is stable ([design.md](design.md)) and the implementation is tracked in [implementation.md](implementation.md).

## What it is

- A single Go binary — no Postgres, no Redis, no object store required.
- SQLite + local filesystem by default.
- CRAN-protocol-compatible endpoints so any R client works.
- Channels (`dev`, `test`, `prod`, or any names you configure) with per-channel overwrite policy.
- A curl-friendly HTTP API for publishing — CI pushes built tarballs, pakman indexes and serves them.
- OpenAPI 3 spec shipped at `/api/v1/openapi.json`.

## What it is NOT (in v1)

- Not a [Posit Package Manager](https://docs.posit.co/rspm/) replacement. PPM is a strong commercial product; pakman fills the slot PPM doesn't — open-source, lightweight, self-hosted for internal packages.
- Not a client CLI. Existing R tools drive pakman. [uvr](https://github.com/nbafrank/uvr) and [rv](https://github.com/A2-ai/rv) occupy the uv-shaped client space; pakman aims to be a well-documented publish destination for them.
- Not a CRAN mirror in v1. Users configure R with pakman + upstream CRAN side-by-side. Air-gap deploy with a bundled CRAN mirror is the committed **v1.x roadmap**.
- Not server-side building in v1. CI builds per cell and uploads; pakman indexes and serves.

## Who it's for

- **R consultancies** (5–30 staff, multi-client) sharing internal toolkit packages across engagements.
- **Mid-size R shops** (20–100 devs) sharing internal packages across teams, not ready to buy PPM.
- **Regulated orgs** (pharma, finance, defense) — natural v1.x target, blocked on the air-gap roadmap.

Not targeted: enterprises with an existing Posit relationship, individual users (use [drat](https://github.com/eddelbuettel/drat) or [r-universe](https://r-universe.dev/)), teams fine with `install_github` from a private repo.

## Documentation

- [design.md](design.md) — architecture and v1 scope.
- [implementation.md](implementation.md) — phased build plan.
- [research.md](research.md) — prior-art survey.

## License

MIT. See [LICENSE](LICENSE).
