-- 005_ui_sessions: server-side sessions for the /ui/ dashboard.
--
-- The cookie carries a random session id; only its sha256 is stored.
-- Before this, the cookie held the bearer token itself (HMAC-signed),
-- so a copied cookie was the token and logout revoked nothing. Rows go
-- away on logout, on expiry (swept at login) and with their token.

CREATE TABLE ui_sessions (
    id_sha256  TEXT    PRIMARY KEY,
    token_id   INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT    NOT NULL
);

CREATE INDEX ui_sessions_expires_at ON ui_sessions(expires_at);
