# End-to-end suite

`make e2e` runs real R clients against a live packyard container.
Docker is the only requirement. It is not part of Woodpecker CI, whose
Kubernetes runners have no Docker daemon. Run it on a Docker host
before tagging a release.

```sh
make e2e                     # both jobs
make e2e E2E_DISTROS=rhel9   # one
```

| Job | Server `distro` | Client image |
|---|---|---|
| `jammy` | `jammy` | Ubuntu 22.04, Posit R 4.4.3 + 4.5.3 under `/opt/R` ([client/jammy.Dockerfile](client/jammy.Dockerfile)) |
| `rhel9` | `rhel9` | AlmaLinux 9, same R versions ([client/rhel9.Dockerfile](client/rhel9.Dockerfile)) |

[run.sh](run.sh) boots the server with a `matrix.yaml` for the job's
distro (cells `r-4.4`, `r-4.5`) and a `channels.yaml` where `prod`
allows anonymous reads and `dev` doesn't, then runs
[scenarios.sh](scenarios.sh) inside the client:

- **Fixture:** `testpkg` 0.1.0, 0.2.0 and 0.3.0 are published through
  [examples/ci/packyard-publish.sh](../../examples/ci/packyard-publish.sh)
  with only R 4.4 visible. Each version gets an `r-4.4` binary and no
  `r-4.5` one. Then 0.3.0 is yanked.
- **Scenarios:**
  1. `available.packages()` on `__linux__` shows only 0.2.0.
  2. R 4.4 installs the binary.
  3. R 4.5 installs from source.
  4. `readRDS()` of `Meta/archive.rds`.
  5. `remotes::install_version()` of an archived version.
  6. `renv::restore()` of a pinned archived version. Scenarios 4–6
     run against both the `src/contrib` and `__linux__` URLs.
  7. pak: current version, plus a `url::` pin.
  8. The yanked version installs by exact pin.
  9. The wrong distro fails.
  10. `dev` refuses anonymous reads.
  11. [packyard-backfill.sh](../../examples/ci/packyard-backfill.sh)
      fills `r-4.5`, after which R 4.5 gets a binary.
  12. [s3-cranlike-to-bundle.R](../../examples/bundler/s3-cranlike-to-bundle.R)
      exports `prod`, current and archived versions, as a bundle, and
      `admin import bundle` loads it into `dev`.

Server logs and the User-Agent strings the clients sent land in
`tests/e2e/out/<distro>/` (git-ignored).

**pak and archived versions.** `pak::pkg_install("pkg@1.0.0")` looks
archived versions up in CRAN's metadata service only. Against any
other repository it silently installs the current version. Pin
archived internal packages in pak with
`url::<repo>/src/contrib/Archive/<pkg>/<pkg>_<ver>.tar.gz`, or use
renv or remotes, which read `Meta/archive.rds`.

## `testpkg/`

A minimal R package: no compiled code, no dependencies. The scenarios
rewrite its `Version:` per fixture. Edit it cautiously.
