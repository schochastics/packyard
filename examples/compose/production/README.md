# Production compose example

This folder combines the pieces of a service deployment
([docs/admin.md](../../../docs/admin.md#running-as-a-service)):

- **Read-only config.** `config/` holds `server.yaml`,
  `channels.yaml` and `matrix.yaml`, mounted at `/etc/packyard`.
- **Provisioned tokens.** `server.yaml` `tokens:` reads its secrets
  from `secrets/`, mounted at `/run/secrets/packyard`. No
  `-mint-token` step is needed.
- **Metrics on an internal port.** `metrics_listen: ":9090"` is
  reachable on the compose network only. Scrape `packyard:9090`.
- **Health check** through the binary itself (`-healthcheck`).
- **Backups** through a one-shot `backup` service under the `backup`
  profile.

## Setup

```sh
mkdir -p secrets
# CI's publish token: plaintext on both sides.
docker run --rm ghcr.io/schochastics/packyard:latest admin token-gen | head -n1 > secrets/ci-publish
# Admin token: the server only gets the hash.
docker run --rm ghcr.io/schochastics/packyard:latest admin token-gen > admin.txt
sed -n 2p admin.txt > secrets/admin.sha256
sed -n 1p admin.txt   # store this in your password manager, then: rm admin.txt
chmod 644 secrets/*   # readable by the container's uid 65532

docker compose up -d
docker compose ps     # STATUS shows (healthy)
```

Edit `config/server.yaml` first: set `public_url` to your hostname
and `trusted_proxies` to your reverse proxy's network. Then set
`config/matrix.yaml` to your clients' distro and R versions.

## Backups

```sh
docker compose --profile backup run --rm backup
docker compose --profile backup run --rm backup admin backup -verify /backup/packyard
```

Run the first line from cron or a systemd timer. The `packyard-backup`
volume is for illustration only: point it at storage outside the
host. See [docs/backup-restore.md](../../../docs/backup-restore.md)
for rotation and restore.
