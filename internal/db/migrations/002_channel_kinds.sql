-- 002_channel_kinds.sql
-- Add the `kind` and `upstream_url` columns to the channels table so a
-- channel can be a "proxy" (lazy-cached view of an upstream CRAN /
-- PPM / r-universe) in addition to the existing "local" channels.
--
-- The kind <-> upstream_url cross-column invariant (kind='proxy'
-- requires upstream_url IS NOT NULL) can't be expressed via ALTER
-- TABLE in SQLite without recreating the table, so it's enforced in
-- the application validate() layer (internal/config/channels.go) and
-- at reconcile insert/update time. The single-column CHECK on `kind`
-- below still catches typos in the YAML config at write time.
--
-- See design.md §15 (Lazy proxy channels) for the feature design.

ALTER TABLE channels
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'local'
    CHECK (kind IN ('local', 'proxy'));

ALTER TABLE channels
    ADD COLUMN upstream_url TEXT;
