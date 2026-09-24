#!/usr/bin/env bash
# packyard-publish.sh — build an R package for every R version the
# server has a cell for, then publish source + binaries.
#
# Run it from the package directory inside an image that has every R
# version installed under /opt/R/<version>/ (the Posit / rig layout
# used by Workbench and Connect images) on the same Linux distribution
# the server serves. CI-vendor neutral: needs bash, curl and jq.
#
# Environment:
#   PACKYARD_SERVER   base URL, e.g. https://packages.example.org (required)
#   PACKYARD_TOKEN    token with publish:<channel> on every target (required)
#   PACKYARD_CHANNELS space-separated target channels, e.g. "prod test dev" (required)
#   PACKYARD_R_ROOT   where R versions live (default /opt/R)
#   PACKYARD_DEPS_CMD command run with each R's Rscript to install build
#                     dependencies; "$RSCRIPT" is set (default: base-R
#                     install of Depends/Imports/LinkingTo from
#                     getOption("repos") into a per-R-version library)
#
# Exit status is non-zero when the source build, the default R
# version's binary build, or any publish fails. Other failed binary
# builds are reported and left for packyard-backfill.sh.

set -euo pipefail

: "${PACKYARD_SERVER:?set PACKYARD_SERVER}"
: "${PACKYARD_TOKEN:?set PACKYARD_TOKEN}"
: "${PACKYARD_CHANNELS:?set PACKYARD_CHANNELS}"
# shellcheck source-path=SCRIPTDIR source=packyard-ci-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/packyard-ci-lib.sh"

PKG=$(awk -F': *' '/^Package:/ {print $2}' DESCRIPTION)
VER=$(awk -F': *' '/^Version:/ {print $2}' DESCRIPTION)
# api failures are caught explicitly: under set -e a bare RESP=$(api …)
# would exit before the server's error body (message + hint) is shown.
if ! CELLS_JSON=$(api "$SERVER/api/v1/cells"); then
  { echo "fetching $SERVER/api/v1/cells failed:"; printf '%s\n' "$CELLS_JSON"; } >&2
  exit 1
fi
DEFAULT_MINOR=$(jq -r .default_r_minor <<<"$CELLS_JSON")
echo "==> $PKG $VER; server serves $(jq -r .distro <<<"$CELLS_JSON"), R $(jq -r '[.cells[].r_minor] | join(", ")' <<<"$CELLS_JSON")"

# ---- source ------------------------------------------------------------
SRC_R=$(r_for_minor "$DEFAULT_MINOR") || { echo "no R $DEFAULT_MINOR under $R_ROOT" >&2; exit 1; }
mkdir -p "$WORK/lib-$DEFAULT_MINOR"
install_deps "$SRC_R/bin/Rscript" "$WORK/lib-$DEFAULT_MINOR"
R_LIBS="$WORK/lib-$DEFAULT_MINOR" "$SRC_R/bin/R" CMD build --no-manual --no-build-vignettes . >/dev/null
SOURCE="$PWD/${PKG}_${VER}.tar.gz"
[ -f "$SOURCE" ] || { echo "R CMD build produced no $SOURCE" >&2; exit 1; }

# ---- binaries ----------------------------------------------------------
MANIFEST='{"source":"source","binaries":[]}'
FILES=(-F "source=@$SOURCE;type=application/gzip")
FAILED=()
while read -r CELL MINOR; do
  RHOME=$(r_for_minor "$MINOR") || { echo "--- R $MINOR not installed; skipping cell $CELL"; FAILED+=("$CELL"); continue; }
  echo "--- building $CELL with $RHOME"
  if BIN=$(build_binary "$PWD" "$SOURCE" "$RHOME" "$WORK/bin-$CELL" "$WORK/lib-$MINOR"); then
    PART="bin_${CELL//[^A-Za-z0-9]/_}"
    MANIFEST=$(jq --arg c "$CELL" --arg p "$PART" '.binaries += [{cell: $c, part: $p}]' <<<"$MANIFEST")
    FILES+=(-F "$PART=@$BIN;type=application/gzip")
  else
    echo "--- build for $CELL failed:"; tail -n 20 "$WORK/bin-$CELL/build.log" || true
    FAILED+=("$CELL")
    if [ "$MINOR" = "$DEFAULT_MINOR" ]; then
      echo "default R $DEFAULT_MINOR build failed" >&2; exit 1
    fi
  fi
done < <(jq -r '.cells[] | "\(.name) \(.r_minor)"' <<<"$CELLS_JSON")

echo "$MANIFEST" >"$WORK/manifest.json"

# ---- publish -----------------------------------------------------------
for CHANNEL in $PACKYARD_CHANNELS; do
  echo "==> publishing to $CHANNEL"
  if ! RESP=$(api -X POST -F "manifest=@$WORK/manifest.json;type=application/json" "${FILES[@]}" \
    "$SERVER/api/v1/packages/$CHANNEL/$PKG/$VER"); then
    { echo "publish of $PKG $VER to $CHANNEL failed:"; printf '%s\n' "$RESP"; } >&2
    exit 1
  fi
  jq -r '"    \(if .already_existed then "unchanged" elif .overwritten then "overwritten" else "created" end); missing cells: \(.missing_cells | if length == 0 then "none" else join(", ") end)"' <<<"$RESP"
done

if [ ${#FAILED[@]} -gt 0 ]; then
  echo "==> binaries not built for: ${FAILED[*]} (clients on those R versions get source; see packyard-backfill.sh)"
fi
