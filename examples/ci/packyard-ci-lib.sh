# packyard-ci-lib.sh — helpers shared by packyard-publish.sh and
# packyard-backfill.sh. Sourced, not executed.

# shellcheck shell=bash
R_ROOT="${PACKYARD_R_ROOT:-/opt/R}"
# shellcheck disable=SC2034 # used by the scripts that source this file
SERVER="${PACKYARD_SERVER%/}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

api() { curl --fail-with-body -sS -H "Authorization: Bearer $PACKYARD_TOKEN" "$@"; }

# r_for_minor 4.4 → path of the newest installed R 4.4.x, or nothing.
r_for_minor() {
  local best
  best=$(for dir in "$R_ROOT/$1".*/; do
    [ -x "${dir}bin/R" ] && echo "${dir%/}"
  done | sort -V | tail -n1)
  [ -n "$best" ] && echo "$best"
}

# install_deps RSCRIPT LIB — install the DESCRIPTION's hard dependencies
# (Depends, Imports, LinkingTo) of the package in the current directory
# into LIB, from getOption("repos"). PACKYARD_DEPS_CMD overrides it.
install_deps() {
  local rscript=$1 lib=$2
  if [ -n "${PACKYARD_DEPS_CMD:-}" ]; then
    RSCRIPT="$rscript" R_LIBS="$lib" bash -c "$PACKYARD_DEPS_CMD"
    return
  fi
  R_LIBS="$lib" "$rscript" -e '
    d <- read.dcf("DESCRIPTION", fields = c("Depends", "Imports", "LinkingTo"))
    deps <- trimws(sub("\\(.*", "", unlist(strsplit(stats::na.omit(as.vector(d)), ","))))
    deps <- setdiff(deps[nzchar(deps)], c("R", rownames(installed.packages(priority = "base"))))
    missing <- deps[!deps %in% rownames(installed.packages())]
    if (length(missing)) install.packages(missing, lib = .libPaths()[1])
  '
}

# build_binary PKGDIR SOURCE RHOME OUTDIR LIB — R CMD INSTALL --build the
# source tarball with RHOME's R; prints the built tarball's path.
build_binary() {
  local pkgdir=$1 source=$2 rhome=$3 out=$4 lib=$5
  mkdir -p "$out" "$lib"
  (cd "$pkgdir" && install_deps "$rhome/bin/Rscript" "$lib") >"$out/build.log" 2>&1 &&
    (cd "$out" && R_LIBS="$lib" "$rhome/bin/R" CMD INSTALL --build "$source" >>build.log 2>&1) &&
    ls "$out"/*_R_*.tar.gz
}
