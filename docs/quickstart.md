# 5-minute quickstart

Zero to "R installs your first internal package from packyard" in roughly
five copy-pasteable commands. Pick the install path that fits:

- **[Docker](#docker)** — one `docker run` and you're done; recommended
  once release images are published.
- **[From source](#from-source)** — clone, `make build`, run the
  binary. Right for kicking the tyres before the first tagged release.

Either path needs `curl`, `jq`, and (for step 4) `R` on the local
machine.

## Docker

> The image is `ghcr.io/schochastics/packyard`. If you can't pull it
> (for example while the package is private), use
> [From source](#from-source).

### 1. Start packyard

```sh
# Initialise the data volume (default configs, DB, blob store).
docker run --rm -v packyard-data:/data ghcr.io/schochastics/packyard:latest -init -data /data

# Let R read the prod channel without a token (step 5).
docker run --rm -i -v packyard-data:/data busybox sh -c 'cat > /data/channels.yaml' <<'EOF'
channels:
  - { name: dev,  overwrite_policy: mutable }
  - { name: test, overwrite_policy: mutable }
  - { name: prod, overwrite_policy: immutable, default: true, anonymous_reads: true }
EOF

docker run --rm -d --name packyard \
  -p 8080:8080 \
  -v packyard-data:/data \
  ghcr.io/schochastics/packyard:latest \
  -data /data
```

Packyard runs in the foreground of the container with WAL-mode SQLite at
`/data/db.sqlite` and a content-addressed blob store under `/data/cas/`.
See [config.md](config.md) for the channel and matrix settings.

`anonymous_reads: true` opens `prod`'s CRAN-protocol reads to
unauthenticated clients, so R's `install.packages()` works without a
bearer token. `dev` and `test` stay scoped, and so does the JSON API.

### 2. Mint an admin token

```sh
ADMIN=$(docker exec packyard packyard-server \
  -mint-token -data /data -scopes admin -label bootstrap 2>/dev/null)
echo "$ADMIN"
```

(Skip to [step 3](#3-mint-a-publish-token-for-ci).)

## From source

Needs Go 1.25+ installed. Everything runs under a throwaway `./tmpdata/`
dir so it won't collide with an existing packyard install.

### 1. Build and start packyard

```sh
git clone https://github.com/schochastics/packyard.git
cd packyard
make build

# Let R read the prod channel without a token (step 5).
mkdir -p tmpdata
cat > tmpdata/channels.yaml <<'EOF'
channels:
  - { name: dev,  overwrite_policy: mutable }
  - { name: test, overwrite_policy: mutable }
  - { name: prod, overwrite_policy: immutable, default: true, anonymous_reads: true }
EOF

# Start the server in the background. The rest of the data dir
# (db.sqlite, cas/, matrix.yaml) is created on first serve.
./packyard-server -data ./tmpdata &
SERVER_PID=$!
sleep 0.5
```

`anonymous_reads: true` is what makes step 5 (R `install.packages()`)
work without a token. It opens `prod`'s CRAN-protocol reads only;
`dev`, `test` and the JSON API stay scoped. See
[config.md](config.md).

Kill the server with `kill $SERVER_PID` when you're done, and
`rm -rf ./tmpdata` to wipe the throwaway state.

### 2. Mint an admin token

```sh
ADMIN=$(./packyard-server -mint-token -data ./tmpdata \
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

This is the token you'd give CI (`PACKYARD_TOKEN` for the scripts in
[examples/ci/](../examples/ci/)). `publish:*` lets it publish to any channel; narrow to
`publish:dev` / `publish:prod` as your channel model demands.

## 4. Scaffold and publish a package

```sh
# A throwaway source package with nothing but a DESCRIPTION. R CMD build
# accepts it — production packages of course have code too.
mkdir -p /tmp/mypkg && cd /tmp/mypkg
cat > DESCRIPTION <<EOF
Package: mypkg
Type: Package
Title: Example packyard package
Version: 1.0.0
Description: Smoke-test package for the packyard quickstart.
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
options(repos = c(PACKYARD = "http://localhost:8080/", getOption("repos")))
install.packages("mypkg")
```

The default-channel alias serves `prod` at the root, so no channel segment
needed. For non-default channels use `http://localhost:8080/dev/`.

Once CI publishes binaries, point R at the binary URL for the server's
distro instead (`distro` in `matrix.yaml`, `jammy` by default):
`http://localhost:8080/prod/__linux__/jammy/latest`. Packages without
a binary for the client's R version are served as source from there,
too.

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

1. `packyard-server -mint-token` — bootstrap the first admin token.
2. `POST /api/v1/admin/tokens` — mint narrow-scope tokens for CI/humans.
3. `POST /api/v1/packages/{channel}/{name}/{version}` — publish.
4. `GET /{channel}/src/contrib/PACKAGES` (via `install.packages()`) —
   CRAN-protocol read.
5. `GET /` from `/ui/` — operator dashboard.

The real CI flow replaces step 4 with
[examples/ci/packyard-publish.sh](../examples/ci/packyard-publish.sh),
which builds the source plus one binary per R version in `matrix.yaml`
and publishes them in one request; see
[examples/ci/README.md](../examples/ci/README.md). Everything else
stays the same.

## Troubleshooting

- **403 on publish** — token's scope list doesn't include
  `publish:<channel>`. Use `admin` for testing; narrow afterwards.
- **409 on republish** — channel is immutable and the version already
  exists with different bytes. Bump `Version:` in DESCRIPTION, or
  publish to a mutable channel (default: `dev`, `test`).
- **R says `cannot open URL '…/src/contrib/PACKAGES'`**: R sends no
  bearer token, and the channel doesn't allow anonymous reads (401).
  Set `anonymous_reads: true` on it in `channels.yaml` and restart.
- **Can't reach `http://localhost:8080/`** — on Linux with rootless
  Docker, the `-p 8080:8080` mapping may need `--publish=host`. Check
  `docker ps` shows the port mapped, and `curl http://localhost:8080/health`.
- **`address already in use`** — a previous packyard server is still
  bound to 8080. `pkill -f packyard-server` (source install) or
  `docker rm -f packyard` (Docker) before retrying.
- **No R on this machine** — `R CMD build` in step 4 needs an R
  install. If you're only smoke-testing the publish endpoint, swap the
  scaffold step for any existing `.tar.gz`. The server accepts it, but
  it reads `DESCRIPTION` from the tarball for the dependency fields in
  `PACKAGES`; without a real one, the entry only has `Package` and
  `Version`, and the log shows a warning.
- **`jq` not installed** — the `tok=$(... | jq -r .token)` step needs
  jq. Install it or grep the JSON manually.

## Next steps

- [api.md](api.md) — full HTTP reference with curl examples.
- [config.md](config.md) — channels, matrix, server config YAMLs.
- [admin.md](admin.md) — CLI admin commands (import, gc, reindex).
- [migration.md](migration.md) — moving from drat or git to packyard.
