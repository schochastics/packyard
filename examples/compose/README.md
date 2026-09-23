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

- Image `gitea.cynkra.com/david.schoch/packyard:latest` pulled on first run.
- Data persisted in the named volume `compose_packyard-data` (SQLite
  catalog + content-addressed blob store + bootstrapped
  `channels.yaml` / `matrix.yaml`).
- Channels `dev`, `test`, `prod` from [channels.yaml](channels.yaml),
  mounted read-only, with `prod` as default.
- **Anonymous CRAN-protocol reads on `prod`**
  (`anonymous_reads: true` in `channels.yaml`). Every other endpoint
  (publish, yank, delete, admin, the JSON API) still requires a token,
  and so do reads of `dev` and `test`.
- `unless-stopped` restart policy.
- Healthcheck runs `packyard-server -healthcheck` (distroless ships no
  `curl` / `wget`).

For a production-shaped deployment (read-only config, tokens from
secret files, a separate metrics port, backups) see
[production/](production/).

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

This works out of the box because `prod` sets `anonymous_reads: true`.
When you turn it off (next section), R clients need to supply a bearer
token; see the recipe below.

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

Code changes now trigger a rebuild instead of pulling from the registry.

## Production hardening

The shipped compose file is tuned for evaluation. Before pointing
real users at it, review the changes below. Note that **"production"
doesn't automatically mean "token-gated reads"** — public CRAN is
anonymous, and most private R registries (PPM, r-universe, drat
behind nginx) rely on network-layer controls rather than
CRAN-protocol auth. Pick the shape that matches your deployment:

- **Trusted network** (VPN / internal subnet / corporate SSO proxy
  in front): leave `anonymous_reads: true` on. Network decides who
  can talk to packyard. This is the more common private-registry
  pattern and is what the compose file ships with.
- **Internet-exposed**: remove `anonymous_reads`, issue
  per-client `read:<channel>` tokens, and wire them into R via the
  recipe in §2. Only necessary when you can't put the server on a
  trusted network.

Everything below applies in both modes except §1 and §2, which only
matter for the internet-exposed shape.

### 1. Turn off anonymous reads

Remove `anonymous_reads: true` from `prod` in
[channels.yaml](channels.yaml) and `docker compose restart packyard`.
The server then rejects unauthenticated reads with `401`.

### 2. Issue `read:<channel>` tokens for R clients

Once anonymous reads are off, R needs to send `Authorization: Bearer`
on every request. Base R's `install.packages()` doesn't take
per-repo headers directly. One way is the external `curl` download
method, whose extra arguments R passes through:

```r
# options-level — before any install.packages() call.
token <- Sys.getenv("PACKYARD_TOKEN")
options(
  repos = c(packyard = "https://packyard.corp/", getOption("repos")),
  download.file.method = "curl",
  download.file.extra = paste0("--header 'Authorization: Bearer ", token, "'")
)
install.packages("<your-package>")
```

This sends the header to every repository in `repos`, CRAN included.
Keep it to trusted mirrors.

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

Replace `gitea.cynkra.com/david.schoch/packyard:latest` with a specific
version tag (e.g. `:1.3.0`; image tags have no `v` prefix, the
matching Git tag does) so a redeploy never surprises you with an
unintended upgrade.

### 5. Back up the volume

The named volume holds the entire catalog. Run
`packyard-server admin backup -out` on a schedule, into storage on a
different host or disk, and verify it with `admin backup -verify`. See
[../../docs/backup-restore.md](../../docs/backup-restore.md) and the
`backup` service in [production/](production/).
