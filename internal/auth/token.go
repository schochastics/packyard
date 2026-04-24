package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// tokenBytes is the length of the random portion of a packyard token.
// 32 bytes (256 bits) is the common choice for opaque bearer tokens —
// big enough to resist online guessing and offline rainbow tables, not
// so big that copying the string into a CI secret is annoying.
const tokenBytes = 32

// tokenPrefix makes tokens identifiable at a glance in logs, secret
// scanners, and CI config files ("ah, pkm_... that's a packyard token").
const tokenPrefix = "pkm_"

// ErrTokenNotFound is returned by Lookup when no unrevoked token with
// the given plaintext matches the DB. Callers use errors.Is to
// distinguish from infrastructure errors.
var ErrTokenNotFound = errors.New("auth: token not found or revoked")

// Identity is what a verified token grants. It is attached to the
// request context by the API's auth middleware.
type Identity struct {
	TokenID int64
	Label   string
	Scopes  ScopeSet
}

// HashToken returns the hex SHA-256 of plaintext. Tokens are never
// stored in plaintext; comparisons are done via this hash.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a new random plaintext token with the packyard
// prefix. Uses crypto/rand; callers should surface the error if the
// system random source is broken.
func GenerateToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return tokenPrefix + hex.EncodeToString(buf), nil
}

// ParseBearer extracts the token from an "Authorization: Bearer <token>"
// header value. Returns ("", false) for anything else. Comparison of
// the "Bearer" keyword is case-insensitive to match real-world clients.
func ParseBearer(header string) (string, bool) {
	const prefix = "bearer"
	header = strings.TrimSpace(header)
	if len(header) < len(prefix)+1 {
		return "", false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// Lookup resolves a plaintext bearer token to an Identity via the DB.
// Returns ErrTokenNotFound on absent/revoked/mismatched tokens so
// callers can distinguish auth failure from DB failure.
//
// Side effect: on a successful lookup, updates last_used_at on the
// token row. The update is best-effort and does not fail the lookup
// if it errors — observability, not correctness.
func Lookup(ctx context.Context, db *sql.DB, plaintext string) (Identity, error) {
	if plaintext == "" {
		return Identity{}, ErrTokenNotFound
	}
	hash := HashToken(plaintext)

	var (
		id    int64
		label sql.NullString
		csv   string
		revAt sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, label, scopes_csv, revoked_at
		FROM tokens
		WHERE token_sha256 = ?
	`, hash).Scan(&id, &label, &csv, &revAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrTokenNotFound
	}
	if err != nil {
		return Identity{}, fmt.Errorf("auth: lookup token: %w", err)
	}
	if revAt.Valid {
		return Identity{}, ErrTokenNotFound
	}

	touchLastUsed(ctx, db, id)

	return Identity{
		TokenID: id,
		Label:   label.String,
		Scopes:  ParseScopes(csv),
	}, nil
}

// touchLastUsed bumps tokens.last_used_at. Best-effort: a failure here
// must not reject the request.
func touchLastUsed(ctx context.Context, db *sql.DB, id int64) {
	_, _ = db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano),
		id,
	)
}
