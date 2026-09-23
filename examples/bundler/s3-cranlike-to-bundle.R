#!/usr/bin/env Rscript
#
# s3-cranlike-to-bundle.R — turn an existing CRAN-like repository (for
# example a public-read S3 bucket) into a packyard source bundle, so its
# packages and their archived versions can be imported into a channel.
#
# Usage:
#   Rscript s3-cranlike-to-bundle.R \
#     --repo https://s3.example.org/s3pm-prod/latest \
#     --out  bundle-prod/
#
#   packyard-server admin import bundle bundle-prod/ -channel prod
#
# --repo is what R users put in repos=: the script reads
# <repo>/src/contrib/PACKAGES for the current versions and
# <repo>/src/contrib/Meta/archive.rds for the archived ones under
# Archive/<pkg>/. A repo without archive.rds contributes only its
# current versions (with a warning).
#
# Only source tarballs are bundled. Binaries are rebuilt by CI for every
# R version in the server's matrix.yaml (examples/ci/packyard-backfill.sh),
# which also covers R versions the old repository never had binaries for.
#
# Needs base R only (R >= 4.5 for tools::sha256sum, otherwise the
# openssl or digest package).

parse_args <- function(argv) {
  out <- list(repo = NULL, out = NULL,
              snapshot = paste0("migration-", format(Sys.Date(), "%Y%m%d")),
              r_version = paste(R.version$major, strsplit(R.version$minor, ".", fixed = TRUE)[[1]][1], sep = "."))
  i <- 1
  while (i <= length(argv)) {
    a <- argv[[i]]
    if (a %in% c("-h", "--help")) {
      cat("Usage: s3-cranlike-to-bundle.R --repo URL --out DIR [--snapshot ID] [--r-version X.Y]\n")
      quit(save = "no", status = 0)
    }
    if (i + 1 > length(argv)) stop(sprintf("flag %s needs a value", a), call. = FALSE)
    key <- switch(a, "--repo" = "repo", "--out" = "out", "--snapshot" = "snapshot",
                  "--r-version" = "r_version",
                  stop(sprintf("unknown flag: %s (try --help)", a), call. = FALSE))
    out[[key]] <- argv[[i + 1]]
    i <- i + 2
  }
  if (is.null(out$repo)) stop("--repo is required", call. = FALSE)
  if (is.null(out$out)) stop("--out is required", call. = FALSE)
  out$repo <- sub("/+$", "", out$repo)
  out
}

contrib <- function(repo, ...) paste(c(paste0(repo, "/src/contrib"), ...), collapse = "/")

fetch <- function(url, dest) {
  dir.create(dirname(dest), showWarnings = FALSE, recursive = TRUE)
  ok <- tryCatch(utils::download.file(url, dest, mode = "wb", quiet = TRUE) == 0L,
                 error = function(e) FALSE, warning = function(w) FALSE)
  if (!ok) stop(sprintf("download failed: %s", url), call. = FALSE)
  dest
}

current_files <- function(repo) {
  tmp <- tempfile()
  on.exit(unlink(tmp))
  db <- read.dcf(fetch(contrib(repo, "PACKAGES"), tmp), fields = c("Package", "Version"))
  sprintf("%s_%s.tar.gz", db[, "Package"], db[, "Version"])
}

# Archived files as "<pkg>/<pkg>_<ver>.tar.gz", from Meta/archive.rds.
archived_files <- function(repo) {
  tmp <- tempfile()
  on.exit(unlink(tmp))
  got <- tryCatch(fetch(contrib(repo, "Meta", "archive.rds"), tmp), error = function(e) NULL)
  if (is.null(got)) {
    warning("no Meta/archive.rds at ", repo, "; bundling current versions only", call. = FALSE)
    return(character())
  }
  unlist(lapply(readRDS(got), rownames), use.names = FALSE)
}

sha256 <- function(path) {
  if (exists("sha256sum", where = asNamespace("tools"))) return(unname(tools::sha256sum(path)))
  if (requireNamespace("openssl", quietly = TRUE)) return(as.character(openssl::sha256(file(path))))
  if (requireNamespace("digest", quietly = TRUE)) return(digest::digest(file = path, algo = "sha256"))
  stop("no SHA-256 implementation available; use R >= 4.5 or install 'openssl'", call. = FALSE)
}

# Minimal JSON writer (no jsonlite dependency). Values are plain
# strings and numbers; package names and versions need no escaping
# beyond quotes and backslashes.
json_str <- function(s) sprintf("\"%s\"", gsub("([\"\\\\])", "\\\\\\1", s))
package_json <- function(p) {
  sprintf('    {"name": %s, "version": %s, "source": {"path": %s, "sha256": %s, "size": %.0f}}',
          json_str(p$name), json_str(p$version), json_str(p$path), json_str(p$sha256), p$size)
}

main <- function(args) {
  archived <- archived_files(args$repo)
  current <- current_files(args$repo)
  message(sprintf("[bundler] %s: %d current, %d archived tarballs",
                  args$repo, length(current), length(archived)))

  # Archived first so each package's versions are imported oldest first;
  # packyard orders by version either way.
  rels <- c(file.path("Archive", archived), current)
  pkgs <- lapply(rels, function(rel) {
    file <- basename(rel)
    dest <- file.path(args$out, "src", "contrib", rel)
    message("[bundler] ", rel)
    fetch(contrib(args$repo, rel), dest)
    list(name = sub("_.*$", "", file),
         version = sub("^[^_]+_(.+)\\.tar\\.gz$", "\\1", file),
         path = file.path("src", "contrib", rel),
         sha256 = sha256(dest),
         size = file.info(dest)$size)
  })

  manifest <- paste0(
    "{\n",
    '  "schema": "packyard-bundle/2",\n',
    sprintf('  "snapshot_id": %s,\n', json_str(args$snapshot)),
    sprintf('  "r_version": %s,\n', json_str(args$r_version)),
    sprintf('  "source_url": %s,\n', json_str(args$repo)),
    '  "mode": "full",\n',
    '  "kind": "source",\n',
    sprintf('  "created_at": %s,\n', json_str(format(Sys.time(), "%Y-%m-%dT%H:%M:%SZ", tz = "UTC"))),
    sprintf('  "tool": %s,\n', json_str(sprintf("examples/bundler/s3-cranlike-to-bundle.R (R %s)", getRversion()))),
    '  "packages": [\n', paste(vapply(pkgs, package_json, ""), collapse = ",\n"), "\n  ]\n",
    "}\n")
  writeLines(manifest, file.path(args$out, "manifest.json"), sep = "")
  message(sprintf("[bundler] wrote %d tarballs + manifest.json to %s", length(pkgs), args$out))
}

if (!interactive()) main(parse_args(commandArgs(trailingOnly = TRUE)))
