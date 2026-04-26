#!/usr/bin/env Rscript
#
# build-bundle.R — produce a CRAN-shaped bundle directory for packyard
# air-gap import. See design.md §10 and docs/airgap.md.
#
# Usage:
#   # Subset source bundle (default): declared packages + transitive deps.
#   Rscript build-bundle.R \
#     --packages packages.txt \
#     --r-version 4.4 \
#     --snapshot cran-r4.4-2026q1 \
#     --out bundle/
#
#   # Full source bundle: all of CRAN's src/contrib/ for the given R minor.
#   Rscript build-bundle.R \
#     --full \
#     --r-version 4.4 \
#     --snapshot cran-r4.4-full-2026q1 \
#     --out bundle/
#
#   # Binary bundle for a specific cell (e.g. RHEL 9 amd64, R 4.4).
#   # Run after the matching source bundle is already on the channel.
#   # Pull binaries from Posit Public Package Manager — works from any
#   # build host (Mac, Linux, Windows) because P3M's __linux__/<distro>
#   # URLs serve precompiled tarballs without UA sniffing.
#   Rscript build-bundle.R \
#     --packages packages.txt \
#     --r-version 4.4 \
#     --snapshot cran-r4.4-2026q1 \
#     --binary-cell rhel9-amd64-r-4.4 \
#     --binary-repo https://packagemanager.posit.co/cran/__linux__/rhel9/2026-04-01 \
#     --out bundle-bin/
#
# Output is a CRAN-shaped directory at <out>/ containing
# {src,bin}/contrib/PACKAGES + tarballs + manifest.json. Tar it and carry
# it to the air-gap site, then run on the disconnected packyard:
#
#   packyard-server admin import bundle <bundle.tar.gz> --channel <snapshot>
#
# The bundler depends only on `miniCRAN` (and its deps). Install
# once on your connected build host:
#
#   install.packages("miniCRAN")

suppressPackageStartupMessages({
  if (!requireNamespace("miniCRAN", quietly = TRUE)) {
    stop("miniCRAN is required: install.packages(\"miniCRAN\")", call. = FALSE)
  }
  library(miniCRAN)
  library(tools)
})

# --- argument parsing (no flags package: keep deps minimal) ---------------

parse_args <- function(argv) {
  out <- list(
    packages_file = NULL,
    full          = FALSE,
    r_version     = NULL,
    snapshot      = NULL,
    out           = NULL,
    repos         = "https://cloud.r-project.org",
    binary_cell   = NULL,
    binary_repo   = NULL,
    deps          = c("Imports", "Depends", "LinkingTo")
  )
  i <- 1
  while (i <= length(argv)) {
    a <- argv[[i]]
    val <- function() {
      if (i + 1 > length(argv)) stop(sprintf("flag %s needs a value", a), call. = FALSE)
      argv[[i + 1]]
    }
    switch(a,
      "--packages"     = { out$packages_file <- val(); i <- i + 2 },
      "--full"         = { out$full <- TRUE; i <- i + 1 },
      "--r-version"    = { out$r_version <- val(); i <- i + 2 },
      "--snapshot"     = { out$snapshot <- val(); i <- i + 2 },
      "--out"          = { out$out <- val(); i <- i + 2 },
      "--repos"        = { out$repos <- val(); i <- i + 2 },
      "--binary-cell"  = { out$binary_cell <- val(); i <- i + 2 },
      "--binary-repo"  = { out$binary_repo <- val(); i <- i + 2 },
      "--with-suggests"= { out$deps <- c(out$deps, "Suggests"); i <- i + 1 },
      "-h"             = { print_help(); quit(save = "no", status = 0) },
      "--help"         = { print_help(); quit(save = "no", status = 0) },
      stop(sprintf("unknown flag: %s (try --help)", a), call. = FALSE)
    )
  }
  if (is.null(out$r_version)) stop("--r-version is required", call. = FALSE)
  if (is.null(out$snapshot))  stop("--snapshot is required", call. = FALSE)
  if (is.null(out$out))       stop("--out is required",      call. = FALSE)
  if (!out$full && is.null(out$packages_file)) {
    stop("either --packages <file> or --full is required", call. = FALSE)
  }
  # Binary mode is mutually exclusive with --full and requires both
  # --binary-cell and --binary-repo. Subset is the only sensible binary
  # shape: closed list of packages + their deps for one cell.
  if (!is.null(out$binary_cell) || !is.null(out$binary_repo)) {
    if (is.null(out$binary_cell) || is.null(out$binary_repo)) {
      stop("--binary-cell and --binary-repo must be set together", call. = FALSE)
    }
    if (out$full) {
      stop("--full is not supported in binary mode; use --packages with a closed list", call. = FALSE)
    }
  }
  out
}

