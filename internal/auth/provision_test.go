package auth_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitea.cynkra.com/david.schoch/packyard/internal/auth"
	"gitea.cynkra.com/david.schoch/packyard/internal/config"
	"gitea.cynkra.com/david.schoch/packyard/internal/db"
)

func writeSecret(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func lookup(t *testing.T, database *db.DB, plaintext string) (auth.Identity, error) {
	t.Helper()
	return auth.Lookup(context.Background(), database.DB, plaintext)
}

func TestSyncConfigTokensLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := setupTokenDB(t)
	apiToken := seedToken(t, database, "minted", "admin", false)

	ci, _ := auth.GenerateToken()
	ro, _ := auth.GenerateToken()
	ciFile := writeSecret(t, "ci", ci+"\n")
	roHash := writeSecret(t, "ro.sha256", strings.ToUpper(auth.HashToken(ro)))

	tokens := []config.TokenConfig{
		{Label: "ci", Scopes: "publish:prod, yank:prod", TokenFile: ciFile},
		{Label: "readonly", Scopes: "read:*", SHA256File: roHash},
	}
	res, err := auth.SyncConfigTokens(ctx, database.DB, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Created, []string{"ci", "readonly"}) || res.Updated != nil || res.Revoked != nil {
		t.Errorf("first sync = %+v", res)
	}
	id, err := lookup(t, database, ci)
	if err != nil || !id.Scopes.Has("yank:prod") {
		t.Fatalf("ci token: %v %v", id, err)
	}
	if _, err := lookup(t, database, ro); err != nil {
		t.Fatalf("sha256-provisioned token: %v", err)
	}

	// Unchanged config: nothing to do.
	res, err = auth.SyncConfigTokens(ctx, database.DB, tokens)
	if err != nil || res.Created != nil || res.Updated != nil || res.Revoked != nil {
		t.Errorf("idempotent sync = %+v, %v", res, err)
	}

	// Rescope ci, rotate readonly's secret.
	ro2, _ := auth.GenerateToken()
	tokens[0].Scopes = "publish:prod"
	tokens[1].SHA256File = writeSecret(t, "ro2.sha256", auth.HashToken(ro2))
	res, err = auth.SyncConfigTokens(ctx, database.DB, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Updated, []string{"ci"}) || !reflect.DeepEqual(res.Created, []string{"readonly"}) ||
		!reflect.DeepEqual(res.Revoked, []string{"readonly"}) {
		t.Errorf("rescope+rotate = %+v", res)
	}
	if id, _ := lookup(t, database, ci); id.Scopes.Has("yank:prod") {
		t.Error("ci kept the removed yank:prod scope")
	}
	if _, err := lookup(t, database, ro); !errors.Is(err, auth.ErrTokenNotFound) {
		t.Errorf("rotated-out secret still valid: %v", err)
	}

	// Drop everything from config: config tokens revoked, API token kept.
	res, err = auth.SyncConfigTokens(ctx, database.DB, nil)
	if err != nil || len(res.Revoked) != 2 {
		t.Errorf("empty sync = %+v, %v", res, err)
	}
	if _, err := lookup(t, database, ci); !errors.Is(err, auth.ErrTokenNotFound) {
		t.Errorf("removed config token still valid: %v", err)
	}
	if _, err := lookup(t, database, apiToken); err != nil {
		t.Errorf("API token touched by sync: %v", err)
	}

	// Re-adding a previously removed secret revives it.
	res, err = auth.SyncConfigTokens(ctx, database.DB, tokens[:1])
	if err != nil || !reflect.DeepEqual(res.Updated, []string{"ci"}) {
		t.Errorf("re-add = %+v, %v", res, err)
	}
	if _, err := lookup(t, database, ci); err != nil {
		t.Errorf("re-added token not valid: %v", err)
	}
}

func TestSyncConfigTokensRejects(t *testing.T) {
	t.Parallel()
	database := setupTokenDB(t)
	apiToken := seedToken(t, database, "minted", "admin", false)
	good, _ := auth.GenerateToken()

	cases := []struct {
		name string
		tok  config.TokenConfig
		want string
	}{
		{"missing file", config.TokenConfig{Label: "a", Scopes: "admin", TokenFile: "/nonexistent/x"}, "no such file"},
		{"no prefix", config.TokenConfig{Label: "a", Scopes: "admin", TokenFile: writeSecret(t, "a", "hunter2")}, "must start with"},
		{"bad hash", config.TokenConfig{Label: "a", Scopes: "admin", SHA256File: writeSecret(t, "b", "abc")}, "64-character hex"},
		{"bad scope", config.TokenConfig{Label: "a", Scopes: "Publish:prod", TokenFile: writeSecret(t, "c", good)}, "invalid scope"},
		{"collides with API token", config.TokenConfig{Label: "a", Scopes: "admin", TokenFile: writeSecret(t, "d", apiToken)}, "already registered as API token"},
	}
	for _, c := range cases {
		_, err := auth.SyncConfigTokens(context.Background(), database.DB, []config.TokenConfig{c.tok})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
