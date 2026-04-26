# Air-gap deploy playbook

Operator-facing walkthrough for running packyard on a host with no
internet egress and getting CRAN packages onto it via offline
sync-bundles. As of v1.x both halves of the workflow ship: the
bundle *producer* (an R script under `examples/bundler/`) and the
*importer* (`packyard-server admin import bundle`).

For the design rationale, see [design.md §10](../design.md). For
the bundler itself, see [examples/bundler/](../examples/bundler/).

## When to use this

Network-isolated deployments — pharma, finance, defense, classified
research — where the packyard host can't reach `cran.r-project.org`
on its own. If your deployment can route to upstream CRAN (even
through an authenticated proxy), the simpler shape is to skip
air-gap and just point R at packyard + CRAN as two repos (see
[quickstart.md](quickstart.md)).

## What you'll need

**On the connected build host:**

- R 4.4 or newer
- `miniCRAN` (`install.packages("miniCRAN")`)
- Network access to a CRAN mirror (or PPM, or RSPM, or any source
  with `<repo>/src/contrib/PACKAGES`)
- The [bundler script](../examples/bundler/build-bundle.R)
- Disk for the bundle — subset mode ~tens of MB to a few GB; full
  mode ~25 GB sources, ~80 GB if you also bundle binaries

**On the disconnected packyard host:**

- packyard installed (Docker or binary; see
  [quickstart.md](quickstart.md))
- Disk for the imported snapshot — comparable to bundle size, with
  CAS dedup across snapshots reducing the marginal cost
- Your approved file-transfer process (USB, one-way diode, signed
  tarball drop) for moving the bundle across the air-gap

## End-to-end

### 1. Decide the snapshot identity

Pick a stable name, by convention `cran-r<minor>-<period>`:

```
cran-r4.4-2026q1
cran-r4.5-2026q2
cran-internal-baseline-2026
```

This becomes the channel name on packyard. R configs that pin to
this snapshot will reference it forever — choose names you won't
regret.

### 2. Build the bundle on the connected host

```sh
cd packyard/examples/bundler
cp packages.txt.example packages.txt
$EDITOR packages.txt   # one package per line, no version pins needed

Rscript build-bundle.R \
  --packages   packages.txt \
  --r-version  4.4 \
  --snapshot   cran-r4.4-2026q1 \
  --out        ./bundle/

tar -C bundle -czf cran-r4.4-2026q1.tar.gz .
```

For all of CRAN instead of a curated subset, swap `--packages
packages.txt` for `--full` — see
[examples/bundler/README.md](../examples/bundler/README.md) for
the full flag reference.

The bundle is now `cran-r4.4-2026q1.tar.gz`. Inside is a
CRAN-shaped repo (`src/contrib/` with `PACKAGES` + tarballs) and a
`manifest.json` with per-package sha256.

### 3. Sign (optional but recommended)

For supply-chain attestations, sign `manifest.json` with whatever
your security policy mandates:

```sh
# Pick one — they all sign the same JSON file.
minisign -Sm bundle/manifest.json
cosign sign-blob --key cosign.key bundle/manifest.json > bundle/manifest.json.sig
gpg --detach-sign bundle/manifest.json
```

Distribute the signature alongside the bundle. Packyard itself
does not enforce signing in v1.x; verification is a policy step
your operator does on the air-gap side before running `import`.

### 4. Transport across the air-gap

Use whatever your security process mandates. Common shapes:

- USB drive, signed manifest verified on the receiving host before
  copying off the drive
- One-way data diode with a downstream verifier
- Two-stage approval: bundle hashed and logged on the connected
  side, hash re-checked on the air-gap side before import

Whatever the method, keep the **per-bundle sha256** somewhere
out-of-band so you can spot a corrupted transfer.

### 5. Import on the disconnected packyard host

First, declare the snapshot channel in `channels.yaml`. Packyard does
not auto-create channels on import — `channels.yaml` is the source of
truth for channel policy, and an air-gap snapshot needs to be
explicitly immutable.

```yaml
# channels.yaml
- name: cran-r4.4-2026q1
  overwrite_policy: immutable
```

Restart the server (or re-run `packyard-server -init`) so the channel
gets reconciled into the DB, then run the import:

```sh
packyard-server admin import bundle ./cran-r4.4-2026q1.tar.gz \
  -channel cran-r4.4-2026q1
```

What happens:

- The bundle's `manifest.json` is validated (schema must be
  `packyard-bundle/1` or `packyard-bundle/2`).
- **Pre-flight:** every tarball is sha256-verified against the
  manifest. Any mismatch aborts before any side effects — neither
  CAS nor DB is touched.
- Each tarball is then written to CAS. Blobs already present (from
  a previous snapshot) are deduplicated automatically.
- Per-package DB rows are inserted referencing the CAS blobs.
- An audit event is appended for every package imported.

Importing a 5 GB bundle that overlaps 95% with last quarter's
snapshot adds only the new packages to disk — the rest is reused
from CAS.

### 6. Point R at the snapshot

On client machines, configure R to read from the new channel:

```r
options(repos = c(
  cran-r4.4-2026q1 = "http://packyard.internal/cran-r4.4-2026q1/",
  packyard         = "http://packyard.internal/",
  getOption("repos")
))
```

Or in `.Rprofile` for project pinning:

```r
options(repos = c(
  cran = "http://packyard.internal/cran-r4.4-2026q1/"
))
```

