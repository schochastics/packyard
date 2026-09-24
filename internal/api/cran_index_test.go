package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
)

func setupIndexDB(t *testing.T) *db.DB {
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

	body, _, err := idx.GetSource(context.Background(), "dev", nil, nil)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("empty channel should yield empty body, got %q", body)
	}
}

func TestGetSourceListsLatestNonYankedOnly(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	// alpha: 1.10.0 must beat 1.9.0 (numeric, not lexical ordering).
	seedPackage(t, database, "dev", "alpha", "1.9.0", false)
	seedPackage(t, database, "dev", "alpha", "1.10.0", false)
	// beta: newest version yanked → the next-highest is listed.
	seedPackage(t, database, "dev", "beta", "2.0.0", true)
	seedPackage(t, database, "dev", "beta", "1.0.0", false)
	// gamma: every version yanked → drops out of the index.
	seedPackage(t, database, "dev", "gamma", "1.0.0", true)

	idx := NewIndex(database.DB)
	body, _, err := idx.GetSource(context.Background(), "dev", nil, nil)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	want := "Package: alpha\nVersion: 1.10.0\n\nPackage: beta\nVersion: 1.0.0\n"
	if string(body) != want {
		t.Errorf("PACKAGES =\n%s\nwant\n%s", body, want)
	}
}

func TestGetSourceIncludesDescriptionFields(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	id := seedPackage(t, database, "dev", "fsapi", "1.2.0", false)
	if _, err := database.ExecContext(context.Background(),
		`UPDATE packages SET index_fields = ? WHERE id = ?`,
		`{"Depends":"R (>= 4.1.0)","Imports":"fsdb (>= 2.1.16), fsutils","License":"MIT"}`, id); err != nil {
		t.Fatal(err)
	}

	idx := NewIndex(database.DB)
	body, _, err := idx.GetSource(context.Background(), "dev", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Package: fsapi\nVersion: 1.2.0\nDepends: R (>= 4.1.0)\nImports: fsdb (>= 2.1.16), fsutils\nLicense: MIT\n"
	if string(body) != want {
		t.Errorf("PACKAGES =\n%s\nwant\n%s", body, want)
	}
}

func TestInvalidateChannelForcesRebuild(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)

	idx := NewIndex(database.DB)
	first, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)

	// Add a package directly; without invalidation the cache hides it.
	seedPackage(t, database, "dev", "beta", "2.0.0", false)
	cached, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
	if string(cached) != string(first) {
		t.Error("cache did not serve stale (pre-invalidation) body")
	}

	idx.InvalidateChannel("dev")
	fresh, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
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
	devBefore, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
	prodBefore, _, _ := idx.GetSource(context.Background(), "prod", nil, nil)

	// Mutate prod, invalidate prod. dev cache must remain.
	seedPackage(t, database, "prod", "gamma", "2.0.0", false)
	idx.InvalidateChannel("prod")

	prodAfter, _, _ := idx.GetSource(context.Background(), "prod", nil, nil)
	devAfter, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)

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

	first, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
	seedPackage(t, database, "dev", "beta", "2.0.0", false)

	// Immediately the cache hides beta.
	cached, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
	if string(cached) != string(first) {
		t.Error("expected stale cache within TTL")
	}

	time.Sleep(20 * time.Millisecond)
	fresh, _, _ := idx.GetSource(context.Background(), "dev", nil, nil)
	if !strings.Contains(string(fresh), "beta") {
		t.Errorf("TTL expired but body still stale: %q", fresh)
	}
}

