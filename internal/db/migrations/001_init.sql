-- 001_init.sql
-- Initial packyard schema. See design.md §3 (entities) and implementation.md A2
-- for field-level rationale. Timestamps are ISO-8601 UTC strings ("YYYY-MM-
-- DDTHH:MM:SS.sssZ") rather than unix seconds so rows are readable with the
-- sqlite3 CLI during ops/debug.

CREATE TABLE channels (
    name             TEXT    PRIMARY KEY,
    overwrite_policy TEXT    NOT NULL CHECK (overwrite_policy IN ('mutable', 'immutable')),
    is_default       INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Exactly one channel may be the default. Enforced by a partial unique index
-- rather than a CHECK, so inserts and updates don't need to reason globally.
CREATE UNIQUE INDEX channels_one_default ON channels(is_default) WHERE is_default = 1;

CREATE TABLE packages (
    id             INTEGER PRIMARY KEY,
    channel        TEXT    NOT NULL REFERENCES channels(name) ON DELETE RESTRICT,
    name           TEXT    NOT NULL,
    version        TEXT    NOT NULL,
    source_sha256  TEXT    NOT NULL,
    source_size    INTEGER NOT NULL,
    published_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    published_by   TEXT,                          -- token label; NULL for in-process imports
    yanked         INTEGER NOT NULL DEFAULT 0 CHECK (yanked IN (0, 1)),
    yank_reason    TEXT,
    UNIQUE (channel, name, version)
);

CREATE INDEX packages_channel_name ON packages(channel, name);
CREATE INDEX packages_source_sha256 ON packages(source_sha256);

CREATE TABLE binaries (
    id            INTEGER PRIMARY KEY,
    package_id    INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    cell          TEXT    NOT NULL,
    binary_sha256 TEXT    NOT NULL,
    size          INTEGER NOT NULL,
    uploaded_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (package_id, cell)
);

CREATE INDEX binaries_binary_sha256 ON binaries(binary_sha256);

CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,    -- monotonic, never reused
    at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    type    TEXT    NOT NULL,                     -- publish, yank, delete, token_issued, ...
    actor   TEXT,
    channel TEXT,
    package TEXT,
    version TEXT,
    note    TEXT
);

CREATE INDEX events_at ON events(at);
CREATE INDEX events_channel_package ON events(channel, package);

CREATE TABLE tokens (
    id            INTEGER PRIMARY KEY,
    token_sha256  TEXT    NOT NULL UNIQUE,        -- we never store the plaintext token
    scopes_csv    TEXT    NOT NULL,               -- e.g. "publish:dev,read:*,admin"
    label         TEXT,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_used_at  TEXT,
    revoked_at    TEXT
);
