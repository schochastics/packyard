# 5-minute quickstart

Zero to "R installs your first internal package from pakman" in roughly
five copy-pasteable commands. Pick the install path that fits:

- **[Docker](#docker)** — one `docker run` and you're done; recommended
  once release images are published.
- **[From source](#from-source)** — clone, `make build`, run the
  binary. Right for kicking the tyres before the first tagged release.

Either path needs `curl`, `jq`, and (for step 4) `R` on the local
machine.

## Docker

> The official `ghcr.io/schochastics/pakman` image ships with the first
> tagged release. Until then, use [From source](#from-source).

### 1. Start pakman

```sh
docker run --rm -d --name pakman \
  -p 8080:8080 \
  -v pakman-data:/data \
  ghcr.io/schochastics/pakman:latest
```

Pakman runs in the foreground of the container with WAL-mode SQLite at
`/data/db.sqlite` and a content-addressed blob store under `/data/cas/`.
Default channels `dev` / `test` / `prod` are created on first start; see
[config.md](config.md) to change them.

### 2. Mint an admin token

```sh
ADMIN=$(docker exec pakman pakman-server \
  -mint-token -data /data -scopes admin -label bootstrap 2>/dev/null)
echo "$ADMIN"
```

(Skip to [step 3](#3-mint-a-publish-token-for-ci).)

## From source

Needs Go 1.22+ installed. Everything runs under a throwaway `./tmpdata/`
dir so it won't collide with an existing pakman install.

### 1. Build and start pakman

```sh
git clone https://github.com/schochastics/pakman.git
cd pakman
make build

# Bootstrap the data dir (creates db.sqlite, cas/, default configs).
./pakman-server -init -data ./tmpdata

# Start the server in the background.
./pakman-server -data ./tmpdata &
SERVER_PID=$!
sleep 0.5
```

Kill the server with `kill $SERVER_PID` when you're done, and
`rm -rf ./tmpdata` to wipe the throwaway state.

### 2. Mint an admin token

```sh
ADMIN=$(./pakman-server -mint-token -data ./tmpdata \
  -scopes admin -label bootstrap 2>/dev/null)
echo "$ADMIN"
```

Admin tokens are only printed once, so stash this somewhere safe for the
rest of the session. `admin` is the universal scope; for CI you'll want
narrower tokens (see next step).

## 3. Mint a publish token for CI

```sh
PUB=$(curl -s -X POST http://localhost:8080/api/v1/admin/tokens \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"label":"ci","scopes":["publish:*","read:*"]}' | jq -r .token)
echo "$PUB"
```

This is the token you'd drop into the [`examples/ci/publish.yml`](../examples/ci/publish.yml)
workflow. `publish:*` lets it publish to any channel; narrow to
`publish:dev` / `publish:prod` as your channel model demands.

## 4. Scaffold and publish a package

```sh
# A throwaway source package with nothing but a DESCRIPTION. R CMD build
# accepts it — production packages of course have code too.
mkdir -p /tmp/mypkg && cd /tmp/mypkg
cat > DESCRIPTION <<EOF
Package: mypkg
Type: Package
Title: Example pakman package
Version: 1.0.0
Description: Smoke-test package for the pakman quickstart.
License: MIT
EOF
R CMD build .

# Publish the resulting mypkg_1.0.0.tar.gz to the prod channel.
curl --fail-with-body -X POST \
  http://localhost:8080/api/v1/packages/prod/mypkg/1.0.0 \
  -H "Authorization: Bearer $PUB" \
  -F 'manifest={"source":"source"};type=application/json' \
  -F "source=@mypkg_1.0.0.tar.gz;type=application/gzip"
```

Response is JSON with the stored `source_sha256`, `source_size`, and a
`created: true` flag. The publish also writes an entry in the audit log
(`GET /api/v1/events`).

## 5. Install from R

```r
options(repos = c(PAKMAN = "http://localhost:8080/", getOption("repos")))
install.packages("mypkg")
```

The default-channel alias serves `prod` at the root, so no channel segment
needed. For non-default channels use `http://localhost:8080/dev/`.

## See it in the dashboard

Point a browser at `http://localhost:8080/ui/`, paste `$ADMIN` into the
login form, and the dashboard shows:

- Three totals: channels, packages, events.
- A card for each channel; `prod` now reports 1 package.
- A recent-activity row for your publish.

Full dashboard pages: `/ui/channels/{name}`, `/ui/events`, `/ui/cells`,
`/ui/storage`.

## What you just did

Five endpoints, in this order:

1. `pakman-server -mint-token` — bootstrap the first admin token.
2. `POST /api/v1/admin/tokens` — mint narrow-scope tokens for CI/humans.
3. `POST /api/v1/packages/{channel}/{name}/{version}` — publish.
4. `GET /{channel}/src/contrib/PACKAGES` (via `install.packages()`) —
   CRAN-protocol read.
5. `GET /` from `/ui/` — operator dashboard.

The real CI flow replaces step 4 with a multi-job workflow that builds
per-cell binaries; see [examples/ci/README.md](../examples/ci/README.md).
Everything else stays the same.

## Troubleshooting

- **403 on publish** — token's scope list doesn't include
  `publish:<channel>`. Use `admin` for testing; narrow afterwards.
- **409 on republish** — channel is immutable and the version already
  exists with different bytes. Bump `Version:` in DESCRIPTION, or
  publish to a mutable channel (default: `dev`, `test`).
- **Can't reach `http://localhost:8080/`** — on Linux with rootless
  Docker, the `-p 8080:8080` mapping may need `--publish=host`. Check
  `docker ps` shows the port mapped, and `curl http://localhost:8080/health`.
- **`address already in use`** — a previous pakman server is still
  bound to 8080. `pkill -f pakman-server` (source install) or
  `docker rm -f pakman` (Docker) before retrying.
- **No R on this machine** — `R CMD build` in step 4 needs an R
  install. If you're only smoke-testing the publish endpoint, swap the
  scaffold step for any existing `.tar.gz`; the server takes arbitrary
  bytes and doesn't parse R tarballs.
- **`jq` not installed** — the `tok=$(... | jq -r .token)` step needs
  jq. Install it or grep the JSON manually.

## Next steps

- [api.md](api.md) — full HTTP reference with curl examples.
- [config.md](config.md) — channels, matrix, server config YAMLs.
- [admin.md](admin.md) — CLI admin commands (import, gc, reindex).
- [migration.md](migration.md) — moving from drat or git to pakman.