func TestGetLinuxMixesBinaryAndSourceEntries(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	alphaID := seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	seedPackage(t, database, "dev", "beta", "1.0.0", false) // source-only
	seedBinary(t, database, alphaID, "r-4.4")
	if _, err := database.ExecContext(context.Background(),
		`UPDATE binaries SET built = 'R 4.4.3; ; 2026-09-01 10:00:00 UTC; unix' WHERE package_id = ?`, alphaID); err != nil {
		t.Fatal(err)
	}
	gammaID := seedPackage(t, database, "dev", "gamma", "1.0.0", false)
	seedBinary(t, database, gammaID, "r-4.4") // built unknown → synthesized

	idx := NewIndex(database.DB)
	cell := &config.Cell{Name: "r-4.4", RMinor: "4.4"}
	body, _, err := idx.GetLinux(context.Background(), "dev", cell, "amd64", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Package: alpha\nVersion: 1.0.0\nBuilt: R 4.4.3; ; 2026-09-01 10:00:00 UTC; unix\n\n" +
		"Package: beta\nVersion: 1.0.0\n\n" +
		"Package: gamma\nVersion: 1.0.0\nBuilt: R 4.4.0; x86_64-pc-linux-gnu; ; unix\n"
	if string(body) != want {
		t.Errorf("linux PACKAGES =\n%s\nwant\n%s", body, want)
	}

	// No cell for the client's R version: everything is a source entry.
	src, _, err := idx.GetLinux(context.Background(), "dev", nil, "amd64", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "Built:") {
		t.Errorf("source view carries Built: %q", src)
	}
}

func TestGetLinuxBinaryOfOlderVersionIgnored(t *testing.T) {
	t.Parallel()

	// The latest version decides; a binary that exists only for an
	// older version must not be advertised for the latest.
	database := setupIndexDB(t)
	oldID := seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	seedBinary(t, database, oldID, "r-4.4")
	seedPackage(t, database, "dev", "alpha", "1.1.0", false)

	idx := NewIndex(database.DB)
	body, _, err := idx.GetLinux(context.Background(), "dev", &config.Cell{Name: "r-4.4", RMinor: "4.4"}, "amd64", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Package: alpha\nVersion: 1.1.0\n"; string(body) != want {
		t.Errorf("linux PACKAGES = %q, want %q", body, want)
	}
}

func TestInvalidateChannelDropsLinuxViews(t *testing.T) {
	t.Parallel()

	database := setupIndexDB(t)
	seedPackage(t, database, "dev", "alpha", "1.0.0", false)
	idx := NewIndex(database.DB)
	cell := &config.Cell{Name: "r-4.4", RMinor: "4.4"}
	if _, _, err := idx.GetLinux(context.Background(), "dev", cell, "amd64", nil, nil); err != nil {
		t.Fatal(err)
	}
	seedPackage(t, database, "dev", "beta", "1.0.0", false)
	idx.InvalidateChannel("dev")
	body, _, _ := idx.GetLinux(context.Background(), "dev", cell, "amd64", nil, nil)
	if !strings.Contains(string(body), "Package: beta") {
		t.Errorf("linux view not invalidated: %q", body)
	}
}

// A body built from a DB read that raced with an invalidation must not
// be cached: the next reader has to see the post-invalidation state.
func TestIndexDropsBodyBuiltBeforeInvalidation(t *testing.T) {
	t.Parallel()
	idx := NewIndex(nil)
	key := sourceKey("dev")

	gen := idx.generation("dev")
	idx.InvalidateChannel("dev") // a publish commits mid-rebuild
	idx.storeIfCurrent(key, "dev", gen, []byte("stale"))
	if _, ok := idx.lookup(key); ok {
		t.Fatal("body built before the invalidation was cached")
	}

	gen = idx.generation("dev")
	idx.storeIfCurrent(key, "dev", gen, []byte("fresh"))
	if body, ok := idx.lookup(key); !ok || string(body) != "fresh" {
		t.Fatalf("current body not cached: %q %v", body, ok)
	}

	// Other channels are unaffected.
	other := idx.generation("prod")
	idx.InvalidateChannel("dev")
	idx.storeIfCurrent(sourceKey("prod"), "prod", other, []byte("p"))
	if _, ok := idx.lookup(sourceKey("prod")); !ok {
		t.Error("invalidating dev dropped a prod rebuild")
	}
}