print_help <- function() {
  cat("Usage: build-bundle.R [--packages FILE | --full] --r-version X.Y --snapshot ID --out DIR\n")
  cat("                      [--repos URL] [--with-suggests]\n")
  cat("                      [--binary-cell NAME --binary-repo P3M_URL]\n")
  cat("\n")
  cat("Source mode (default):    bundles source tarballs from --repos.\n")
  cat("Binary mode (binary-*):   bundles precompiled tarballs for one cell.\n")
}

# --- inputs --------------------------------------------------------------

read_packages <- function(path) {
  lines <- trimws(readLines(path, warn = FALSE))
  lines <- lines[nzchar(lines) & !startsWith(lines, "#")]
  # Strip optional "==version" pin — miniCRAN resolves to the upstream
  # latest matching the R-version snapshot, not arbitrary version pins.
  # Pins are recorded in the lockfile after the fact.
  sub("==.*$", "", lines)
}

# --- main ----------------------------------------------------------------

build_subset <- function(args) {
  pkgs <- read_packages(args$packages_file)
  message(sprintf("[bundler] resolving deps for %d packages...", length(pkgs)))
  closure <- pkgDep(pkgs,
    repos    = args$repos,
    type     = "source",
    suggests = "Suggests" %in% args$deps,
    Rversion = args$r_version)
  message(sprintf("[bundler] dependency closure: %d packages", length(closure)))
  makeRepo(closure,
    path     = args$out,
    repos    = args$repos,
    type     = "source",
    Rversion = args$r_version)
  closure
}

build_full <- function(args) {
  message("[bundler] full mode: enumerating all CRAN packages for R ", args$r_version)
  db <- available.packages(
    repos   = contrib.url(args$repos, type = "source"),
    type    = "source",
    filters = "duplicates"
  )
  pkgs <- rownames(db)
  message(sprintf("[bundler] downloading %d packages...", length(pkgs)))
  makeRepo(pkgs,
    path     = args$out,
    repos    = args$repos,
    type     = "source",
    Rversion = args$r_version)
  pkgs
}

