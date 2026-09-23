#!/usr/bin/env bash
# scenarios.sh — runs inside the e2e client container (see run.sh).
#
# Fixture: testpkg 0.1.0, 0.2.0 and 0.3.0 published to prod through
# examples/ci/packyard-publish.sh with only R 4.4 visible, so every
# version gets an r-4.4 binary and none an r-4.5 one; 0.3.0 is then
# yanked. PACKAGES therefore serves 0.2.0, and the archive holds 0.1.0
# and 0.3.0.
set -euo pipefail

: "${E2E_DISTRO:?}" "${PACKYARD_TOKEN:?}"
export PACKYARD_SERVER=http://packyard:8080 PACKYARD_TOKEN
SRC=$PACKYARD_SERVER/prod
LINUX=$PACKYARD_SERVER/prod/__linux__/$E2E_DISTRO/latest
R44=$(printf '%s\n' /opt/R/4.4.* | sort -V | tail -n1)/bin/Rscript
R45=$(printf '%s\n' /opt/R/4.5.* | sort -V | tail -n1)/bin/Rscript
WORK=$(mktemp -d)
LOG=$WORK/log
export SRC LINUX RENV_CONFIG_CACHE_ENABLED=FALSE RENV_PATHS_ROOT=$WORK/renv

# ---- fixture -------------------------------------------------------------
mkdir -p "$WORK/r44"
ln -s "$(dirname "$(dirname "$R44")")" "$WORK/r44/"
for v in 0.1.0 0.2.0 0.3.0; do
  echo "==> publishing testpkg $v"
  cp -r /e2e/testpkg "$WORK/testpkg-$v"
  sed -i "s/^Version: .*/Version: $v/" "$WORK/testpkg-$v/DESCRIPTION"
  (cd "$WORK/testpkg-$v" &&
    PACKYARD_CHANNELS=prod PACKYARD_R_ROOT="$WORK/r44" bash /ci/packyard-publish.sh)
done
curl -fsS -X POST -H "Authorization: Bearer $PACKYARD_TOKEN" \
  -H 'Content-Type: application/json' -d '{"reason":"e2e: yanked newest"}' \
  "$PACKYARD_SERVER/api/v1/packages/prod/testpkg/0.3.0/yank" >/dev/null

# ---- harness -------------------------------------------------------------
failed=()
# scenario NAME RSCRIPT CODE — run CODE with RSCRIPT in a fresh library
# ($LIB); output goes to $LOG for follow-up greps.
scenario() {
  local name=$1 rscript=$2 code=$3
  LIB=$(mktemp -d)
  export LIB
  echo "--- $name"
  if ! (cd "$(mktemp -d)" && "$rscript" -e "$code") >"$LOG" 2>&1; then
    cat "$LOG"
    failed+=("$name")
    return 1
  fi
}
expect_log() {
  local name=$1 pattern=$2
  if ! grep -q -- "$pattern" "$LOG"; then
    cat "$LOG"
    echo "    expected output matching: $pattern"
    failed+=("$name")
  fi
}

# 1. available.packages() on the __linux__ URL: one row, latest non-yanked.
for r in "$R44" "$R45"; do
  scenario "1 available.packages ($r)" "$r" '
    ap <- available.packages(repos = Sys.getenv("LINUX"))
    print(ap[, c("Package", "Version")])
    stopifnot(nrow(ap) == 1, ap[1, "Version"] == "0.2.0")' || true
done

# 2. R 4.4 installs the r-4.4 binary.
scenario "2 R 4.4 installs binary" "$R44" '
  install.packages("testpkg", repos = Sys.getenv("LINUX"), lib = Sys.getenv("LIB"))
  d <- packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))
  cat("Version:", d$Version, "\nBuilt:", d$Built, "\n")
  stopifnot(d$Version == "0.2.0", startsWith(d$Built, "R 4.4"))
  library(testpkg, lib.loc = Sys.getenv("LIB"))
  stopifnot(hello() == "hello from testpkg")' &&
  expect_log "2 R 4.4 installs binary" "installing \*binary\* package"

# 3. R 4.5 has no binary and installs from source.
scenario "3 R 4.5 installs source" "$R45" '
  install.packages("testpkg", repos = Sys.getenv("LINUX"), lib = Sys.getenv("LIB"))
  d <- packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))
  stopifnot(d$Version == "0.2.0", startsWith(d$Built, "R 4.5"))' &&
  expect_log "3 R 4.5 installs source" "installing \*source\* package"

# 4. Meta/archive.rds is readable and lists the archived versions.
for base in "$SRC" "$LINUX"; do
  BASE=$base scenario "4 readRDS archive.rds ($base)" "$R44" '
    con <- gzcon(url(paste0(Sys.getenv("BASE"), "/src/contrib/Meta/archive.rds")))
    a <- readRDS(con)
    str(a)
    stopifnot(identical(names(a), "testpkg"), is.data.frame(a$testpkg),
      identical(rownames(a$testpkg),
        c("testpkg/testpkg_0.1.0.tar.gz", "testpkg/testpkg_0.3.0.tar.gz")),
      all(a$testpkg$size > 0), inherits(a$testpkg$mtime, "POSIXct"))' || true
done

