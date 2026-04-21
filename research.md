# Pakman — Prior-Art Research for a New R Package Manager

## Context

`pakman` is intended to be a brand-new R package manager, built from scratch in an empty repository (`/Users/david/projects/pakman`). Before committing to a design, this document captures what already exists — inside R and across other language ecosystems — and identifies the strengths, weaknesses, and transferable ideas. The goal is to avoid re-inventing what works, avoid known footguns, and have a clear view of what R is missing relative to modern standards.

This is a **research deliverable**, not an implementation plan. A separate design document should follow once the direction is chosen.

---

## 1. The R Ecosystem Today

### 1.1 Base R: `install.packages()` and friends

- **What:** `install.packages()`, `update.packages()`, `remove.packages()`, `.libPaths()`, library-path env vars (`R_LIBS`, `R_LIBS_USER`, `R_LIBS_SITE`). Repositories described by flat `PACKAGES` text files under `src/contrib` and `bin/{windows,macosx}/contrib/{R-major.minor}`.
- **Strengths:** Zero setup, universal, single source of truth for what a "package" is.
- **Weaknesses:**
  - No dependency resolver — greedy, order-dependent installs.
  - No lockfile / reproducibility primitive.
  - Serial HTTP, no parallelism.
  - `.libPaths()` precedence is genuinely confusing (two env vars + site + user + RStudio's additions).
  - `library` (directory) vs `package` (artifact) terminology confuses newcomers.
  - `install.packages()` vs `library()` split — installing ≠ loading.
  - `SystemRequirements` is free-text, not machine-readable.
  - Linux has **no** CRAN-hosted binaries — source-only by default.
  - No concept of "project."

### 1.2 CRAN / Bioconductor / PPM / r-universe / R-Forge

- **CRAN:** The reference repository. Immutable-ish; archive keeps old versions. Windows + macOS binaries per R minor version. Linux = source. Flat `PACKAGES` index.
- **Bioconductor:** Parallel repo with a strict 6-month release cycle pinned to R's annual release. Cross-repo dependencies routed through [BiocManager](https://bioconductor.github.io/BiocManager/). Forces R-version coupling, which is a reproducibility constraint most solvers don't natively model.
- **Posit Public Package Manager (PPM, formerly RSPM):** Binary builds for many Linux distros, snapshot dates, CDN-served. Does what CRAN doesn't on Linux. Free to use but Posit-operated — infrastructure risk.
- **r-universe** (Jeroen Ooms, rOpenSci): Per-user/per-org binary repos built from GitHub. Decentralized CRAN-alternative; excellent for pre-release/dev code.
- **MRAN** (retired Nov 2023): Microsoft-run date-snapshots of CRAN. Its death killed `checkpoint`. Lesson: **do not depend on infrastructure you don't control.**
- **R-Forge:** Legacy SVN-based build farm. Largely superseded by r-universe.

### 1.3 Project-level reproducibility tools

- **[renv](https://rstudio.github.io/renv/)** — The de-facto standard.
  - *Strengths:* RStudio integration, JSON lockfile, project-local libraries, global cache, widely known.
  - *Weaknesses:* **Post-hoc snapshotting** (records what you ended up with, not what you asked for — conflicts get silently baked in). No real dependency solver. Serial installs. No cross-platform lockfile. Lockfile has no R-version / OS / glibc markers, so "reproducibility" often fails on a different OS. Python integration is cosmetic.
- **[packrat](https://rstudio.github.io/packrat/)** — renv's deprecated predecessor; same model, worse UX.
- **[pak](https://pak.r-lib.org/)** (r-lib / Gábor Csárdi) — The installer R should have shipped with.
  - *Strengths:* True dependency solver (`pkgdepends`), parallel/async HTTP, caching (`pkgcache`), multi-source (CRAN, Bioc, GitHub, GitLab, URL, local), SystemRequirements→apt/dnf on supported distros, good progress UX.
  - *Weaknesses:* No lockfile — people pair it with renv. No workspaces. Not a replacement for renv's project-model. Still needs R to bootstrap.
- **[remotes](https://remotes.r-lib.org/) / `devtools::install_*`** — Serial, no resolver, but ubiquitous for GitHub installs.
- **[pkgr](https://metrumresearchgroup.github.io/pkgr/)** (Metrum) — Declarative YAML config, pharma/audit focus, fail-fast. Complements renv. One of the few R tools that starts from "declare first, resolve second."
- **[rv](https://github.com/A2-ai/rv)** (A2-Ai, first release March 2025) — Rust-based, declarative config + lockfile, resolves the full tree up front, multi-source, project-local envs, explicitly inspired by `uv` and Cargo. Docs at [a2-ai.github.io/rv-docs](https://a2-ai.github.io/rv-docs/). Focused on package resolution; **does not** manage R installations and has no `rv run`. (Caveat: there is a separate Ruby "rv" project from 2025 — don't confuse.)
- **[uvr](https://github.com/nbafrank/uvr)** (nbafrank, first commit March 2026, ~80★ at time of writing) — **The other direct prior-art project, and the most feature-complete so far.** Rust, 2-crate workspace (`uvr-core` + `uvr` CLI), MIT. Goes further than rv: adds built-in R-version management (`uvr r install/use/pin`), `uvr run` in isolated env, `uvr doctor`, `uvr export`/`import` for renv.lock interop, P3M-binary-first with source fallback, GitHub + Bioconductor sources, r-hub sysreqs API for Linux system deps. Manifest `uvr.toml`, lockfile `uvr.lock`, project library at `.uvr/library/`. Companion R package [uvr-r](https://github.com/nbafrank/uvr-r) wraps the CLI for RStudio/Positron users. *Strengths:* closest to a uv-for-R experience; benchmarks claim 2–50× faster installs than renv/pak/`install.packages` on warm caches; explicit renv.lock compatibility path. *Weaknesses/gaps:* single-maintainer, ~1 month old; Linux has no P3M binaries so falls back to source; Windows needs Rtools for source fallback; no mention of content-addressable store or hardlinks (project-local `.uvr/library/` only); no workspaces; PubGrub-style resolver explanations not yet demonstrated. **Both rv and uvr should be studied deeply before designing anything** — together they define the "declarative Rust-based R PM" design space that `pakman` enters. The fact that two independent projects converged on nearly the same design in 12 months is itself signal.

### 1.4 R-version managers (adjacent)

- **[rig](https://github.com/r-lib/rig)** (r-lib, Rust-based) — The standard for installing & switching R versions. Not a package manager, but any serious PM has to integrate with it (or subsume it, like `uv` did with `rye`/`pyenv`).

### 1.5 Binary-first / system-integrated

- **[r2u](https://github.com/eddelbuettel/r2u)** (Eddelbuettel) — CRAN as Debian `.deb`s. Full system-lib resolution via apt. Via `bspm`, `install.packages()` transparently uses apt. *Strengths:* fastest installs in the R world, correct system deps. *Weaknesses:* Ubuntu/Debian-only, not portable.
- **Nix / [rix](https://docs.ropensci.org/rix/)** — Full hermetic reproducibility, multi-language. *Weaknesses:* steep learning curve, macOS/Windows still rough, small community.
- **[Rocker](https://rocker-project.org/)** — Docker images. The "nuclear" reproducibility option. Usually layered *under* renv, not a replacement.

### 1.6 Date/snapshot-based reproducibility

- **checkpoint** (Microsoft, archived 2023) — Dead with MRAN.
- **[groundhog](https://groundhogr.com/)** — Date-based on-load fetching; loans packages from a dated library. OS/R-version agnostic.

### 1.7 Loader/namespace helpers (not package managers, often confused for them)

- **pacman** (`p_load`), **librarian**, **automagic**, **needs**, **import**, **box**, **switchr** — Different DX sugar over `install.packages` + `library`. Not reproducible; mostly convenience for interactive work.

### 1.8 Cross-cutting R pain points

1. No semantic versioning enforcement; R package authors rarely bump deliberately.
2. `SystemRequirements` is a free-text string, not structured. Only PPM/pak parse it, and only for some distros.
3. Bioconductor ↔ R-version coupling is a *global constraint* that most resolvers can't express.
4. Compilation burden on Linux (and historically macOS/Windows with C/Fortran deps like GDAL, GSL, openMP).
5. Bootstrapping: every R tool (renv, pak, BiocManager) needs R already running. Chicken-and-egg for fresh machines & air-gapped installs.
6. No workspace / monorepo primitive. Tidyverse is built by hand.
7. No task runner (`renv::run()` exists but isn't a standard).
8. CRAN security: MD5 only, no signing, no SBOM, no provenance. Behind every other major ecosystem.
9. `.libPaths()` precedence is a recurring source of "why isn't this installed?" tickets.

---

## 2. Cross-Language Landscape (what's worth stealing from)

### 2.1 Python

- **[uv](https://docs.astral.sh/uv/)** (Astral, Rust) — **The single biggest influence to study.** Unifies pip + pip-tools + virtualenv + pyenv + poetry + pipx. 10–100× faster than pip. Global content-addressable cache with hardlinks/reflinks into project venvs. **PubGrub** resolver with explainable errors. TOML lockfile (`uv.lock`) with cross-platform markers. Manages Python itself. Workspaces. `uv run` script runner. Standalone binary, no runtime dependency.
- **[Poetry](https://python-poetry.org)** — Popularized `pyproject.toml`, dependency groups, PubGrub (via Mixology). Slower than uv; heavier.
- **pip + pip-tools + virtualenv** — The legacy stack uv is eating. pip-tools introduced the "resolve once, lock, sync" discipline.
- **pdm / hatch / rye** — rye merged into uv (2024). pdm/hatch are still alive but losing share.
- **[pixi](https://prefix.dev/)** (prefix.dev, Rust) — Cross-language (Python, C++, **R via conda-forge**), conda-compatible, fast solver, tasks, lockfile. Relevant as a *multi-language scientific* PM since R users overlap with this audience.
- **conda / mamba / micromamba** — Legacy SAT-based, libsolv-backed, heavy but complete. Pixi is the modern successor.

### 2.2 Rust — [Cargo](https://doc.rust-lang.org/cargo/)

The gold standard nearly every modern PM cites. Key ideas:
- Manifest (`Cargo.toml`) vs lock (`Cargo.lock`) separation.
- Strict semver enforcement.
- Immutable registry (crates.io) — once published, never mutated (only yanked).
- Workspaces as a first-class concept.
- Compile-time **features** (optional dependencies). R has no analog.
- `cargo new`, `cargo check`, `cargo test`, `cargo publish`, `cargo tree` — unified UX.
- Single binary, no runtime required.

### 2.3 JavaScript

- **npm** — `package-lock.json`, flat-ish node_modules, workspaces since v7. The pathological ancestor.
- **yarn** (classic / berry) — Berry's Plug'n'Play eliminates node_modules entirely.
- **[pnpm](https://pnpm.io/motivation)** — **The other biggest influence to study.** Global content-addressable store at `~/.pnpm-store`, **hardlinked** into a strict symlink-farm `node_modules/.pnpm`. 50–70 % disk savings, much faster installs. Enforces non-flat resolution (can't accidentally use transitive deps).
- **bun** — Fast, Zig-based; rough edges.
- **deno** — URL imports; interesting anti-pattern for a PM.

### 2.4 Go modules — [MVS](https://research.swtch.com/vgo-mvs)

Minimum Version Selection: pick the *lowest* version satisfying every requirement. No SAT, ~500 LOC. Lockfile-free (`go.sum` is just checksums). Requires strict import-compatibility (breaking change = new import path). **Interesting but probably unfit for R** — R has no MVS culture.

### 2.5 Ruby — [Bundler](https://bundler.io/)

`Gemfile` + `Gemfile.lock`, dependency groups, PubGrub-based resolver. Solid, mature, unsurprising. Version managers (rbenv/rvm) separate from PM.

### 2.6 Julia — [Pkg.jl](https://pkgdocs.julialang.org/v1/)

**Closest sibling to R conceptually.** `Project.toml` + `Manifest.toml`. Environment *stacks* (shared+project). Artifacts system for non-Julia binaries (BLAS, GDAL-equivalents). Reproducibility-first design. **Worth studying** for the artifact model and the stacked-environment UX.

### 2.7 Haskell — Cabal + [Stack/Stackage](https://docs.haskellstack.org/)

Stack's **curated LTS snapshots** (tested-together package sets, pinned to a GHC version) are a great fit for the CRAN+Bioconductor reality. The "snapshot as default, pinning as override" model is a real idea to steal.

### 2.8 Others worth glancing at

- **opam** (OCaml) — Pre-publication matrix testing against the whole ecosystem. CRAN incoming tests are weaker.
- **Swift Package Manager** — Cargo-clone with PubGrub.
- **Dart pub** — Origin of PubGrub.
- **Elixir mix+hex** — Elegant, PubGrub.
- **composer** (PHP) — Solid SAT solver, `composer.lock`.
- **Nix** — Functional, content-addressed, hermetic. Philosophically influential.
- **[Spack](https://spack.io/)** — HPC scientific PM. Multi-version coexistence, compiler/variant tracking, matrix builds. R's scientific audience overlaps heavily.

---

## 3. Cross-Cutting Design-Space Themes

### 3.1 Resolver

| Algorithm | Used by | Strength | Weakness |
|---|---|---|---|
| **PubGrub** | uv, Poetry, Bundler, Dart, Swift PM, Elixir | Explainable errors, fast in practice | Newer implementations |
| **SAT (libsolv)** | conda, RPM | Industrial-strength | Cryptic errors |
| **MVS** | Go | Trivial, predictable | Requires cultural buy-in on semver |
| **ILP** | pak | Works for R's actual constraint shape | Error explanation weaker than PubGrub |
| **Greedy / backtrack** | pip-legacy, npm-legacy | Simple | Exponential worst case, bad errors |
| **Curated snapshot** | Stack LTS, MRAN, PPM | Zero resolution cost | Less flexibility |

**Recommendation:** PubGrub is the modern default and R users benefit most from explainable failures. pak's ILP is a credible alternative worth benchmarking against.

### 3.2 Lockfile

Essentials: `name`, `version`, **`sha256`** (not MD5), source URL + registry id, platform/ABI markers, R-version marker (Bioc coupling). Format: TOML is standard; YAML acceptable; JSON (renv.lock) is fine but noisier. **One file with multi-platform entries** (uv, modern Poetry) beats per-OS lockfiles. Always include **integrity hashes** — CRAN's MD5 is below the industry baseline.

### 3.3 Global content-addressable store

pnpm and uv prove this works. For R:
- Key by `sha256(source tarball)` and separately by `sha256(built artifact + R version + platform)`.
- **Hardlink** into project libraries (default) with **reflink** fallback on APFS/Btrfs/XFS and **copy** fallback on Windows/NTFS.
- Hash-verify on install only (not every read) — R packages are large.
- This alone would solve the biggest renv pain point (per-project cache bloat).

### 3.4 Installation model

Project-local is the winning default (matches renv's mental model and RStudio integration). Activation should be automatic via `.Rprofile` or an RStudio hook — but also scriptable from shell, since modern users run things outside RStudio. Global/fallback lib is fine for `rig`-style tooling packages.

### 3.5 Binary distribution

Hardest R-specific problem. Options ordered by increasing ambition:
1. **Ride on PPM and r-universe** — metadata-only PM; let existing binary repos do the work.
2. **Build-cache layer** — hash of (source, R-version, platform, glibc/BLAS/GDAL versions) → binary artifact. Like pixi/conda's model but R-flavored.
3. **Vendor system libs** (conda model) — biggest investment, best portability.

Tag binaries with something PEP-425-like: `pkg-ver-Rmajor-platform-abi.tgz`. R currently has no ABI tags — anything is an improvement.

### 3.6 Version grammar

R's `Depends: foo (>= 1.0)` is already fine. Add semver-style sugar (`^`, `~`) but don't break DESCRIPTION compatibility. Consider optional **CRAN-date snapshots** à la groundhog/PPM as a parallel constraint mechanism.

### 3.7 Registry protocol

Flat `PACKAGES` is legacy but universal — must stay compatible. Layer a JSON/MessagePack index with ETags for incremental updates. Support multiple ordered sources (CRAN, Bioc, PPM, r-universe, private). Authentication for private registries (GitHub Packages, Posit-managed).

### 3.8 Workspaces

R has essentially nothing here. A simple model:
```
pakman.toml   # workspace root
  packages/
    pkgA/DESCRIPTION
    pkgB/DESCRIPTION  # Depends: pkgA
```
Shared lockfile, workspace-local path resolution. Low cost, high differentiator.

### 3.9 Environment management

Default to auto-activate via `.Rprofile`. Respect `RENV_ACTIVATE=FALSE`-style escape hatch. Consider Julia-style env stacks (base + project overlay) for HPC/lab-shared-lib use cases.

### 3.10 Reproducibility tiers

Build in escalating strictness:
1. **Version-pinned lockfile** (renv-equivalent).
2. **+ platform/ABI-pinned lockfile** (what renv is missing).
3. **+ snapshot date** coupling (MRAN/PPM/groundhog semantics).
4. **+ system-lib hashes** (Nix-equivalent; opt-in).

### 3.11 Bootstrapping — the Rust question

Every modern fast PM (`uv`, `pixi`, `rv`, `rustup`, `rig`) is **Rust-based + single binary**. This is the industry direction and for good reason:
- No chicken-and-egg with R itself.
- Air-gapped friendly.
- Fast + parallel by default.
- Easy cross-compile for Win/macOS/Linux × x86_64/arm64.

Counter-argument: the R contributor pool is ~0 % Rust. A pure-R tool (pak/renv model) gets more contributors but is slow and requires R to bootstrap. Middle path: Rust core + optional R bindings via extendr/Rcpp, following `rig`'s example.

### 3.12 Developer UX

Table-stakes for a 2026 PM:
- `pakman init` scaffolding.
- `pakman add <pkg>` / `pakman remove`.
- `pakman sync` / `pakman lock` / `pakman lock --upgrade [pkg]`.
- `pakman tree`, `pakman outdated`, `pakman why <pkg>`.
- `pakman run <task>`.
- Shell completions (bash/zsh/fish/pwsh).
- Dry-run on every mutating op.
- **Explainable resolver errors** (the biggest UX differentiator in 2026).

### 3.13 Security

CRAN is behind every other major ecosystem here. Minimum upgrades:
- SHA256 (not MD5) integrity hashes in the lockfile.
- Sigstore/provenance support for packages published via modern CI (GitHub, GitLab).
- Lockfile signing.
- Audit trail: `pakman audit` listing CVEs / retracted packages.

### 3.14 Offline / air-gapped

`pakman vendor` (copy all resolved packages into `./vendor/`), plus a mirror-building mode. Critical for pharma, HPC, regulated industries — a natural R user base that renv serves poorly.

### 3.15 Task running

Not essential for v1. If added, keep trivial: tasks declared in the manifest, `pakman run <name>` activates env + executes. Don't reinvent `targets`/`make`.

---

## 4. What R Specifically Lacks vs. Modern Ecosystems

1. **Up-front declarative resolution.** renv snapshots *after the fact*; rv/uv/Cargo resolve *before* installing. This is the single most impactful conceptual upgrade.
2. **Explainable resolver errors.** PubGrub-style "A needs B ≥ 2 but C needs B < 2" vs R's "Warning: package 'foo' had non-zero exit status."
3. **Cross-platform lockfile.** renv.lock is implicitly one-machine. uv.lock handles Linux/Mac/Windows × arch × Python-version in one file.
4. **Content-addressable global store with hardlinks.** renv's cache copies. pnpm/uv hardlink.
5. **Integrated language-version management.** uv bundles Python-install. R has rig, separate.
6. **Workspaces.** Nothing in R. Cargo/pnpm/uv all have them.
7. **Standalone-binary bootstrap.** Every R tool requires R already installed.
8. **Modern security** (SHA256, signing, provenance, vuln audit).
9. **Structured SystemRequirements.** Only PPM/pak parse this today.
10. **Task runner.** `uv run`, `cargo run`, `pixi run` — R has none canonical.
11. **Pre-publication matrix CI** (opam-style). CRAN's incoming tests are thinner.
12. **Immutable + cryptographically-verified registry.** CRAN archives but doesn't sign or hash-guarantee.

---

## 5. Ideas Most Worth Stealing for pakman

High-leverage, differentiated:
1. **uv's whole model** (manifest + lockfile + content-addressable cache + PubGrub + standalone binary + R-version management).
2. **pnpm's hardlink store design.**
3. **Cargo's workspace + publish + `tree`/`check`/`run` unified UX.**
4. **rv's and uvr's declarative-first R-flavored configs** (read their TOML manifests, resolver semantics, error output). uvr's CLI surface (`init`/`add`/`sync`/`lock`/`tree`/`run`/`r install`/`doctor`/`export renv.lock`) is a ready-made UX template.
5. **Stack's curated snapshot mode** (CRAN-date-as-default reproducibility).
6. **Julia's artifact system** (for non-R system deps).
7. **pak's ILP resolver + parallel async HTTP** (even if ultimately replaced by PubGrub).
8. **r2u's "use the OS package manager when possible"** optional fast-path on Debian/Ubuntu.
9. **Spack's variant/ABI tracking** for scientific-computing users.
10. **opam-style pre-publication matrix testing** as a long-term registry ambition.

---

## 6. Open Design Questions (for the Design Phase)

These are **not** to be answered here — just flagged:

1. **Implementation language.** Pure R (pak/renv model), pure Rust (uv/rv model), or hybrid (Rust core + R shim)?
2. **Scope.** Replace only renv? Or renv + pak + rig + publishing? (uv-style unification vs single-purpose tool.)
3. **Registry relationship.** Build a new registry, or metadata-only PM over CRAN/Bioc/PPM/r-universe?
4. **Compatibility.** Must read existing `renv.lock`? `DESCRIPTION`? `pak` cache?
5. **Bioconductor coupling.** First-class, or community-maintained source plugin?
6. **Binary story.** Ride on PPM, build our own cache, or vendor-system-lib model?
7. **Resolver choice.** PubGrub, ILP (pak-style), MVS, or SAT?
8. **Target audience priority.** Solo data scientist / Shiny app dev / pharma / HPC / package author — these have conflicting defaults.
9. **CLI-first or REPL-first.** `pakman` as a shell binary, or `library(pakman)` as an R package, or both?
10. **Config format.** TOML (industry norm), YAML (pkgr precedent), or R DSL (Rcpp/devtools precedent)?

---

## 7. Reference Inventory

Tools/docs/readings the design phase should touch:

**R-world:**
- [renv docs](https://rstudio.github.io/renv/) · [pak docs](https://pak.r-lib.org/) · [pkgdepends](https://r-lib.github.io/pkgdepends/) · [pkgcache](https://r-lib.github.io/pkgcache/) · [rig](https://github.com/r-lib/rig) · [rv](https://github.com/A2-ai/rv) · [rv docs](https://a2-ai.github.io/rv-docs/) · [uvr](https://github.com/nbafrank/uvr) · [uvr homepage](https://nbafrank.github.io/uvr/) · [uvr-r companion](https://github.com/nbafrank/uvr-r) · [pkgr](https://metrumresearchgroup.github.io/pkgr/) · [r2u](https://github.com/eddelbuettel/r2u) · [rix](https://docs.ropensci.org/rix/) · [groundhog](https://groundhogr.com/) · [BiocManager](https://bioconductor.github.io/BiocManager/) · [PPM](https://docs.posit.co/rspm/) · [r-universe](https://r-universe.dev/) · [Rocker](https://rocker-project.org/)

**Cross-language:**
- [uv docs](https://docs.astral.sh/uv/) · [Astral blog](https://astral.sh/blog) · [pnpm motivation](https://pnpm.io/motivation) · [Cargo book](https://doc.rust-lang.org/cargo/) · [pixi](https://prefix.dev/) · [Poetry](https://python-poetry.org/docs/) · [Pkg.jl](https://pkgdocs.julialang.org/v1/) · [Stack](https://docs.haskellstack.org/) · [Bundler](https://bundler.io/) · [Swift PM](https://docs.swift.org/swiftpm/) · [Spack](https://spack.io/) · [Nix](https://nixos.org/manual/nix/)

**Design-space readings:**
- [PubGrub writeup](https://nex3.medium.com/pubgrub-2fb6470504f) · [Go MVS (Russ Cox)](https://research.swtch.com/vgo-mvs) · [PEP 425 compatibility tags](https://peps.python.org/pep-0425/) · [Cargo.toml vs Cargo.lock](https://doc.rust-lang.org/cargo/guide/cargo-toml-vs-cargo-lock.html) · [Lockfile design tradeoffs](https://arxiv.org/abs/2505.04834) · [Reproducible builds](https://reproducible-builds.org/)

---

## 8. Verification — what "done" looks like for this research phase

- ☐ User agrees the inventory in §1 (R ecosystem) is complete, or names additional tools.
- ☐ User confirms cross-language scope in §2 is right, or asks to dig into specific ones further.
- ☐ User picks a position on each of the §6 open questions before the design phase begins.
- ☐ Any specific tool (likely **rv**, **uv**, **pnpm**) gets a deeper dive — read their actual source/RFCs — before design starts.
