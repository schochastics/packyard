package config_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
)

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(context.Background(), database); err != nil {
		t.Fatalf("MigrateEmbedded: %v", err)
	}
	return database
}

func mustDecodeChannels(t *testing.T, yaml string) *config.ChannelsConfig {
	t.Helper()
	cfg, err := config.DecodeChannels(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("DecodeChannels: %v", err)
	}
	return cfg
}

func TestReconcileChannelsCreatesAll(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	cfg := mustDecodeChannels(t, `
channels:
  - name: dev
    overwrite_policy: mutable
  - name: test
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
`)

	result, err := config.ReconcileChannels(context.Background(), database.DB, cfg)
	if err != nil {
		t.Fatalf("ReconcileChannels: %v", err)
	}
	if len(result.Created) != 3 {
		t.Errorf("Created = %v, want 3 names", result.Created)
	}
	if len(result.Updated)+len(result.Unchanged)+len(result.Obsolete) != 0 {
		t.Errorf("unexpected diff on fresh DB: %+v", result)
	}
	assertDefault(t, database.DB, "prod")
}

func TestReconcileChannelsIdempotent(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	cfg := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
`)
	ctx := context.Background()

	if _, err := config.ReconcileChannels(ctx, database.DB, cfg); err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := config.ReconcileChannels(ctx, database.DB, cfg)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(second.Created) != 0 {
		t.Errorf("second pass created %v, want none", second.Created)
	}
	if len(second.Updated) != 0 {
		t.Errorf("second pass updated %v, want none", second.Updated)
	}
	if len(second.Unchanged) != 1 || second.Unchanged[0] != "prod" {
		t.Errorf("Unchanged = %v, want [prod]", second.Unchanged)
	}
}

func TestReconcileChannelsUpdatesPolicy(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	// Seed with mutable prod.
	before := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: mutable
    default: true
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, before); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Harden to immutable.
	after := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
`)
	result, err := config.ReconcileChannels(ctx, database.DB, after)
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "prod" {
		t.Errorf("Updated = %v, want [prod]", result.Updated)
	}

	var policy string
	if err := database.QueryRowContext(ctx,
		`SELECT overwrite_policy FROM channels WHERE name='prod'`).Scan(&policy); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if policy != "immutable" {
		t.Errorf("overwrite_policy = %q, want immutable", policy)
	}
}

func TestReconcileChannelsMovesDefault(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	before := mustDecodeChannels(t, `
channels:
  - name: dev
    overwrite_policy: mutable
    default: true
  - name: prod
    overwrite_policy: immutable
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, before); err != nil {
		t.Fatalf("seed: %v", err)
	}
	assertDefault(t, database.DB, "dev")

	after := mustDecodeChannels(t, `
channels:
  - name: dev
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
`)
	result, err := config.ReconcileChannels(ctx, database.DB, after)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	assertDefault(t, database.DB, "prod")

	// Both names should appear in Updated (dev lost default, prod gained it).
	haveDev, haveProd := false, false
	for _, n := range result.Updated {
		if n == "dev" {
			haveDev = true
		}
		if n == "prod" {
			haveProd = true
		}
	}
	if !haveDev || !haveProd {
		t.Errorf("Updated = %v, want to contain both dev and prod", result.Updated)
	}
}

func TestReconcileChannelsReportsObsolete(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	// Seed with two channels.
	before := mustDecodeChannels(t, `
channels:
  - name: legacy
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, before); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Operator removed "legacy" from YAML. We must report, not delete.
	after := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
`)
	result, err := config.ReconcileChannels(ctx, database.DB, after)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}

	if len(result.Obsolete) != 1 || result.Obsolete[0] != "legacy" {
		t.Errorf("Obsolete = %v, want [legacy]", result.Obsolete)
	}

	// "legacy" must still be in the DB.
	var count int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channels WHERE name='legacy'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("legacy channel count = %d, want 1 (preserved, not deleted)", count)
	}
}

func TestReconcileChannelsPersistsKindAndUpstream(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	cfg := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, cfg); err != nil {
		t.Fatalf("ReconcileChannels: %v", err)
	}

	var (
		kind     string
		upstream sql.NullString
	)
	if err := database.QueryRowContext(ctx,
		`SELECT kind, upstream_url FROM channels WHERE name='cran'`).Scan(&kind, &upstream); err != nil {
		t.Fatalf("read cran: %v", err)
	}
	if kind != "proxy" || !upstream.Valid || upstream.String != "https://cloud.r-project.org" {
		t.Errorf("cran row = (kind=%q upstream=%v); want (proxy, https://cloud.r-project.org)", kind, upstream)
	}

	// prod (local) must have NULL upstream_url and kind=local.
	if err := database.QueryRowContext(ctx,
		`SELECT kind, upstream_url FROM channels WHERE name='prod'`).Scan(&kind, &upstream); err != nil {
		t.Fatalf("read prod: %v", err)
	}
	if kind != "local" || upstream.Valid {
		t.Errorf("prod row = (kind=%q upstream=%v); want (local, NULL)", kind, upstream)
	}
}

func TestReconcileChannelsUpdatesUpstreamURL(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	before := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, before); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Move to a pinned PPM snapshot.
	after := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://packagemanager.posit.co/cran/2026-01-15
`)
	result, err := config.ReconcileChannels(ctx, database.DB, after)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "cran" {
		t.Errorf("Updated = %v, want [cran]", result.Updated)
	}

	var upstream sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT upstream_url FROM channels WHERE name='cran'`).Scan(&upstream); err != nil {
		t.Fatalf("read upstream: %v", err)
	}
	if !upstream.Valid || upstream.String != "https://packagemanager.posit.co/cran/2026-01-15" {
		t.Errorf("upstream_url = %v, want the pinned snapshot URL", upstream)
	}
}

func TestReconcileChannelsRefusesKindChangeWhenPopulated(t *testing.T) {
	t.Parallel()

	database := setupDB(t)
	ctx := context.Background()

	// Seed a local channel and give it a package row.
	seed := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: legacy
    overwrite_policy: mutable
`)
	if _, err := config.ReconcileChannels(ctx, database.DB, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size)
		VALUES ('legacy', 'somepkg', '1.0.0', 'deadbeef', 100)
	`); err != nil {
		t.Fatalf("insert package: %v", err)
	}

	// Try to flip the populated channel from local to proxy.
	after := mustDecodeChannels(t, `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: legacy
    overwrite_policy: mutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
`)
	_, err := config.ReconcileChannels(ctx, database.DB, after)
	if err == nil {
		t.Fatal("expected error for kind change on populated channel; got nil")
	}
	if !strings.Contains(err.Error(), "kind change") {
		t.Errorf("error = %v, want one mentioning kind change", err)
	}
}

func TestReconcileChannelsNilConfigFails(t *testing.T) {
	t.Parallel()
	database := setupDB(t)
	if _, err := config.ReconcileChannels(context.Background(), database.DB, nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func assertDefault(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(),
		`SELECT name FROM channels WHERE is_default=1`).Scan(&got); err != nil {
		t.Fatalf("read default: %v", err)
	}
	if got != want {
		t.Errorf("default channel = %q, want %q", got, want)
	}
}
