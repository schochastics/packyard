# Generates the golden archive.rds fixtures for internal/rds tests.
#
#   Rscript internal/rds/testdata/gen-archive.R
#
# The objects mirror what packyard serves as src/contrib/Meta/archive.rds:
# CRAN's layout (a named list of file.info()-shaped data frames), with
# packyard's fixed mode/uid/gid/owner values. Written uncompressed so
# the Go test can compare bytes directly; the 14-byte header carries
# the writer's R version and is skipped by the test.

frame <- function(files, sizes, times) {
  n <- length(files)
  t <- structure(as.numeric(times), class = c("POSIXct", "POSIXt"))
  structure(
    list(
      size   = as.numeric(sizes),
      isdir  = rep(FALSE, n),
      mode   = structure(rep(420L, n), class = "octmode"),
      mtime  = t, ctime = t, atime = t,
      uid    = rep(0L, n),
      gid    = rep(0L, n),
      uname  = rep("packyard", n),
      grname = rep("packyard", n)
    ),
    class = "data.frame",
    row.names = files
  )
}

archive <- list(
  alpha = frame(c("alpha/alpha_1.0.0.tar.gz", "alpha/alpha_1.1.0.tar.gz"),
                c(100, 200), c(1700000000, 1700086400.5)),
  beta  = frame("beta/beta_0.1.tar.gz", 50, 1600000000)
)

dir <- "internal/rds/testdata"
saveRDS(archive, file.path(dir, "archive.rds"), version = 2, compress = FALSE)
saveRDS(setNames(list(), character(0)), file.path(dir, "archive-empty.rds"),
        version = 2, compress = FALSE)
