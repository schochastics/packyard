# Reference CI scripts

Two CI-vendor-neutral shell scripts for publishing R packages to
packyard, plus a thin GitHub/Gitea Actions wrapper
([publish.yml](publish.yml)). They need `bash`, `curl` and `jq`, and
an image with every R version the server builds binaries for installed
under `/opt/R/<version>/` — the layout Posit's R builds, rig and our
Workbench/Connect images use.

| Script | What it does |
|---|---|
| [packyard-publish.sh](packyard-publish.sh) | Reads the server's cells (`GET /api/v1/cells`), builds the source tarball, builds one binary per R minor, publishes source + binaries to each target channel. |
| [packyard-backfill.sh](packyard-backfill.sh) | Asks the server which current versions lack a binary (`GET /api/v1/channels/{channel}/missing-binaries`), builds those and attaches them (`POST …/binaries/{cell}`). |
| [packyard-ci-lib.sh](packyard-ci-lib.sh) | Helpers both scripts source. Keep it next to them. |

## Publish

```sh
export PACKYARD_SERVER=https://packages.example.org
export PACKYARD_TOKEN=pkm_…            # publish:<channel> on every target
export PACKYARD_CHANNELS="prod test dev"
cd path/to/package && /path/to/packyard-publish.sh
```

What happens:

1. The server says which distro it serves and which R minors it has
   cells for. The script never hard-codes the list, so adding an R
   version to `matrix.yaml` is picked up by the next publish.
2. `R CMD build` with the default R version (`default_r_minor`).
3. For each cell: `R CMD INSTALL --build` with the newest installed
   patch release of that minor, after installing the package's hard
   dependencies (Depends/Imports/LinkingTo) from `getOption("repos")`
   into a per-R-version library. Point `repos` at CRAN (PPM) and at
   packyard's `__linux__` URL — images configured by managed-infra
   already do.
4. One publish per channel. The response's `missing_cells` is printed.

A cell whose build fails, or whose R version isn't installed, is
skipped: clients on that R version get the source tarball until
`packyard-backfill.sh` fills it in. The job fails only when the source
build, the **default** R version's binary build, or a publish fails.

Override dependency installation with `PACKYARD_DEPS_CMD` (run with
`$RSCRIPT` set to the Rscript for the R version being built and
`R_LIBS` to its library), e.g. to use pak:

```sh
export PACKYARD_DEPS_CMD='"$RSCRIPT" -e "pak::local_install_deps(lib = .libPaths()[1])"'
```

A 409 `version_immutable` means the version already exists on an
immutable channel with different bytes: bump `Version:` in
DESCRIPTION. Republishing byte-identical content is a no-op, so
reruns are safe. (`R CMD build` output isn't byte-reproducible, so a
rerun on a fresh checkout usually *does* differ — gate prod publishes
on a version bump.)

## Backfill

```sh
export PACKYARD_CHANNEL=prod           # one channel per run
export PACKYARD_CELL=r-4.6             # optional
/path/to/packyard-backfill.sh
```

Run it after adding an R version to `matrix.yaml`, or on a schedule
to retry failed builds. The token needs `publish:<channel>`, plus
`read:<channel>` unless the channel sets `anonymous_reads` (the script
downloads the source tarballs it builds from). Only the current
version of each package is backfilled; older versions stay source-only
on new R versions.

On the server, `packyard-server admin missing-binaries -channel prod`
prints the same list.

## Woodpecker

```yaml
# .woodpecker/publish.yml
when:
  - event: push
    branch: [main, test, dev]

steps:
  - name: publish
    image: gitea.example.org/org/workbench:latest   # every R under /opt/R
    environment:
      PACKYARD_SERVER: https://packages.example.org
      PACKYARD_TOKEN:
        from_secret: packyard_token
    commands:
      - |
        case "$CI_COMMIT_BRANCH" in
          main) export PACKYARD_CHANNELS="prod test dev" ;;
          test) export PACKYARD_CHANNELS="test dev" ;;
          *)    export PACKYARD_CHANNELS="dev" ;;
        esac
      - .ci/packyard-publish.sh
```

## GitHub / Gitea Actions

Copy [publish.yml](publish.yml) to `.github/workflows/` (or
`.gitea/workflows/`), the scripts to `.ci/`, and set the
`PACKYARD_SERVER` / `PACKYARD_BUILD_IMAGE` variables and the
`PACKYARD_TOKEN` secret.