# Binary mode: resolve the closure against the P3M-style binary repo,
# then download each precompiled tarball directly into
# bin/linux/<cell>/<pkg>_<ver>.tar.gz so the bundle layout matches
# packyard's URL surface.
#
# We bypass miniCRAN::makeRepo() for the binary case because makeRepo
# always writes to src/contrib/, and putting binaries there would
# misrepresent the layout to anyone serving the bundle as a static
# CRAN. Reusing miniCRAN::pkgDep keeps the dep resolver intact —
# P3M's distro URL exposes a CRAN-shape src/contrib/PACKAGES that
# pkgDep walks correctly.
build_binary <- function(args) {
  pkgs <- read_packages(args$packages_file)
  message(sprintf("[bundler] resolving binary deps for %d packages against %s ...",
                  length(pkgs), args$binary_repo))
  closure <- pkgDep(pkgs,
    repos    = args$binary_repo,
    type     = "source",
    suggests = "Suggests" %in% args$deps,
    Rversion = args$r_version)
  message(sprintf("[bundler] dependency closure: %d packages", length(closure)))

  # Resolve concrete versions from the upstream PACKAGES index.
  available <- available.packages(
    repos   = contrib.url(args$binary_repo, type = "source"),
    type    = "source",
    filters = "duplicates"
  )
  missing <- setdiff(closure, rownames(available))
  if (length(missing) > 0) {
    stop(sprintf("[bundler] %d packages in the closure are not available at %s: %s",
                 length(missing), args$binary_repo,
                 paste(head(missing, 10), collapse = ", ")), call. = FALSE)
  }

  cell_dir <- file.path(args$out, "bin", "linux", args$binary_cell)
  dir.create(cell_dir, showWarnings = FALSE, recursive = TRUE)

  contrib_url <- contrib.url(args$binary_repo, type = "source")
  for (p in closure) {
    ver <- available[p, "Version"]
    fn  <- sprintf("%s_%s.tar.gz", p, ver)
    src <- paste0(contrib_url, "/", fn)
    dst <- file.path(cell_dir, fn)
    message(sprintf("[bundler] %s %s ...", p, ver))
    if (utils::download.file(src, destfile = dst, mode = "wb", quiet = TRUE) != 0L) {
      stop(sprintf("[bundler] download failed: %s", src), call. = FALSE)
    }
  }

  # Regenerate a PACKAGES index next to the tarballs so the bundle is
  # also a working "tarball CRAN" served behind a static web server.
  # type="source" matches the upstream P3M layout: Linux binaries are
  # packaged as source-shape tarballs that R installs without compiling.
  tools::write_PACKAGES(cell_dir, type = "source")

  closure
}

write_manifest <- function(args, included) {
  if (is_binary_mode(args)) {
    write_manifest_binary(args, included)
  } else {
    write_manifest_source(args, included)
  }
}

is_binary_mode <- function(args) !is.null(args$binary_cell)

# write_manifest_source produces a packyard-bundle/2 source manifest.
# Per-package entries carry a `source` blob; no `binaries[]`.
write_manifest_source <- function(args, included) {
  contrib <- file.path(args$out, "src", "contrib")
  tarballs <- list.files(contrib, pattern = "\\.tar\\.gz$", full.names = TRUE)

  rows <- lapply(tarballs, function(p) {
    list(
      name    = sub("_.*$", "", basename(p)),
      version = sub("^[^_]+_(.+)\\.tar\\.gz$", "\\1", basename(p)),
      source  = list(
        path   = file.path("src", "contrib", basename(p)),
        sha256 = compute_sha256(p),
        size   = file.info(p)$size
      )
    )
  })

  manifest <- list(
    schema       = "packyard-bundle/2",
    snapshot_id  = args$snapshot,
    r_version    = args$r_version,
    source_url   = args$repos,
    mode         = if (args$full) "full" else "subset",
    kind         = "source",
    created_at   = format(Sys.time(), "%Y-%m-%dT%H:%M:%SZ", tz = "UTC"),
    tool         = sprintf("examples/bundler/build-bundle.R (miniCRAN %s, R %s)",
                           packageVersion("miniCRAN"),
                           paste(R.version$major, R.version$minor, sep = ".")),
    input_packages = if (args$full) NULL else read_packages(args$packages_file),
    packages     = rows
  )

  json <- to_json(manifest)
  writeLines(json, file.path(args$out, "manifest.json"))
}

