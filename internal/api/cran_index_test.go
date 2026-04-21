package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schochastics/pakman/internal/db"
)

func setupIndexDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "pakman.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Seed channels + matrix cell via direct SQL so we don't drag in the
	// config package for a simple unit test.
	if _, err := database.ExecContext(ctx, `
		INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('dev','mutable',0);
	`); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return database
}

func seedPackage(t *testing.T, database *db.DB, channel, name, version string, yanked bool) int64 {
	t.Helper()
	var y int
	if yanked {
		y = 1
	}
	res, err := database.ExecContext(context.Background(), `
		INSERT INTO packages(channel, name, version, source_sha256, source_size, yanked)
		VALUES (?, ?, ?, ?, ?, ?)
	`, channel, name, version, "deadbeef", 42, y)
	if err != nil {
		t.Fatalf("seed package: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedBinary(t *testing.T, database *db.DB, pkgID int64, cell string) {
	t.Helper()
	_, err := database.ExecContext(context.Background(), `
		INSERT INTO binaries(package_id, cell, binary_sha256, size)
		VALUES (?, ?, ?, ?)
	`, pkgID, cell, "cafebabe", 21)
	if err != nil {
		t.Fatalf("seed binary: %v", err)
	}
}

func TestGetSourceEmptyChannel(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	idx := NewIndex(database.DB)

	body, err := idx.GetSource(context.Background(), "dev")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("empty channel should yield empty body, got %q", body)
	}
}

func TestGetSourceStanzasIncludeYanked(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	seedPackage(t, database, "dev", "beta", "2.0.0", true)
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	seedPackage(t, database, "dev", "alpha", "1.1.0", false)

	idx := NewIndex(database.DB)
	body, err := idx.GetSource(context.Background(), "dev")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	s := string(body)

	// Three stanzas, sorted alpha by name.
	if !strings.Contains(s, "Package: alpha\nVersion: 1.0.0\n") {
		t.Errorf("alpha 1.0.0 stanza missing: %q", s)
	}
	if !strings.Contains(s, "Package: alpha\nVersion: 1.1.0\n") {
		t.Errorf("alpha 1.1.0 stanza missing: %q", s)
	}
	if !strings.Contains(s, "Package: beta\nVersion: 2.0.0\nYanked: yes\n") {
		t.Errorf("beta yanked stanza missing or malformed: %q", s)
	}
	// alpha stanzas come before beta
	if strings.Index(s, "Package: alpha") >= strings.Index(s, "Package: beta") {
		t.Error("stanzas not sorted by name")
	}
}

func TestInvalidateChannelForcesRebuild(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)

	idx := NewIndex(database.DB)
	first, _ := idx.GetSource(context.Background(), "dev")

	// Add a package directly; without invalidation the cache hides it.
	seedPackage(t, database, "dev", "beta", "2.0.0", false)
	cached, _ := idx.GetSource(context.Background(), "dev")
	if string(cached) != string(first) {
		t.Error("cache did not serve stale (pre-invalidation) body")
	}

	idx.InvalidateChannel("dev")
	fresh, _ := idx.GetSource(context.Background(), "dev")
	if !strings.Contains(string(fresh), "Package: beta") {
		t.Errorf("after invalidation, beta missing: %q", fresh)
	}
}

func TestInvalidateChannelScopedPerChannel(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	_, err := database.ExecContext(context.Background(),
		`INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('prod','immutable',1)`)
	if err != nil {
		t.Fatal(err)
	}
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	seedPackage(t, database, "prod", "gamma", "1.0.0", false)

	idx := NewIndex(database.DB)
	devBefore, _ := idx.GetSource(context.Background(), "dev")
	prodBefore, _ := idx.GetSource(context.Background(), "prod")

	// Mutate prod, invalidate prod. dev cache must remain.
	seedPackage(t, database, "prod", "gamma", "2.0.0", false)
	idx.InvalidateChannel("prod")

	prodAfter, _ := idx.GetSource(context.Background(), "prod")
	devAfter, _ := idx.GetSource(context.Background(), "dev")

	if string(prodBefore) == string(prodAfter) {
		t.Error("prod cache not refreshed after invalidation")
	}
	if string(devBefore) != string(devAfter) {
		t.Error("dev cache wrongly refreshed by prod invalidation")
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)

	idx := NewIndex(database.DB)
	idx.ttl = 10 * time.Millisecond

	first, _ := idx.GetSource(context.Background(), "dev")
	seedPackage(t, database, "dev", "beta", "2.0.0", false)

	// Immediately the cache hides beta.
	cached, _ := idx.GetSource(context.Background(), "dev")
	if string(cached) != string(first) {
		t.Error("expected stale cache within TTL")
	}

	time.Sleep(20 * time.Millisecond)
	fresh, _ := idx.GetSource(context.Background(), "dev")
	if !strings.Contains(string(fresh), "beta") {
		t.Errorf("TTL expired but body still stale: %q", fresh)
	}
}

func TestGetBinaryOnlyRowsWithBinariesForCell(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	alphaID := seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	seedPackage(t, database, "dev", "beta", "1.0.0", false) // source-only
	seedBinary(t, database, alphaID, "ubuntu-22.04-amd64-r-4.4")

	idx := NewIndex(database.DB)
	body, err := idx.GetBinary(context.Background(), "dev", "ubuntu-22.04-amd64-r-4.4", "4.4")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "Package: alpha") {
		t.Errorf("alpha (which has a binary for this cell) missing: %q", s)
	}
	if strings.Contains(s, "Package: beta") {
		t.Errorf("beta (no binary for this cell) should not appear: %q", s)
	}
	if !strings.Contains(s, "Built: R 4.4.0;") {
		t.Errorf("Built field missing or malformed: %q", s)
	}
}
