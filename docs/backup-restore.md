# Backup & restore

Packyard keeps everything in its data directory (`-data <dir>`).
Protecting that one directory is the whole backup story: there is no
external database or object store to coordinate with. Three admin
commands cover it:

| Command | What it does | Server running? |
|---|---|---|
| `admin backup -out <dir>` | consistent snapshot of DB, blobs and config | yes, safe |
| `admin backup -verify <dir>` | re-hash every blob, check references, `PRAGMA integrity_check` | n/a |
| `admin restore -from <dir> [-data <dir>] [-force]` | verify, then lay the backup into a data dir | **no**, stop it first |

## What's in the data directory

```
<data-dir>/
  db.sqlite           # catalog: channels, packages, binaries, events, tokens
  db.sqlite-wal/-shm  # present while the server runs
  cas/<aa>/<rest>     # content-addressed blobs: source + binary tarballs
  channels.yaml       # channel set (unless channels_file points elsewhere)
  matrix.yaml         # distro + R versions (unless matrix_file points elsewhere)
  ui-session-key      # HMAC key for UI cookies; not backed up (users log in again)
```

Tokens live in `db.sqlite`, and only as `sha256(token)`. Restoring a
backup keeps every token working. Losing the DB without a backup
means reissuing all of them (see [Reissuing tokens](#reissuing-tokens)).

## Taking a backup

```sh
packyard-server admin -data /data backup -out /backup/packyard
# backup written to /backup/packyard: blobs=412 (3 new) size=1.2 GiB config=[channels.yaml matrix.yaml]
```

With `-config /etc/packyard/server.yaml` in front of the verb, the
data dir and config paths come from that file, and `server.yaml` is
included in the backup too.

What it writes:

```
<out>/db.sqlite         VACUUM INTO snapshot, transactionally consistent
<out>/cas/<aa>/<rest>   every blob the snapshot references
<out>/config/*.yaml     server.yaml (with -config), channels.yaml, matrix.yaml
<out>/manifest.json     time, packyard version, schema version, blob count and bytes
```

- **Consistent while serving.** The DB snapshot is a single
  transaction, and the blob list is read from the snapshot, not from
  the live DB. Every row in the backup therefore has its blob.
- **Incremental.** Blobs never change, so backing up into the same
  `<out>` again only adds the new ones. Blobs are hardlinked when
  `<out>` is on the same filesystem as the data dir and copied
  otherwise. Put backups on different storage for real protection;
  a same-disk backup only guards against mistakes, not disk failure.
- **Rotation.** For dated snapshots, back up into a new directory
  each time and let `rsync --link-dest` (or a snapshotting
  filesystem) share unchanged blobs:

  ```sh
  today=/backup/packyard-$(date -u +%Y%m%d)
  packyard-server admin -data /data backup -out /backup/packyard-staging
  rsync -a --link-dest=/backup/packyard-latest /backup/packyard-staging/ "$today/"
  ln -sfn "$today" /backup/packyard-latest
  ```

- **Not included:** token secret files referenced by `server.yaml`
  `tokens:` (they live in your secret store) and `ui-session-key`.

In Docker, run it inside the container with the backup target mounted:

```sh
docker compose exec packyard packyard-server admin -data /data backup -out /backup/packyard
```

## Verifying

```sh
packyard-server admin backup -verify /backup/packyard
# integrity_check: ok
# blobs re-hashed: 412
# missing: 0
# corrupt: 0
```

Exits non-zero on any problem and lists the affected blobs. Run it
after each backup, or at least on a schedule. It reads every byte, so
it is the check that catches silent storage rot.

## Cadence

| Publish rate | Backup | Verify |
|---|---|---|
| a few packages a week | daily | weekly |
| every CI run | hourly | daily |

A backup is cheap after the first one: a DB snapshot plus the handful
of new blobs.

## Restoring

1. Stop the server.
2. Restore:

   ```sh
   packyard-server admin restore -from /backup/packyard -data /data
   ```

   Restore verifies the backup first and refuses a damaged one. It
   also refuses a data dir that already holds a database or blobs
   unless given `-force`. `-force` replaces `db.sqlite` and `cas/`.
   Stale `db.sqlite-wal`/`-shm` files are removed too.

   `channels.yaml` and `matrix.yaml` are written only when their
   effective path is inside the data dir, and without `-force` only
   when absent. Files managed elsewhere, such as a read-only config
   mount or `server.yaml`, are reported with the path of the backup's
   copy.
3. Fix ownership if you restored as root. The image runs as
   uid/gid 65532:

   ```sh
   chown -R 65532:65532 /data
   ```

4. Start the server. It applies pending migrations, so restoring an
   older backup onto a newer packyard works. There are no down
   migrations, so a backup made by a newer packyard needs that
   version or later.
5. Spot-check: `admin reindex` must report no missing blobs, and an
   `install.packages()` from the restored server should succeed.

## Reissuing tokens

- **Restoring a backup keeps every token valid.** CI keeps its secret.
- **Tokens from `server.yaml` `tokens:`** come back on the next start
  from their secret files, whatever the state of the DB.
- **Minted tokens, if the DB is lost without a backup,** are gone.
  The server only ever had their hashes. Mint new ones and rotate
  them in every CI config:

  ```sh
  ADMIN=$(packyard-server -mint-token -data /data -scopes admin -label bootstrap)
  # then POST /api/v1/admin/tokens with it
  ```

## Disaster recovery drill

Once a quarter, prove the runbook works:

1. On a throwaway host, `admin restore` the latest backup.
2. Start the server and run `admin reindex`. It should report clean.
3. `install.packages()` a known package from it.
4. Publish a test package, yank it, delete it.
5. Tear the host down.

Anything that fails is a bug in the runbook or the backup pipeline.
Fix it now, not during an outage.