# write_manifest_binary produces a packyard-bundle/2 binary manifest.
# Per-package entries carry a `binaries[]` list with the single cell
# (this script always emits one cell per bundle).
write_manifest_binary <- function(args, included) {
  cell_dir <- file.path(args$out, "bin", "linux", args$binary_cell)
  tarballs <- list.files(cell_dir, pattern = "\\.tar\\.gz$", full.names = TRUE)

  rows <- lapply(tarballs, function(p) {
    list(
      name    = sub("_.*$", "", basename(p)),
      version = sub("^[^_]+_(.+)\\.tar\\.gz$", "\\1", basename(p)),
      binaries = list(list(
        cell   = args$binary_cell,
        path   = file.path("bin", "linux", args$binary_cell, basename(p)),
        sha256 = compute_sha256(p),
        size   = file.info(p)$size
      ))
    )
  })

  manifest <- list(
    schema       = "packyard-bundle/2",
    snapshot_id  = args$snapshot,
    r_version    = args$r_version,
    source_url   = args$binary_repo,
    mode         = "subset",
    kind         = "binary",
    cell         = args$binary_cell,
    created_at   = format(Sys.time(), "%Y-%m-%dT%H:%M:%SZ", tz = "UTC"),
    tool         = sprintf("examples/bundler/build-bundle.R (miniCRAN %s, R %s)",
                           packageVersion("miniCRAN"),
                           paste(R.version$major, R.version$minor, sep = ".")),
    input_packages = read_packages(args$packages_file),
    packages     = rows
  )

  json <- to_json(manifest)
  writeLines(json, file.path(args$out, "manifest.json"))
}

# Tiny dependency-free SHA-256 helper. R 4.5+ ships tools::sha256sum;
# 4.4 doesn't, so fall back to openssl::sha256 if installed, then to
# digest::digest. One of those three is always reachable on a host
# that already has miniCRAN.
compute_sha256 <- function(path) {
  if (exists("sha256sum", where = asNamespace("tools"))) {
    return(unname(tools::sha256sum(path)))
  }
  if (requireNamespace("openssl", quietly = TRUE)) {
    return(as.character(openssl::sha256(file(path))))
  }
  if (requireNamespace("digest", quietly = TRUE)) {
    return(digest::digest(file = path, algo = "sha256"))
  }
  stop("no SHA-256 implementation available; install 'openssl' or 'digest'", call. = FALSE)
}

# Tiny JSON serialiser so we don't pull jsonlite. Handles lists, named
# lists, atomic vectors of length-1, and character/numeric/logical.
to_json <- function(x, indent = 0) {
  pad <- strrep("  ", indent)
  if (is.null(x)) return("null")
  if (is.logical(x) && length(x) == 1) return(if (x) "true" else "false")
  if (is.numeric(x) && length(x) == 1) return(format(x, scientific = FALSE))
  if (is.character(x) && length(x) == 1) return(json_string(x))
  if (is.list(x) && !is.null(names(x))) {
    items <- vapply(seq_along(x), function(i) {
      sprintf("%s  %s: %s", pad, json_string(names(x)[[i]]), to_json(x[[i]], indent + 1))
    }, character(1))
    return(sprintf("{\n%s\n%s}", paste(items, collapse = ",\n"), pad))
  }
  if (is.list(x) || length(x) > 1) {
    items <- vapply(x, function(v) sprintf("%s  %s", pad, to_json(v, indent + 1)), character(1))
    return(sprintf("[\n%s\n%s]", paste(items, collapse = ",\n"), pad))
  }
  json_string(as.character(x))
}

json_string <- function(s) {
  # Escape just enough for the values we produce — no embedded
  # control characters, no Unicode hijinks.
  s <- gsub("\\\\", "\\\\\\\\", s, perl = TRUE)
  s <- gsub("\"",   "\\\\\"",   s, perl = TRUE)
  sprintf("\"%s\"", s)
}

# --- entrypoint ----------------------------------------------------------

main <- function() {
  args <- parse_args(commandArgs(trailingOnly = TRUE))
  dir.create(args$out, showWarnings = FALSE, recursive = TRUE)

  included <- if (is_binary_mode(args)) {
    build_binary(args)
  } else if (args$full) {
    build_full(args)
  } else {
    build_subset(args)
  }

  write_manifest(args, included)

  message(sprintf("[bundler] wrote bundle to %s (%d packages)",
                  normalizePath(args$out), length(included)))
  message("[bundler] next: tar czf bundle.tar.gz -C $(dirname ", args$out, ") $(basename ", args$out, ")")
}

main()
