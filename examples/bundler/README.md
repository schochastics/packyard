# `examples/bundler/` — build a CRAN bundle for air-gap import

Reference R script that produces a CRAN-shaped bundle directory
suitable for `packyard-server admin import bundle`. Run it on a
host that *has* network access to CRAN; carry the output to your
disconnected packyard via your approved file-transfer process.

See [docs/airgap.md](../../docs/airgap.md) for the operator
playbook end-to-end. See [design.md §10](../../design.md) for the
design rationale.

## Files

| File | Purpose |
|---|---|
| `build-bundle.R` | The bundler. Wraps `miniCRAN::makeRepo()` + writes a packyard `manifest.json`. ~200 lines, no external deps beyond `miniCRAN`. |
| `packages.txt.example` | Sample input for subset mode. Copy to `packages.txt`, edit. |

## Prerequisites on the build host

```r
install.packages("miniCRAN")
```

R 4.4+ recommended. The script is tested with R 4.4 and 4.5.

## Subset mode — declare packages, get their dep closure

```sh
cp packages.txt.example packages.txt
# edit packages.txt: one package per line

Rscript build-bundle.R \
  --packages   packages.txt \
  --r-version  4.4 \
  --snapshot   cran-r4.4-2026q2 \
  --out        ./bundle/
```

Output: a directory `bundle/` containing
`src/contrib/PACKAGES{,.gz}`, every tarball in the dependency
closure, and `manifest.json`. Tar it for transport:

```sh
tar -C bundle -czf cran-r4.4-2026q2.tar.gz .
```

## Full mode — all of CRAN at the latest available version

```sh
Rscript build-bundle.R \
  --full \
  --r-version 4.4 \
  --snapshot  cran-r4.4-full-2026q2 \
  --out       ./bundle/
```

Expect tens of GB of source downloads and ~30+ minutes runtime.
For most teams, subset mode is the right answer.

## Binary mode — pre-built tarballs from P3M

When the air-gap host can't (or shouldn't) compile from source, build
a separate **binary bundle** for one cell at a time. The bundle is
imported on top of a matching source bundle — packyard merges the
binaries into the existing `(channel, name, version)` rows.

```sh
# Step 1: build + import the source bundle (any host, any time).
Rscript build-bundle.R \
  --packages   packages.txt \
  --r-version  4.4 \
  --snapshot   cran-r4.4-2026q2 \
  --out        ./bundle-src/

# Step 2: build the binary bundle for one cell. Run from any build
# host (Mac, Linux, Windows) — Posit Public Package Manager's
# __linux__/<distro>/<snapshot> URLs serve precompiled tarballs
# regardless of the requesting client's OS.
Rscript build-bundle.R \
  --packages    packages.txt \
  --r-version   4.4 \
  --snapshot    cran-r4.4-2026q2 \
  --binary-cell rhel9-amd64-r-4.4 \
  --binary-repo https://packagemanager.posit.co/cran/__linux__/rhel9/2026-04-01 \
  --out         ./bundle-bin/
```

Tar both for transport, then on the air-gap host:

```sh
packyard-server admin import bundle ./bundle-src.tar.gz --channel cran-r4.4-2026q2
packyard-server admin import bundle ./bundle-bin.tar.gz --channel cran-r4.4-2026q2
```

**Order matters.** The binary bundle attaches binaries to existing
package rows; if the source bundle hasn't been imported yet, every
package in the binary bundle lands in `failed=` with
`source row not found`. Re-run the binary import after the source
import succeeds — the second run is idempotent.

The cell name (`--binary-cell`) must match a cell declared in
`matrix.yaml` on the air-gap server. The default matrix ships
Ubuntu cells; add RHEL / Rocky / Alma rows yourself before
importing. The bundler does not validate this on the build side.

## Flags

| Flag | Required | Meaning |
|---|---|---|
| `--packages FILE` | subset mode | Plain-text input, one package name per line. `==version` suffix is parsed but currently advisory. |
| `--full` | source full mode | Snapshot every package on CRAN for the given R minor. Mutually exclusive with `--packages` and with `--binary-*`. |
| `--r-version X.Y` | yes | R minor version the bundle targets. Affects which package versions miniCRAN will resolve to. |
| `--snapshot ID` | yes | Bundle identity, recorded in `manifest.json`. Convention: `cran-r<minor>-<period>` e.g. `cran-r4.4-2026q1`. Used as the channel name on the import side. |
| `--out DIR` | yes | Output directory. Created if missing. |
| `--repos URL` | no | Upstream source repo, defaults to `https://cloud.r-project.org`. Override to point at a snapshot mirror (PPM dated URL, RSPM, an internal mirror). |
| `--with-suggests` | no | Also walk `Suggests:`. Roughly doubles bundle size. Off by default. |
| `--binary-cell NAME` | binary mode | Cell to bundle for, matching a name in the air-gap server's `matrix.yaml` (e.g. `rhel9-amd64-r-4.4`). Activates binary mode; requires `--binary-repo` and `--packages`. |
| `--binary-repo URL` | binary mode | P3M-style URL whose `src/contrib/` path serves precompiled tarballs for the cell, e.g. `https://packagemanager.posit.co/cran/__linux__/rhel9/2026-04-01`. |

## What the bundle looks like

```
bundle/
├── manifest.json
└── src/
    └── contrib/
        ├── PACKAGES
        ├── PACKAGES.gz
        ├── PACKAGES.rds
        ├── ggplot2_3.5.1.tar.gz
        ├── dplyr_1.1.4.tar.gz
        └── ... (transitive deps)
```

## "Tarball CRAN" — the bundle is a self-serving repo

The output is a valid CRAN repository on its own. Drop it behind
any static web server and `install.packages()` works:

```sh
cd bundle && python3 -m http.server 8000
```

```r
install.packages("ggplot2", repos = "http://localhost:8000/", type = "source")
```

Useful for evaluation or CI runners that don't need the full
packyard read surface — but for the audit trail, channel scoping,
and per-snapshot pinning that production air-gap deployments
need, import the bundle into packyard:

```sh
packyard-server admin import bundle ./cran-r4.4-2026q2.tar.gz \
  --channel cran-r4.4-2026q2
```

## What's in `manifest.json`

```json
{
  "schema": "packyard-bundle/1",
  "snapshot_id": "cran-r4.4-2026q2",
  "r_version": "4.4",
  "source_url": "https://cloud.r-project.org",
  "mode": "subset",
  "created_at": "2026-04-25T07:44:08Z",
  "tool": "examples/bundler/build-bundle.R (miniCRAN 0.3.2, R 4.5.2)",
  "input_packages": ["ggplot2", "dplyr", "data.table", "testthat"],
  "packages": [
    {
      "name": "ggplot2",
      "version": "3.5.1",
      "path": "src/contrib/ggplot2_3.5.1.tar.gz",
      "sha256": "abc123...",
      "size": 4123456
    }
  ]
}
```

The packyard import side validates `sha256` for every tarball
before insert. `input_packages` and `tool` are recorded for the
audit trail; the importer otherwise ignores them.

## Signing (optional)

For supply-chain attestations, sign `manifest.json` with your tool
of choice on the build host:

```sh
# minisign
minisign -Sm bundle/manifest.json
# or cosign
cosign sign-blob --key cosign.key bundle/manifest.json > bundle/manifest.json.sig
# or gpg
gpg --detach-sign bundle/manifest.json
```

Distribute the signature alongside the bundle. Operators verify on
the air-gap side before running `import bundle`. Packyard itself
does not enforce signing in v1.x — that's the operator's policy
choice.