Now `install.packages("ggplot2")` resolves to the curated bundle
content, not to upstream CRAN. The pin holds forever — the
immutable channel doesn't change after import.

## Updating

Snapshots are immutable by design. To "update CRAN," cut a new
snapshot:

1. Repeat steps 2–5 with a new `--snapshot cran-r4.4-2026q2`.
2. Old `cran-r4.4-2026q1` stays — projects that pinned to it still
   resolve.
3. Update R configs that should track latest to point at the new
   channel name. Old projects don't move unless you choose to.

Coexistence is intentional: pharma audit trails depend on
"version X of analysis was reproducible against snapshot Y forever."

## Troubleshooting

**"Bundler downloaded fewer packages than I expected."**
Check `--with-suggests` — by default the bundler walks `Imports`,
`Depends`, and `LinkingTo` only. Adding `Suggests` roughly doubles
the closure but covers test-only deps.

**"Some packages in `packages.txt` weren't included."**
miniCRAN skips base / recommended R packages (which ship with R
itself — no point bundling them). Check `manifest.json
.input_packages` vs `.packages[].name`; recommended packages only
appear in the input list.

**"Import says sha256 mismatch."**
Bundle was modified or corrupted in transit. Rebuild on the
connected side and re-transfer. If you signed `manifest.json`,
verify the signature first — that catches it earlier.

**"I want to import a hand-built bundle, not the script's output."**
Fine — packyard's importer cares about the format, not the
producer. Any directory with `src/contrib/PACKAGES` + tarballs +
a valid `manifest.json` works. See
[examples/bundler/README.md](../examples/bundler/README.md) for
the manifest schema.

**"How do I include compiled binaries, not just sources?"**
Build a separate **binary bundle** for each cell you need and
import it on top of the matching source bundle. See
[Pre-built binaries via Posit Public Package Manager](#pre-built-binaries-via-posit-public-package-manager)
below. The source bundle remains the baseline — the importer
attaches binaries onto existing `(channel, name, version)` rows.

## Pre-built binaries via Posit Public Package Manager

CRAN doesn't publish Linux binaries; the canonical source for
distro-specific precompiled tarballs is Posit Public Package
Manager (P3M). Packyard's bundle format treats binaries as a
second, optional bundle layered on top of a source bundle.

### When to use this

Skip this section if your air-gap site has a build toolchain
(`gcc`, `gfortran`, `R-devel`) and is happy compiling at install
time — that's the `R CMD INSTALL`-from-source path, and it works
on any distro. Use binary bundles when the air-gap host can't or
shouldn't compile, or when install-time compilation is too slow
for your operators.

### Build a binary bundle

```sh
# Run from any host (Mac, Linux, Windows) — P3M's Linux URLs serve
# precompiled tarballs based on the URL path, not on the requesting
# client's OS. So building RHEL 9 binaries on a Mac is fine.
Rscript build-bundle.R \
  --packages    packages.txt \
  --r-version   4.4 \
  --snapshot    cran-r4.4-2026q1 \
  --binary-cell rhel9-amd64-r-4.4 \
  --binary-repo https://packagemanager.posit.co/cran/__linux__/rhel9/2026-04-01 \
  --out         ./bundle-bin/

tar -C bundle-bin -czf cran-r4.4-2026q1-rhel9.tar.gz .
```

`--binary-cell` is the cell name as declared in `matrix.yaml` on
the air-gap server. The bundler does not validate this on the
build side — typos surface at import time as
`cell %q is not declared in matrix.yaml`.

### Add the cell to `matrix.yaml`

Before importing, declare the cell on the air-gap server:

```yaml
# matrix.yaml
cells:
  - name: rhel9-amd64-r-4.4
    os: linux
    os_version: rhel9
    arch: amd64
    r_minor: "4.4"
```

Restart the server so the matrix is reloaded.

### Import in order: source first, then binaries

```sh
# Step 1: source bundle. Creates the packages rows.
packyard-server admin import bundle ./cran-r4.4-2026q1.tar.gz \
  -channel cran-r4.4-2026q1

# Step 2: binary bundle. Attaches binaries to the existing rows.
packyard-server admin import bundle ./cran-r4.4-2026q1-rhel9.tar.gz \
  -channel cran-r4.4-2026q1
```

If you import the binary bundle first (or for a package that's
not in the source bundle), every binary entry in `manifest.packages`
lands in `failed=` with `source row not found; import the source
bundle first`. Re-running the binary import after the source import
is idempotent — already-present binaries are reported as `skipped=`.

### Multiple cells

One bundle = one cell. To support a second cell, run the bundler
again with a different `--binary-cell` / `--binary-repo` and
import the resulting archive. Cells are independent: the read
surface serves whichever cells the operator has populated, falling
back to source for clients on cells that aren't built.

## Status

| Component | Status |
|---|---|
| Bundle format spec | `packyard-bundle/2` (current); `packyard-bundle/1` accepted on read |
| Bundler script | Shipped in [`examples/bundler/`](../examples/bundler/) |
| Air-gap operator playbook | This doc |
| `admin import bundle` CLI | Shipped — see [admin.md](admin.md#admin-import-bundle-path-or-targz--channel-name) |
| Binary bundle support | Shipped — one cell per bundle, P3M as the binary source |
| Bundle-level signing enforcement | Deferred; manifest sha256 mandatory, ed25519 optional |
| Bioconductor support | Deferred; same format applies |
| Diff bundles | Deferred; full bundles + CAS dedup is enough at v1.x scale |
