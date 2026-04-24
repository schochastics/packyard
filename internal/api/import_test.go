package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/config"
)

func newImportTestDeps(t *testing.T, channel, policy string) Deps {
	t.Helper()
	deps := newAuthTestDeps(t)
	if _, err := deps.DB.ExecContext(context.Background(),
		`INSERT INTO channels(name, overwrite_policy) VALUES (?, ?)`, channel, policy,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return deps
}

func TestImportSourceCreatesPackageAndEvent(t *testing.T) {
	deps := newImportTestDeps(t, "dev", config.PolicyMutable)
	ctx := context.Background()

	resp, err := ImportSource(ctx, deps, ImportInput{
		Channel: "dev",
		Name:    "foo",
		Version: "1.0.0",
		Source:  strings.NewReader("fake tarball bytes"),
		Actor:   "import-drat",
		Note:    "https://example.org/drat",
	})
	if err != nil {
		t.Fatalf("ImportSource: %v", err)
	}
	if resp.AlreadyExisted || resp.Overwritten {
		t.Errorf("fresh import should be Created; got %+v", resp)
	}

	// The package row is there.
	var count int
	if err := deps.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM packages WHERE channel='dev' AND name='foo' AND version='1.0.0'`,
	).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected 1 package row, got %d (err %v)", count, err)
	}

	// Both a publish event and the import annotation event are logged.
	var types []string
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT type FROM events WHERE channel='dev' AND package='foo' ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, s)
	}
	want := []string{"publish", "import"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("event types = %v; want %v", types, want)
	}
}

func TestImportSourceImmutableConflictOnDifferentBytes(t *testing.T) {
	deps := newImportTestDeps(t, "prod", config.PolicyImmutable)
	ctx := context.Background()

	if _, err := ImportSource(ctx, deps, ImportInput{
		Channel: "prod", Name: "foo", Version: "1.0.0",
		Source: strings.NewReader("original bytes"),
	}); err != nil {
		t.Fatalf("first import: %v", err)
	}

	_, err := ImportSource(ctx, deps, ImportInput{
		Channel: "prod", Name: "foo", Version: "1.0.0",
		Source: strings.NewReader("different bytes"),
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("want ErrImmutableConflict; got %v", err)
	}
}

func TestImportSourceIdempotentOnIdenticalBytes(t *testing.T) {
	deps := newImportTestDeps(t, "prod", config.PolicyImmutable)
	ctx := context.Background()

	body := "consistent bytes"
	if _, err := ImportSource(ctx, deps, ImportInput{
		Channel: "prod", Name: "foo", Version: "1.0.0",
		Source: strings.NewReader(body),
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	resp, err := ImportSource(ctx, deps, ImportInput{
		Channel: "prod", Name: "foo", Version: "1.0.0",
		Source: strings.NewReader(body),
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !resp.AlreadyExisted {
		t.Errorf("expected AlreadyExisted=true on idempotent replay; got %+v", resp)
	}
}

func TestImportSourceRejectsUnknownChannel(t *testing.T) {
	deps := newAuthTestDeps(t)
	_, err := ImportSource(context.Background(), deps, ImportInput{
		Channel: "nope", Name: "foo", Version: "1.0.0",
		Source: strings.NewReader("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "channel \"nope\" not found") {
		t.Errorf("want channel-not-found error; got %v", err)
	}
}

func TestImportSourceValidatesNameAndVersion(t *testing.T) {
	deps := newImportTestDeps(t, "dev", config.PolicyMutable)
	_, err := ImportSource(context.Background(), deps, ImportInput{
		Channel: "dev", Name: "9-bad-start", Version: "1.0.0",
		Source: strings.NewReader("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid package name") {
		t.Errorf("want invalid-name error; got %v", err)
	}
	_, err = ImportSource(context.Background(), deps, ImportInput{
		Channel: "dev", Name: "foo", Version: "not-a-version",
		Source: strings.NewReader("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Errorf("want invalid-version error; got %v", err)
	}
}
