#!/usr/bin/env bash
# packyard-backfill.sh — build and attach the binaries a channel is
# missing for the current version of each package.
#
# Run after adding an R version to matrix.yaml, or on a schedule to
# retry builds that failed during publish. Same image requirements as
# packyard-publish.sh (every R version under /opt/R/<version>/).
#
# Environment:
#   PACKYARD_SERVER   base URL (required)
#   PACKYARD_TOKEN    token with publish:<channel> — plus read:<channel>
#                     unless the channel sets anonymous_reads (required)
#   PACKYARD_CHANNEL  channel to backfill (required)
#   PACKYARD_CELL     restrict to one cell (optional)
#   PACKYARD_R_ROOT / PACKYARD_DEPS_CMD  as for packyard-publish.sh
#
# Exits non-zero if any download, build or attach failed; the rest
# still ran.

set -euo pipefail

: "${PACKYARD_SERVER:?set PACKYARD_SERVER}"
: "${PACKYARD_TOKEN:?set PACKYARD_TOKEN}"
: "${PACKYARD_CHANNEL:?set PACKYARD_CHANNEL}"
# shellcheck source-path=SCRIPTDIR source=packyard-ci-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/packyard-ci-lib.sh"

CH=$PACKYARD_CHANNEL
QUERY=""
[ -n "${PACKYARD_CELL:-}" ] && QUERY="?cell=$PACKYARD_CELL"
# fetch URL — GET an API document; on failure print the server's error
# body to stderr and exit (a bare VAR=$(api …) under set -e would exit
# before the body is shown).
fetch() {
  local out
  if ! out=$(api "$1"); then
    { echo "fetching $1 failed:"; printf '%s\n' "$out"; } >&2
    exit 1
  fi
  printf '%s\n' "$out"
}
MISSING=$(fetch "$SERVER/api/v1/channels/$CH/missing-binaries$QUERY") || exit 1
CELLS_JSON=$(fetch "$SERVER/api/v1/cells") || exit 1
MINORS=$(jq -r '.cells | map({(.name): .r_minor}) | add' <<<"$CELLS_JSON")
echo "==> $(jq '.missing | length' <<<"$MISSING") binaries missing on $CH"

# Every per-package failure is logged, counted and skipped; the loop
# body never lets set -e abort the run.
FAILED=0
while read -r NAME VERSION CELL; do
  MINOR=$(jq -r --arg c "$CELL" '.[$c] // empty' <<<"$MINORS")
  if [ -z "$MINOR" ]; then
    echo "--- cell $CELL unknown to the server; skipping $NAME $VERSION"; FAILED=$((FAILED + 1)); continue
  fi
  RHOME=$(r_for_minor "$MINOR") || { echo "--- R $MINOR not installed; skipping $NAME $VERSION ($CELL)"; FAILED=$((FAILED + 1)); continue; }
  SRC="$WORK/${NAME}_${VERSION}.tar.gz"
  SRCDIR="$WORK/src-$NAME-$VERSION"
  if [ ! -d "$SRCDIR" ]; then
    # With --fail-with-body the error body lands in $SRC; show it.
    if ! api -o "$SRC" "$SERVER/$CH/src/contrib/${NAME}_${VERSION}.tar.gz"; then
      echo "--- downloading $NAME $VERSION failed; skipping ($CELL):"
      head -c 2000 "$SRC" 2>/dev/null && echo
      rm -f "$SRC"
      FAILED=$((FAILED + 1))
      continue
    fi
    if ! { mkdir -p "$SRCDIR.tmp" && tar -xzf "$SRC" -C "$SRCDIR.tmp" && mv "$SRCDIR.tmp" "$SRCDIR"; }; then
      echo "--- extracting $NAME $VERSION failed; skipping ($CELL)"
      rm -rf "$SRC" "$SRCDIR.tmp"
      FAILED=$((FAILED + 1))
      continue
    fi
  fi
  echo "--- $NAME $VERSION for $CELL with $RHOME"
  if ! BIN=$(build_binary "$SRCDIR/$NAME" "$SRC" "$RHOME" "$WORK/bin-$NAME-$CELL" "$WORK/lib-$MINOR"); then
    echo "    build failed:"; tail -n 20 "$WORK/bin-$NAME-$CELL/build.log" 2>/dev/null || true
    FAILED=$((FAILED + 1))
  elif ! RESP=$(api -X POST -F "binary=@$BIN;type=application/gzip" \
         "$SERVER/api/v1/packages/$CH/$NAME/$VERSION/binaries/$CELL"); then
    echo "    attach failed:"; printf '    %s\n' "$RESP"
    FAILED=$((FAILED + 1))
  else
    echo "    attached"
  fi
done < <(jq -r '.missing[] | "\(.name) \(.version) \(.cell)"' <<<"$MISSING")

if [ "$FAILED" -gt 0 ]; then
  echo "==> $FAILED binaries failed on $CH" >&2
  exit 1
fi
