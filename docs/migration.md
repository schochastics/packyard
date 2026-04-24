# Migrating to packyard

Moving an existing internal-R-packages setup onto packyard. Covers the
two migration paths packyard ships with — drat repos and git repos —
plus the one-line change consumers need in their `install.packages()`
/ `install_github()` calls.

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

# New: packyard, default channel
options(repos = c(INTERNAL = "https://packyard.corp", getOption("repos")))

# New: packyard, specific channel
options(repos = c(DEV = "https://packyard.corp/dev", getOption("repos")))
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
- **Backfill per-cell binaries.** There's no good way to do this
  after-the-fact for older versions — the cell images may no longer
  exist. Import restores the source tree; binaries for the CURRENT
  version get built by CI on the next push.
- **Delete from the source.** Imports are additive. Clean up the drat
  repo / archive the git branch manually once you've verified packyard
  has what you need.
