# Docker Compose example

A single-service Compose file that brings up packyard on `:8080` with
a persistent named volume. Intended as the one-liner path for
evaluation, a home lab, or a small team that doesn't want to write a
systemd unit.

```sh
cd examples/compose
docker compose up -d
```

## What you get

- Image `ghcr.io/schochastics/packyard:latest` pulled on first run.
- Data persisted in the named volume `compose_packyard-data` (SQLite
  catalog + content-addressed blob store + bootstrapped
  `channels.yaml` / `matrix.yaml`).
- Default channels `dev`, `test`, `prod` with `prod` as default.
- **Anonymous CRAN-protocol reads enabled on the default channel**
  via the `-allow-anonymous-reads` flag in the compose file. Every
  non-read endpoint (publish, yank, delete, admin) still requires a
  token.
- `unless-stopped` restart policy.
- Healthcheck shells out to `packyard-server -version` (distroless
  ships no `curl` / `wget`).

## First-run: mint an admin token

```sh
ADMIN=$(docker compose exec packyard \
  packyard-server -mint-token -data /data \
  -scopes admin -label bootstrap 2>/dev/null)
echo "$ADMIN"
```

The plaintext is printed **once**; packyard stores only
`sha256(token)`. Copy it into a secrets manager now. Use it to mint
scoped publish / read tokens via `POST /api/v1/admin/tokens` — see
[../../docs/admin.md](../../docs/admin.md) and
[../../docs/api.md](../../docs/api.md).

## Publish a test package

```sh
PUBLISH=$(curl -sf -X POST http://localhost:8080/api/v1/admin/tokens \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"label":"ci","scopes":["publish:dev"]}' | jq -r .token)

# Then follow the quickstart step 4 onwards:
# ../../docs/quickstart.md
```

## Install from R

```r
install.packages(
  "<your-package>",
  repos = c(packyard = "http://localhost:8080/", getOption("repos"))
)
```

This works out of the box because `-allow-anonymous-reads` is on in
the compose file. When you turn it off (next section), R clients need
to supply a bearer token — see the recipe below.

## Daily ops

```sh
docker compose logs -f packyard     # stream structured JSON logs
docker compose restart packyard     # hot-restart (30 s graceful)
docker compose down                 # stop; volume survives
docker compose down -v              # stop and WIPE the data volume
```

Volume location:

```sh
docker volume inspect compose_packyard-data
```

Back the volume up per [../../docs/backup-restore.md](../../docs/backup-restore.md).

## Using a local build

For contributor workflows, comment the `image:` line and uncomment
the `build:` block in `docker-compose.yml`, then:

```sh
docker compose up -d --build
```

Code changes now trigger a rebuild instead of pulling from GHCR.

## Production hardening

The shipped compose file is tuned for evaluation. Before pointing
real users at it, make these changes:

### 1. Drop `-allow-anonymous-reads`

Edit `docker-compose.yml` and remove the flag from `command:` so the
server rejects unauthenticated reads with `401`. Or set it off in a
mounted `server.yaml`:

```yaml
# server.yaml
listen: ":8080"
data_dir: "/data"
allow_anonymous_reads: false
```

…and point the container at it:

```yaml
# docker-compose.yml fragment
    command: ["-config", "/config/server.yaml"]
    volumes:
      - packyard-data:/data
      - ./server.yaml:/config/server.yaml:ro
```

### 2. Issue `read:<channel>` tokens for R clients

Once anonymous reads are off, R needs to send `Authorization: Bearer`
on every request. Base R's `install.packages()` doesn't take
per-repo headers directly, so wire it through `download.file.method`
or `pak`:

```r
# options-level — before any install.packages() call.
token <- Sys.getenv("PACKYARD_TOKEN")
options(
  repos = c(packyard = "https://packyard.corp/", getOption("repos")),
  download.file.method = "libcurl",
  download.file.extra = paste0("--header 'Authorization: Bearer ", token, "'")
)
install.packages("<your-package>")
```

`pak` users can instead do:

```r
pak::repo_add(packyard = "https://packyard.corp/")
# pak picks up the header via the same download.file.extra option.
```

### 3. Terminate TLS in front

Packyard ships with a TLS option
([../../docs/config.md](../../docs/config.md)) but the common pattern
is Caddy / Traefik / nginx in front, running in the same Compose
stack. Skeleton:

```yaml
services:
  packyard:
    # ...as above, but remove the `ports:` block so only the reverse
    # proxy publishes 443.

  caddy:
    image: caddy:2
    ports: ["443:443", "80:80"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-certs:/config
    depends_on: [packyard]

volumes:
  packyard-data:
  caddy-data:
  caddy-certs:
```

Minimal `Caddyfile`:

```
packyard.corp {
  reverse_proxy packyard:8080
}
```

### 4. Pin the image tag

Replace `ghcr.io/schochastics/packyard:latest` with a specific
version tag (e.g. `:1.0.1` — GHCR tags have no `v` prefix; the
matching Git tag on GitHub does) so a redeploy never surprises
you with an unintended upgrade.

### 5. Back up the volume

The named volume holds the entire catalog. Follow the cadence table
in [../../docs/backup-restore.md](../../docs/backup-restore.md) — at
minimum: nightly `.backup` of `db.sqlite`, nightly `rsync` of
`cas/`, on a host distinct from the live disk.