# 5. remotes::install_version() of an archived version.
for base in "$SRC" "$LINUX"; do
  BASE=$base scenario "5 remotes::install_version 0.1.0 ($base)" "$R44" '
    remotes::install_version("testpkg", "0.1.0", repos = Sys.getenv("BASE"),
      lib = Sys.getenv("LIB"), upgrade = "never")
    stopifnot(packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))$Version == "0.1.0")' || true
done

# 6. renv::restore() of a lockfile pinning an archived version.
for base in "$SRC" "$LINUX"; do
  LOCK=$(mktemp)
  cat >"$LOCK" <<EOF
{
  "R": {"Version": "$("$R44" -s -e 'cat(format(getRversion()))')",
        "Repositories": [{"Name": "packyard", "URL": "$base"}]},
  "Packages": {"testpkg": {"Package": "testpkg", "Version": "0.1.0",
                           "Source": "Repository", "Repository": "packyard"}}
}
EOF
  LOCK=$LOCK BASE=$base scenario "6 renv::restore 0.1.0 ($base)" "$R44" '
    options(repos = c(packyard = Sys.getenv("BASE")))
    renv::restore(lockfile = Sys.getenv("LOCK"), library = Sys.getenv("LIB"), prompt = FALSE)
    stopifnot(packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))$Version == "0.1.0")' || true
done

# 7. pak. Its "pkg@version" resolves archived versions for CRAN only
# (through CRAN's metadata service); against any other repository it
# silently installs the current version. So pak users pin archived
# internal versions with a url:: ref to Archive/.
scenario "7 pak::pkg_install current + url:: pin" "$R44" '
  options(repos = c(packyard = Sys.getenv("SRC")))
  pak::pkg_install("testpkg", lib = Sys.getenv("LIB"), ask = FALSE)
  stopifnot(packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))$Version == "0.2.0")
  pak::pkg_install(paste0("url::", Sys.getenv("SRC"),
    "/src/contrib/Archive/testpkg/testpkg_0.1.0.tar.gz"), lib = Sys.getenv("LIB"), ask = FALSE)
  stopifnot(packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))$Version == "0.1.0")' || true

# 8. The yanked version still installs when pinned exactly.
scenario "8 yanked 0.3.0 by exact pin" "$R44" '
  remotes::install_version("testpkg", "0.3.0", repos = Sys.getenv("SRC"),
    lib = Sys.getenv("LIB"), upgrade = "never")
  stopifnot(packageDescription("testpkg", lib.loc = Sys.getenv("LIB"))$Version == "0.3.0")
  install.packages(paste0(Sys.getenv("SRC"), "/src/contrib/testpkg_0.3.0.tar.gz"),
    repos = NULL, lib = Sys.getenv("LIB"))' || true

# 9. The wrong distro fails visibly instead of serving foreign binaries.
scenario "9 wrong distro" "$R44" '
  msgs <- character()
  ap <- withCallingHandlers(
    available.packages(repos = sub("/__linux__/[^/]+/", "/__linux__/wrong/", Sys.getenv("LINUX"))),
    warning = function(w) { msgs <<- c(msgs, conditionMessage(w)); invokeRestart("muffleWarning") })
  print(msgs)
  stopifnot(nrow(ap) == 0, length(msgs) > 0)' || true
code=$(curl -s -o "$LOG" -w '%{http_code}' "$PACKYARD_SERVER/prod/__linux__/wrong/latest/src/contrib/PACKAGES")
if [ "$code" != 404 ] || ! grep -q "$E2E_DISTRO" "$LOG"; then
  echo "--- 9 wrong distro (curl): got $code"; cat "$LOG"; failed+=("9 wrong distro (curl)")
fi

# 10. Channels without anonymous_reads refuse anonymous clients.
code=$(curl -s -o /dev/null -w '%{http_code}' "$PACKYARD_SERVER/dev/src/contrib/PACKAGES")
echo "--- 10 dev without token → $code"
[ "$code" = 401 ] || failed+=("10 anonymous read on dev")

# 11. Backfilling r-4.5 turns the R 4.5 install into a binary install.
echo "--- 11 backfill r-4.5"
if PACKYARD_CHANNEL=prod PACKYARD_CELL=r-4.5 bash /ci/packyard-backfill.sh >"$LOG" 2>&1; then
  scenario "11 R 4.5 installs backfilled binary" "$R45" '
    install.packages("testpkg", repos = Sys.getenv("LINUX"), lib = Sys.getenv("LIB"))' &&
    expect_log "11 R 4.5 installs backfilled binary" "installing \*binary\* package"
else
  cat "$LOG"; failed+=("11 backfill")
fi

# 12. Migration export: examples/bundler/s3-cranlike-to-bundle.R turns
# prod (the same layout as an S3 CRAN-like repo) into a source bundle;
# run.sh imports it into dev. R 4.5 for tools::sha256sum.
echo "--- 12 s3-cranlike-to-bundle"
rm -rf /out/bundle
if ! "$R45" /bundler/s3-cranlike-to-bundle.R --repo "$SRC" --out /out/bundle >"$LOG" 2>&1 ||
  ! grep -q "wrote 3 tarballs" "$LOG"; then
  cat "$LOG"; failed+=("12 s3-cranlike-to-bundle")
fi

if [ ${#failed[@]} -gt 0 ]; then
  printf '==> [%s] FAILED:\n' "$E2E_DISTRO"; printf '    %s\n' "${failed[@]}"
  exit 1
fi
echo "==> [$E2E_DISTRO] all scenarios passed"
