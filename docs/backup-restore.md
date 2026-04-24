# Backup & restore

Packyard stores everything in the data directory (`-data <dir>`,
default `/data` in the Docker image). Protecting that one directory is
the whole backup story — there is no external database or object store
to coordinate with.

This runbook covers what to back up, how to take a consistent
snapshot while the server is running, and how to restore from a
backup on a fresh host.

## What's in the data directory

```
<data-dir>/
  db.sqlite           # catalog: channels, packages, binaries, events, tokens
  db.sqlite-wal       # WAL file (present while the server runs)
  db.sqlite-shm       # shared memory index (present while the server runs)
  cas/                # content-addressed blob store: source + binary tarballs
    <aa>/<rest-of-sha256>
    ...
  channels.yaml       # channel set (usually also tracked in git)
  matrix.yaml         # binary cell matrix (usually also tracked in git)
```

What matters:

- **`db.sqlite`** — the catalog of packages, event log, token hashes.
  Losing this loses everything; CAS blobs without DB rows are orphans.
- **`cas/`** — every published tarball. Losing this loses the
  artifacts themselves; DB rows without CAS blobs are broken.
- **`channels.yaml` / `matrix.yaml`** — configuration. Typically
  tracked in git alongside your infrastructure code, so backing them
  up with the data dir is belt-and-braces.

Tokens live **only** in `db.sqlite` and only as `sha256(token)`. A
restore from backup keeps existing tokens working; a lost DB means
every token has to be reissued (see [Reissuing tokens](#reissuing-tokens)).

## Taking a consistent snapshot

### SQLite — use `VACUUM INTO` or the online backup API

The catalog runs in WAL mode (`journal_mode=WAL`), so a plain
`cp db.sqlite backup.sqlite` under load can capture an inconsistent
mid-transaction state: the file you copy won't contain recent writes
still held in `db.sqlite-wal`, and you can end up with a torn read if
the server checkpoints mid-copy.

**Do not `cp` / `rsync` `db.sqlite` while the server is running.**

Use one of these instead:

```sh
# Option 1: VACUUM INTO — single-file dump, consistent, also defragments.
sqlite3 <data-dir>/db.sqlite "VACUUM INTO '/backup/db-$(date -u +%Y%m%dT%H%M%SZ).sqlite'"

# Option 2: .backup — uses SQLite's online backup API; safe under concurrent writes.
sqlite3 <data-dir>/db.sqlite ".backup '/backup/db-$(date -u +%Y%m%dT%H%M%SZ).sqlite'"
```

Both produce a single `.sqlite` file that is a point-in-time
consistent snapshot and can be restored by dropping it back in
place. `.backup` is the one to reach for if the DB is under active
write load; `VACUUM INTO` is simpler and also compacts, but takes a
longer write lock.

### CAS — rsync is safe

Blobs in `cas/` are written atomically (temp-file-then-rename) and
keyed by `sha256(content)`, so any file at `cas/<aa>/<rest>` either
doesn't exist yet or has its final content. A concurrent `rsync` on
the live directory will:

- Include blobs that finished writing before rsync walked them.
- Miss blobs that landed after rsync's directory walk — those will be
  picked up by the next backup run.
- **Never** capture a half-written blob, because half-written blobs
  live under `cas/tmp/` (not under `cas/<aa>/`) until they rename.

A typical nightly looks like:

```sh
rsync -a --delete-after <data-dir>/cas/ /backup/cas/
```

Do the rsync **after** the SQLite snapshot so every DB row references
a blob that's already on disk. The other order risks backing up DB
rows for blobs that haven't been copied yet.

### Config files — copy with the rest

```sh
cp <data-dir>/channels.yaml <data-dir>/matrix.yaml /backup/
```

These change rarely; if you already track them in git, the git copy
is authoritative and this step is redundant.

## Cadence

Tune to your tolerance for losing publishes. A reasonable starting
point for a small team:

| What | How often | Retention |
|---|---|---|
| `db.sqlite` via `.backup` | **Hourly** | 7 × hourly + 30 × daily |
| `cas/` via `rsync` | **Daily** | 30 × daily |
| `channels.yaml` / `matrix.yaml` | On change (git) | Git history |

If you publish rarely (say, a handful of packages per week), a daily
SQLite backup is enough. If you publish on every CI run, an hourly
SQLite backup keeps the worst-case data loss window small, while the
daily CAS rsync is fine because old blobs never change — only new
blobs are added, and a missed new blob just means a re-publish.

Keep at least one backup on a different host or storage tier from the
live data. A backup on the same disk protects against accidental `rm
-rf`, not against disk failure.

## Restoring

On a clean host:

1. Install the same packyard version that produced the backup
   (`packyard-server -version` on the old host tells you which).
   Restoring a newer backup onto an older binary may fail schema
   validation; restoring an older backup onto a newer binary will run
   pending migrations at startup, which is safe but not reversible.

2. Lay the data directory back out:

   ```sh
   mkdir -p /data
   cp /backup/db-<timestamp>.sqlite /data/db.sqlite
   rsync -a /backup/cas/ /data/cas/
   cp /backup/channels.yaml /backup/matrix.yaml /data/   # if not in git
   chown -R <packyard-user>:<packyard-user> /data
   ```

   Do **not** restore `db.sqlite-wal` or `db.sqlite-shm` — they're
   artifacts of the running server and SQLite recreates them on
   startup from the consistent snapshot.

3. Start the server:

   ```sh
   packyard-server -data /data
   ```

   It will apply any pending migrations, reconcile `channels.yaml`
   against the DB, and start serving.

4. Verify integrity (next section).

## Verifying a restore

After restore, two checks catch the common failure modes:

```sh
# 1. Does the DB open cleanly? Migrations applied?
packyard-server -init -data /data
# → prints "storage ready: db=... cas=..." and exits 0 on success.

# 2. Does every DB row point to a blob that exists, and vice versa?
packyard-server admin -data /data reindex
```

`admin reindex` walks the DB, regenerates PACKAGES caches, and
reports any DB rows whose CAS blob is missing. A clean report means
the restore is consistent. Missing blobs mean the CAS rsync was
incomplete — re-run it and re-check.

Then spot-check a real install:

```sh
# From a machine with R:
R -e 'install.packages("<some-pkg>", repos = "http://<restored-host>:8080/")'
```

If install succeeds, the read surface is working.

## Reissuing tokens

Tokens are stored as `sha256(token)`, so:

- **Restoring from a real backup keeps all existing tokens valid** —
  CI doesn't need to learn a new secret.
- **Losing the DB means all tokens are gone.** Clients have the
  plaintext token; the server has only the hash and can't recompute
  it. There is no recovery path other than issuing fresh tokens and
  rotating the secret in every CI config that publishes.

If you've lost the DB:

```sh
# Mint a fresh admin token on the new data dir.
ADMIN=$(packyard-server -mint-token -data /data -scopes admin -label bootstrap)
# Then use it to reissue publish tokens via POST /api/v1/admin/tokens.
```

See [admin.md](admin.md) for the full token-management flow.

## Disaster recovery drill

Once a quarter (or before a big release), prove the runbook actually
works:

1. On a throwaway host, restore the latest backup.
2. Run `admin reindex` — should report clean.
3. `install.packages()` a known package from the restored server.
4. Publish a fresh test package; yank it; delete it.
5. Tear the host down.

Any step that fails is a bug in the runbook or in the backup
pipeline. Fix it now, not during a real outage.
