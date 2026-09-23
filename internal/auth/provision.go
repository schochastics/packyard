package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gitea.cynkra.com/david.schoch/packyard/internal/config"
)

// Token sources, as stored in tokens.source.
const (
	SourceAPI    = "api"
	SourceConfig = "config"
)

// SyncResult reports what SyncConfigTokens changed, by label.
type SyncResult struct {
	Created []string
	Updated []string
	Revoked []string
}

// SyncConfigTokens makes the active config-sourced tokens match
// server.yaml's tokens: list, in one transaction:
//
//   - a listed token whose secret isn't in the DB is inserted;
//   - one that is (matched by hash) gets the listed label and scopes,
//     and is un-revoked if it had been removed from the config before;
//   - an active config token no longer listed — including the old
//     secret of a rotated entry — is revoked.
//
// API-minted tokens are never touched. Every secret file is read up
// front, so a missing or malformed one fails startup before anything
// changes.
func SyncConfigTokens(ctx context.Context, db *sql.DB, tokens []config.TokenConfig) (SyncResult, error) {
	var res SyncResult

	type wanted struct {
		label, scopes, hash string
	}
	want := make([]wanted, 0, len(tokens))
	for _, t := range tokens {
		hash, err := readTokenSecret(t)
		if err != nil {
			return res, fmt.Errorf("token %q: %w", t.Label, err)
		}
		scopes := ParseScopes(t.Scopes)
		for s := range scopes {
			if !ValidScope(s) {
				return res, fmt.Errorf("token %q: invalid scope %q", t.Label, s)
			}
		}
		want = append(want, wanted{t.Label, scopes.CSV(), hash})
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	keep := map[int64]bool{}
	for _, w := range want {
		var (
			id             int64
			source, scopes string
			label, revoked sql.NullString
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, source, label, scopes_csv, revoked_at FROM tokens WHERE token_sha256 = ?`,
			w.hash).Scan(&id, &source, &label, &scopes, &revoked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			r, err := tx.ExecContext(ctx, `
				INSERT INTO tokens(token_sha256, scopes_csv, label, created_at, source)
				VALUES (?, ?, ?, ?, 'config')`, w.hash, w.scopes, w.label, now)
			if err != nil {
				return res, fmt.Errorf("token %q: insert: %w", w.label, err)
			}
			id, _ = r.LastInsertId()
			res.Created = append(res.Created, w.label)
		case err != nil:
			return res, fmt.Errorf("token %q: lookup: %w", w.label, err)
		case source != SourceConfig:
			return res, fmt.Errorf("token %q: the same secret is already registered as API token id %d; revoke that one or use a different secret", w.label, id)
		default:
			if label.String != w.label || scopes != w.scopes || revoked.Valid {
				if _, err := tx.ExecContext(ctx,
					`UPDATE tokens SET label = ?, scopes_csv = ?, revoked_at = NULL WHERE id = ?`,
					w.label, w.scopes, id); err != nil {
					return res, fmt.Errorf("token %q: update: %w", w.label, err)
				}
				res.Updated = append(res.Updated, w.label)
			}
		}
		keep[id] = true
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, COALESCE(label, '') FROM tokens WHERE source = 'config' AND revoked_at IS NULL`)
	if err != nil {
		return res, err
	}
	type stale struct {
		id    int64
		label string
	}
	var drop []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.label); err != nil {
			_ = rows.Close()
			return res, err
		}
		if !keep[s.id] {
			drop = append(drop, s)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, s := range drop {
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ?`, now, s.id); err != nil {
			return res, fmt.Errorf("revoke token %d: %w", s.id, err)
		}
		res.Revoked = append(res.Revoked, s.label)
	}

	for _, ev := range []struct {
		typ    string
		labels []string
	}{{"token_create", res.Created}, {"token_update", res.Updated}, {"token_revoke", res.Revoked}} {
		for _, l := range ev.labels {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events(at, type, actor, note) VALUES (?, ?, 'config', ?)`,
				now, ev.typ, l); err != nil {
				return res, fmt.Errorf("record event: %w", err)
			}
		}
	}
	return res, tx.Commit()
}

// readTokenSecret returns the sha256 hex of the token t declares.
func readTokenSecret(t config.TokenConfig) (string, error) {
	path := t.TokenFile
	if path == "" {
		path = t.SHA256File
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if t.TokenFile != "" {
		if !strings.HasPrefix(v, tokenPrefix) || len(v) <= len(tokenPrefix) {
			return "", fmt.Errorf("%s: token must start with %q (generate one with `packyard-server admin token-gen`)", path, tokenPrefix)
		}
		return HashToken(v), nil
	}
	v = strings.ToLower(v)
	if raw, err := hex.DecodeString(v); err != nil || len(raw) != 32 {
		return "", fmt.Errorf("%s: expected a 64-character hex sha256", path)
	}
	return v, nil
}
