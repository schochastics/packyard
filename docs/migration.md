# Migrating to packyard

Moving an existing internal-R-packages setup onto packyard. Covers the
migration paths packyard ships with: any CRAN-like repository (an S3
bucket, a static web server, another packyard) with its archived
versions, drat repos, and git repos. It also covers the one-line
change consumers need in their `install.packages()` /
`install_github()` calls.

## From an S3-hosted (or any) CRAN-like repository

A repository that R already installs from, with `src/contrib/PACKAGES`,
`Archive/` and `Meta/archive.rds`, moves over complete, archived
versions included. That covers a public-read S3 bucket laid out like
PPM, a directory behind nginx, or a CRAN-like layout written by
`tools::write_PACKAGES()`. It's two steps: export to a bundle, then
import the bundle.

```sh
# 1. On any host with R >= 4.5 that can reach the old repository:
Rscript examples/bundler/s3-cranlike-to-bundle.R \
  --repo https://s3.example.org/s3pm-prod/latest \
  --out  bundle-prod/

# 2. On the packyard host (the target channel must exist):
packyard-server admin -data /data import bundle bundle-prod/ -channel prod
```

- **`--repo`** is the URL R users have in `repos =`. The script reads
  `PACKAGES` for the current versions and `Meta/archive.rds` for the
  archived ones. Without `archive.rds` you get the current versions
  only, and a warning.
- **One bundle per channel.** For a bucket per environment, e.g.
  `s3pm-prod`, `s3pm-test` and `s3pm-dev`, run the pair once per
  bucket, into `prod`, `test` and `dev`.
- **Source only.** Binaries in the old repository, such as a
  `__linux__/jammy/latest` tree, are not carried over. CI rebuilds
  them for every R version in `matrix.yaml`: run
  [packyard-backfill.sh](../examples/ci/packyard-backfill.sh) once
  per cell after the import. That also covers R versions the old
  repository never had binaries for.
- **Channel policy applies.** Importing into an immutable channel
  twice is safe: identical bytes are skipped, and different bytes for
  an existing version fail that package. A mutable channel is
  overwritten.
- **Yanks don't exist in the source,** so every imported version is
  live, and `PACKAGES` serves the highest. That matches repositories
  like s3pm, where the current version is always the highest. Yank
  anything that shouldn't be current after importing.
- **Cutover.** Import while the old repository still serves, switch
  `repos =` (below), then re-run both steps to pick up anything
  published in between. Re-imports are idempotent.

[tests/e2e](../tests/e2e/) runs this round trip on every run.


## From drat

[drat](https://github.com/eddelbuettel/drat) repos are already shaped
like CRAN: a file tree with `src/contrib/PACKAGES` and per-package
source tarballs. packyard can pull the full set in one command.

```sh
packyard-server admin import drat https://drat.example.org -channel dev
```

What it does:

1. Fetches `https://drat.example.org/src/contrib/PACKAGES`.
2. For each entry, downloads the tarball and calls the in-process
   publish path (same validation, same event log as an HTTP publish,
   no bearer token required).
3. Reports `imported / skipped / failed` counts at the end. Individual
   tarball failures don't abort the run — they show up in `failed`.

Notes:

- **Current versions only:** the drat importer reads `PACKAGES`. To
  bring archived versions along, use the bundle path above; a drat
  repo is a CRAN-like repository.
- Source-only. drat doesn't carry per-cell binaries, so neither does
  this import. CI takes over for binaries going forward.
- Target channel must exist in `channels.yaml` before you run. If the
  channel is **immutable** and you re-import the same repo, bytes that
  already match return `skipped`; bytes that differ for the same
  `(channel, name, version)` abort that one package and continue.
- On a **mutable** channel a re-import overwrites identical content
  (same sha, no new CAS blob) and shows up as an `overwrote` publish.

### After the import

Point your R users at packyard:

```r
# Old: drat
options(repos = c(INTERNAL = "https://drat.example.org", getOption("repos")))

# New: packyard, binaries for the client's R version (distro from matrix.yaml)
options(repos = c(INTERNAL = "https://packyard.corp/prod/__linux__/jammy/latest", getOption("repos")))

# New: packyard, source only, default channel
options(repos = c(INTERNAL = "https://packyard.corp", getOption("repos")))
```

`install.packages()` just works from there — packyard serves the CRAN
protocol at `/<channel>/src/contrib/PACKAGES` and the default-channel
alias at `/src/contrib/PACKAGES`.

## From git (one repo at a time)

For packages that live in a git repo without a drat sidecar, use the
git importer. It shallow-clones and runs `R CMD build`, so you need
both `git` and `R` on the machine running the command.

```sh
packyard-server admin import git https://git.example.org/foo.git -branch main -channel dev
```

What it does:

1. `git clone --depth 1 --branch <b> <url>` into a temp dir.
2. Reads `(Package, Version)` from `DESCRIPTION` in the clone root.
3. `R CMD build` to produce `<name>_<version>.tar.gz`.
4. Imports the tarball. The event row's `note` is the
   `https://…@<branch>` string so the audit log records the source.

The temp dir is wiped after the command, success or fail.

### Migrating from install_github / devtools workflows

Old:

```r
devtools::install_github("corp/foo", ref = "main")
```

New (once `foo` has been imported at least once):

```r
install.packages("foo", repos = "https://packyard.corp/dev")
```

You'll typically wrap the import + tag for release in the project's CI
— see [examples/ci/publish.yml](../examples/ci/publish.yml) for the
template. The git importer is for one-off backfills and quick
experiments, not a replacement for CI-driven publishes.

## Scripting larger migrations

Both `admin import` commands work well inside a shell loop. For a
dozen git repos:

```sh
while read repo; do
  packyard-server admin import git "$repo" -branch main -channel dev \
    || echo "failed: $repo" >> failures.log
done < repos.txt
```

## What migration does NOT do

- **Preserve publish history.** Imports show up in `/ui/events` with
  `actor = import-drat` / `import-git`. There's no attempt to
  reconstruct original publish timestamps or authors.
- **Import binaries.** Imports are source only. Binaries for the
  current version of each package come from a CI backfill
  (`admin missing-binaries`, `examples/ci/packyard-backfill.sh`).
  Archived versions stay source-only and compile on install.
- **Delete from the source.** Imports are additive. Clean up the drat
  repo / archive the git branch manually once you've verified packyard
  has what you need.
