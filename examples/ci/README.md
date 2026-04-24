# Reference CI workflow

A drop-in publish workflow for teams that host their R packages in git
(one package per repo) and want pushes to land in packyard. The template
at [publish.yml](publish.yml) runs on **GitHub Actions** and **Gitea
Actions** unmodified.

## Quickstart

1. Copy `publish.yml` into your repo at `.github/workflows/publish.yml`
   (or `.gitea/workflows/publish.yml`).
2. In your repo's CI settings, add:
   - **Secret** `PACKYARD_TOKEN` — a bearer token with `publish:<channel>`
     scope. Mint one with `packyard-server -mint-token -scopes publish:dev`.
   - **Variable** `PACKYARD_SERVER` — the base URL of your packyard, e.g.
     `https://packyard.corp` (no trailing slash).
3. Push to `main` or `develop`. The workflow builds source + per-cell
   binaries and publishes.

## What to customise

### The cell matrix

The `build-binary` job ships with two example cells:

```yaml
matrix:
  cell:
    - { name: ubuntu-24.04-amd64-r-4.4, image: ghcr.io/rocker-org/r-ver:4.4 }
    - { name: ubuntu-24.04-amd64-r-4.5, image: ghcr.io/rocker-org/r-ver:4.5 }
```

Every `cell.name` must also exist in the server's `matrix.yaml` — the
publish endpoint rejects binaries for unknown cells. To discover what
the server accepts:

```sh
curl -H "Authorization: Bearer $PACKYARD_TOKEN" $PACKYARD_SERVER/api/v1/cells
```

or open the `/ui/cells` page in the operator dashboard.

### The branch → channel mapping

The `publish` job hardcodes:

```sh
case "${{ github.ref }}" in
  refs/heads/main) echo "name=prod" >> "$GITHUB_OUTPUT" ;;
  *)               echo "name=dev"  >> "$GITHUB_OUTPUT" ;;
esac
```

Edit for whatever convention fits: tag pushes → `release`, PR merges →
`staging`, etc. The token's scope must include whatever channel you
resolve to.

### Triggers

The default `on:` block runs on pushes to `main` and `develop`, plus
manual dispatch. Add `tags:`, `pull_request:`, or `schedule:` as needed.
Remember to tighten the token scope if you broaden triggers.

## How publish failures surface

The workflow uses `curl --fail-with-body`, so any non-2xx response
prints the packyard error envelope (`error_code`, `message`, `hint` — see
[design.md §7.2](../../design.md)) and fails the job. Common causes:

- **403** — token scope doesn't include the resolved channel.
- **409** — channel is immutable and the version already exists. Bump
  `Version:` in DESCRIPTION or publish to a mutable channel.
- **422** — manifest references a cell not in `matrix.yaml` or a
  binary part that wasn't uploaded.

## Not included

- **`SystemRequirements` → apt translation.** The workflow assumes the
  cell image contains everything the package needs. For heavier
  dependencies (GDAL, Stan, etc.) register a fatter cell in the
  server's `matrix.yaml` and point the matrix entry at that image.
- **Monorepos** (multiple packages per repo). v1 is one-package-per-repo.
- **A maintained `packyard-publish@v1` action.** Deliberately: the
  canonical publish surface is the HTTP API, and a curl job keeps the
  failure modes simple and debuggable.

## Gitea notes

Gitea Actions is syntactically compatible with GitHub Actions. The
template ran unmodified in our testing. On locked-down Gitea setups
you may need to:

- Replace `uses: actions/checkout@v4` / `upload-artifact@v4` /
  `download-artifact@v4` with the mirrors your instance ships.
- Pick a `runs-on:` label that your Gitea runner actually advertises
  (default is `ubuntu-latest`).
