-- 004_token_source: tokens provisioned from server.yaml.
--
-- 'api' tokens are minted at runtime (CLI -mint-token, POST
-- /api/v1/admin/tokens) and live only in the DB. 'config' tokens are
-- declared under server.yaml's tokens: and reconciled on every start:
-- inserted when new, rescoped when edited, revoked when removed. The
-- API refuses to revoke a 'config' token — the config would put it
-- back on the next start.

ALTER TABLE tokens ADD COLUMN source TEXT NOT NULL DEFAULT 'api'
    CHECK (source IN ('api', 'config'));
