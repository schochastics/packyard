#!/usr/bin/env bash
# Install the client-side tools the scenarios drive (remotes, renv,
# pak) into every R's own library. All come from public repos at
# image build time; the scenarios themselves only talk to packyard
# for testpkg.
set -euo pipefail

for rhome in /opt/R/*/; do
  "${rhome}bin/Rscript" -e '
    options(repos = c(CRAN = "https://cloud.r-project.org"), Ncpus = 4)
    install.packages(c("remotes", "renv"), lib = .Library)
    install.packages("pak", lib = .Library, repos = sprintf(
      "https://r-lib.github.io/p/pak/stable/%s/%s/%s",
      .Platform$pkgType, R.Version()$os, R.Version()$arch))
    for (p in c("remotes", "renv", "pak")) library(p, character.only = TRUE)
  '
done
