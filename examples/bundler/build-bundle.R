#!/usr/bin/env Rscript
#
# build-bundle.R — produce a CRAN-shaped bundle directory for packyard
# air-gap import. See design.md §10 and docs/airgap.md.
#
# Usage:
#   # Subset mode: declared packages plus their transitive deps.
#   Rscript build-bundle.R \
#     --packages packages.txt \
#     --r-version 4.4 \
#     --snapshot cran-r4.4-2026q1 \
#     --out bundle/
#
#   # Full mode: all of CRAN's src/contrib/ for the given R minor.
#   Rscript build-bundle.R \
#     --full \
#     --r-version 4.4 \
#     --snapshot cran-r4.4-full-2026q1 \
#     --out bundle/
#
# Output is a CRAN-shaped directory at <out>/ containing
# src/contrib/PACKAGES + tarballs + manifest.json. Tar it and carry
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
  out
}

print_help <- function() {
  cat("Usage: build-bundle.R [--packages FILE | --full] --r-version X.Y --snapshot ID --out DIR [--repos URL] [--with-suggests]\n")
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

write_manifest <- function(args, included) {
  contrib <- file.path(args$out, "src", "contrib")
  tarballs <- list.files(contrib, pattern = "\\.tar\\.gz$", full.names = TRUE)

  hash_one <- function(p) {
    list(
      name    = sub("_.*$", "", basename(p)),
      version = sub("^[^_]+_(.+)\\.tar\\.gz$", "\\1", basename(p)),
      path    = file.path("src", "contrib", basename(p)),
      sha256  = unname(tools::md5sum(p)),  # placeholder; overridden below
      size    = file.info(p)$size
    )
  }
  rows <- lapply(tarballs, hash_one)
  # Recompute with sha256 (tools::md5sum is what's stdlib; for sha256
  # we need a separate call). digest is in base 4.5+; on 4.4 fallback
  # to a manual call to openssl::sha256 if available, or read+digest.
  for (i in seq_along(rows)) {
    rows[[i]]$sha256 <- compute_sha256(tarballs[[i]])
  }

  manifest <- list(
    schema       = "packyard-bundle/1",
    snapshot_id  = args$snapshot,
    r_version    = args$r_version,
    source_url   = args$repos,
    mode         = if (args$full) "full" else "subset",
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

  included <- if (args$full) build_full(args) else build_subset(args)

  write_manifest(args, included)

  message(sprintf("[bundler] wrote bundle to %s (%d packages)",
                  normalizePath(args$out), length(included)))
  message("[bundler] next: tar czf bundle.tar.gz -C $(dirname ", args$out, ") $(basename ", args$out, ")")
}

main()
