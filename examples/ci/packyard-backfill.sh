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
# Exits non-zero if any build or attach failed; the rest still ran.

set -euo pipefail

: "${PACKYARD_SERVER:?set PACKYARD_SERVER}"
: "${PACKYARD_TOKEN:?set PACKYARD_TOKEN}"
: "${PACKYARD_CHANNEL:?set PACKYARD_CHANNEL}"
# shellcheck source-path=SCRIPTDIR source=packyard-ci-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/packyard-ci-lib.sh"

CH=$PACKYARD_CHANNEL
QUERY=""
[ -n "${PACKYARD_CELL:-}" ] && QUERY="?cell=$PACKYARD_CELL"
MISSING=$(api "$SERVER/api/v1/channels/$CH/missing-binaries$QUERY")
MINORS=$(api "$SERVER/api/v1/cells" | jq -r '.cells | map({(.name): .r_minor}) | add')
echo "==> $(jq '.missing | length' <<<"$MISSING") binaries missing on $CH"

FAILED=0
while read -r NAME VERSION CELL; do
  MINOR=$(jq -r --arg c "$CELL" '.[$c]' <<<"$MINORS")
  RHOME=$(r_for_minor "$MINOR") || { echo "--- R $MINOR not installed; skipping $NAME $VERSION ($CELL)"; FAILED=1; continue; }
  SRC="$WORK/${NAME}_${VERSION}.tar.gz"
  if [ ! -f "$SRC" ]; then
    api -o "$SRC" "$SERVER/$CH/src/contrib/${NAME}_${VERSION}.tar.gz"
    mkdir -p "$WORK/src-$NAME-$VERSION" && tar -xzf "$SRC" -C "$WORK/src-$NAME-$VERSION"
  fi
  echo "--- $NAME $VERSION for $CELL with $RHOME"
  if BIN=$(build_binary "$WORK/src-$NAME-$VERSION/$NAME" "$SRC" "$RHOME" "$WORK/bin-$NAME-$CELL" "$WORK/lib-$MINOR") &&
     api -X POST -F "binary=@$BIN;type=application/gzip" \
       "$SERVER/api/v1/packages/$CH/$NAME/$VERSION/binaries/$CELL" >/dev/null; then
    echo "    attached"
  else
    echo "    failed:"; tail -n 20 "$WORK/bin-$NAME-$CELL/build.log" 2>/dev/null || true
    FAILED=1
  fi
done < <(jq -r '.missing[] | "\(.name) \(.version) \(.cell)"' <<<"$MISSING")

exit "$FAILED"
