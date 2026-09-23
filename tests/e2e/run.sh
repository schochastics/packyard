#!/usr/bin/env bash
# run.sh — end-to-end suite: a live packyard container, real R clients.
#
#   tests/e2e/run.sh [distro ...]     # default: jammy rhel9
#
# Per distro it builds a client image (Posit R 4.4 and 4.5 under
# /opt/R), boots packyard with a matrix.yaml for that distro, and runs
# scenarios.sh inside the client, which publishes testpkg through the
# reference CI scripts in examples/ci and then installs it the ways R
# users do. Needs only Docker. Server logs and the User-Agent samples
# the clients sent land in tests/e2e/out/<distro>/.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E2E="$ROOT/tests/e2e"
OUT="$E2E/out"
DISTROS=("$@")
[ ${#DISTROS[@]} -gt 0 ] || DISTROS=(jammy rhel9)

echo "==> building packyard:e2e"
docker build -q -t packyard:e2e "$ROOT" >/dev/null

run_distro() {
  local distro=$1 cfg
  local name="packyard-e2e-$distro"
  local out="$OUT/$distro"
  mkdir -p "$out"
  cfg=$(mktemp -d)

  cleanup() {
    docker logs "$name" >"$out/server.log" 2>&1 || true
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker volume rm "$name" >/dev/null 2>&1 || true
    docker network rm "$name" >/dev/null 2>&1 || true
    rm -rf "$cfg"
  }
  trap cleanup RETURN

  echo "==> [$distro] building client image"
  docker build -q -t "packyard-e2e-client:$distro" \
    -f "$E2E/client/$distro.Dockerfile" "$E2E/client" >/dev/null

  # The platform containers actually run as (DOCKER_DEFAULT_PLATFORM
  # may differ from the host), in matrix.yaml's spelling.
  local arch
  arch=$(docker run --rm "packyard-e2e-client:$distro" uname -m)
  case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac

  cat >"$cfg/channels.yaml" <<'EOF'
channels:
  - name: dev
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
    anonymous_reads: true
EOF
  cat >"$cfg/matrix.yaml" <<EOF
distro: $distro
arch: $arch
default_r_minor: "4.4"
cells:
  - name: r-4.4
    r_minor: "4.4"
  - name: r-4.5
    r_minor: "4.5"
EOF

  docker network create "$name" >/dev/null
  docker volume create "$name" >/dev/null
  # distroless runs as nonroot (65532); seed the volume as root first.
  docker run --rm -v "$name:/data" -v "$cfg:/cfg:ro" busybox:1.36 \
    sh -c 'cp /cfg/*.yaml /data/ && chown -R 65532:65532 /data'
  docker run -d --name "$name" --network "$name" --network-alias packyard \
    -v "$name:/data" packyard:e2e -data /data >/dev/null

  local ok=
  for _ in $(seq 1 30); do
    if docker run --rm --network "$name" "packyard-e2e-client:$distro" \
      curl -sf -o /dev/null http://packyard:8080/health; then
      ok=1
      break
    fi
    sleep 1
  done
  [ -n "$ok" ] || { echo "[$distro] /health never returned 200" >&2; return 1; }

  local token
  token=$(docker exec "$name" packyard-server -mint-token -data /data \
    -scopes 'publish:*,read:*,yank:*' -label e2e 2>/dev/null)

  echo "==> [$distro] running scenarios"
  local status=0
  docker run --rm --network "$name" \
    -e E2E_DISTRO="$distro" -e PACKYARD_TOKEN="$token" \
    -v "$E2E:/e2e:ro" -v "$ROOT/examples/ci:/ci:ro" \
    "packyard-e2e-client:$distro" bash /e2e/scenarios.sh || status=$?

  docker logs "$name" 2>&1 |
    sed -n 's/.*"user_agent":"\([^"]*\)".*/\1/p' |
    sort -u >"$out/user-agents.txt"
  return "$status"
}

failed=()
for d in "${DISTROS[@]}"; do
  run_distro "$d" || failed+=("$d")
done
if [ ${#failed[@]} -gt 0 ]; then
  echo "==> e2e FAILED for: ${failed[*]} (logs in $OUT/)" >&2
  exit 1
fi
echo "==> e2e passed for: ${DISTROS[*]}"
