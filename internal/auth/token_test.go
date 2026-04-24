package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/db"
)

func setupTokenDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

// seedToken inserts a token row and returns (plaintext, id).
func seedToken(t *testing.T, database *db.DB, label, scopes string, revoked bool) string {
	t.Helper()
	plaintext, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	var revAt sql.NullString
	if revoked {
		revAt = sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true}
	}
	_, err = database.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label, revoked_at)
		VALUES (?, ?, ?, ?)
	`, auth.HashToken(plaintext), scopes, label, revAt)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return plaintext
}

func TestGenerateTokenHasPrefixAndHexBody(t *testing.T) {
	t.Parallel()
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(tok, "pkm_") {
		t.Errorf("token %q missing pkm_ prefix", tok)
	}
	// 32 bytes → 64 hex chars after prefix.
	body := strings.TrimPrefix(tok, "pkm_")
	if len(body) != 64 {
		t.Errorf("token body len = %d, want 64", len(body))
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	t.Parallel()
	a := auth.HashToken("pkm_hello")
	b := auth.HashToken("pkm_hello")
	if a != b {
		t.Error("HashToken is non-deterministic")
	}
	if len(a) != 64 {
		t.Errorf("hash len = %d, want 64 hex", len(a))
	}
}

func TestParseBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header, want string
		ok           bool
	}{
		{"Bearer pkm_xyz", "pkm_xyz", true},
		{"bearer pkm_xyz", "pkm_xyz", true},
		{"BEARER pkm_xyz", "pkm_xyz", true},
		{"  Bearer   pkm_xyz  ", "pkm_xyz", true},
		{"Basic dXNlcjpwYXNz", "", false},
		{"", "", false},
		{"Bearer", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()
			got, ok := auth.ParseBearer(tc.header)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLookupHappyPath(t *testing.T) {
	t.Parallel()

	database := setupTokenDB(t)
	tok := seedToken(t, database, "ci-dev", "publish:dev,read:*", false)

	id, err := auth.Lookup(context.Background(), database.DB, tok)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if id.Label != "ci-dev" {
		t.Errorf("Label = %q, want ci-dev", id.Label)
	}
	if !id.Scopes.Has("publish:dev") {
		t.Error("identity missing publish:dev")
	}
	if !id.Scopes.Has("read:prod") { // via read:*
		t.Error("identity missing read:prod via read:*")
	}

	// Successful lookup must update last_used_at.
	var lastUsed sql.NullString
	_ = database.QueryRowContext(context.Background(),
		`SELECT last_used_at FROM tokens WHERE id=?`, id.TokenID).Scan(&lastUsed)
	if !lastUsed.Valid || lastUsed.String == "" {
		t.Error("last_used_at not updated after successful lookup")
	}
}

func TestLookupUnknownToken(t *testing.T) {
	t.Parallel()

	database := setupTokenDB(t)
	_, err := auth.Lookup(context.Background(), database.DB, "pkm_bogus")
	if !errors.Is(err, auth.ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestLookupRevokedToken(t *testing.T) {
	t.Parallel()

	database := setupTokenDB(t)
	tok := seedToken(t, database, "rev", "publish:dev", true)

	_, err := auth.Lookup(context.Background(), database.DB, tok)
	if !errors.Is(err, auth.ErrTokenNotFound) {
		t.Errorf("revoked token was accepted; err = %v", err)
	}
}

func TestLookupEmptyPlaintext(t *testing.T) {
	t.Parallel()

	database := setupTokenDB(t)
	_, err := auth.Lookup(context.Background(), database.DB, "")
	if !errors.Is(err, auth.ErrTokenNotFound) {
		t.Errorf("empty plaintext: err = %v, want ErrTokenNotFound", err)
	}
}
