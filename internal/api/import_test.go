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

// newAttachTestDeps builds a Deps with a known cell wired into Matrix
// so AttachBinaries can validate against it.
func newAttachTestDeps(t *testing.T, channel, policy string) Deps {
	t.Helper()
	deps := newImportTestDeps(t, channel, policy)
	deps.Matrix = &config.MatrixConfig{
		Cells: []config.Cell{
			{
				Name:      "rhel9-amd64-r-4.4",
				OS:        "linux",
				OSVersion: "rhel9",
				Arch:      "amd64",
				RMinor:    "4.4",
			},
		},
	}
	return deps
}

// seedSourceRow inserts a (channel, name, version) row directly to set
// up an AttachBinaries scenario without re-running the source import.
func seedSourceRow(t *testing.T, deps Deps, channel, name, version, sha string) {
	t.Helper()
	if _, err := deps.DB.ExecContext(context.Background(), `
		INSERT INTO packages(channel, name, version, source_sha256, source_size)
		VALUES (?, ?, ?, ?, ?)
	`, channel, name, version, sha, len(sha)); err != nil {
		t.Fatalf("seed package row: %v", err)
	}
}

func TestAttachBinariesCreatesBinaryRow(t *testing.T) {
	deps := newAttachTestDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable)
	seedSourceRow(t, deps, "cran-r4.4-2026q2", "ggplot2", "3.5.1", "abc")

	resp, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2",
		Name:    "ggplot2",
		Version: "3.5.1",
		Cell:    "rhel9-amd64-r-4.4",
		Binary:  strings.NewReader("rhel9-binary-bytes"),
		Actor:   "import-bundle",
		Note:    "bundle bin/linux/rhel9-amd64-r-4.4/ggplot2_3.5.1.tar.gz",
	})
	if err != nil {
		t.Fatalf("AttachBinaries: %v", err)
	}
	if resp.AlreadyExisted || resp.Overwritten {
		t.Errorf("fresh attach should be created; got %+v", resp)
	}
	if len(resp.Binaries) != 1 || resp.Binaries[0].Cell != "rhel9-amd64-r-4.4" {
		t.Errorf("response binaries = %+v; want one for rhel9", resp.Binaries)
	}

	var n int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM binaries WHERE cell = ?`, "rhel9-amd64-r-4.4").Scan(&n); err != nil || n != 1 {
		t.Fatalf("expected 1 binary row; got %d (err %v)", n, err)
	}

	var evType string
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT type FROM events WHERE channel = ? AND package = ? ORDER BY id DESC LIMIT 1`,
		"cran-r4.4-2026q2", "ggplot2").Scan(&evType); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if evType != "import_binary" {
		t.Errorf("event type = %q; want import_binary", evType)
	}
}

func TestAttachBinariesSourceRowMissing(t *testing.T) {
	deps := newAttachTestDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable)
	// No seedSourceRow — channel exists but no packages row.
	_, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2",
		Name:    "ggplot2",
		Version: "3.5.1",
		Cell:    "rhel9-amd64-r-4.4",
		Binary:  strings.NewReader("x"),
	})
	if !errors.Is(err, ErrSourceRowMissing) {
		t.Fatalf("want ErrSourceRowMissing; got %v", err)
	}
}

func TestAttachBinariesIdempotentOnIdenticalBytes(t *testing.T) {
	deps := newAttachTestDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable)
	seedSourceRow(t, deps, "cran-r4.4-2026q2", "ggplot2", "3.5.1", "abc")

	body := "binary-bytes"
	if _, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader(body),
	}); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	resp, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader(body),
	})
	if err != nil {
		t.Fatalf("second attach: %v", err)
	}
	if !resp.AlreadyExisted {
		t.Errorf("expected AlreadyExisted=true on idempotent replay; got %+v", resp)
	}
}

func TestAttachBinariesImmutableConflictOnDifferentBytes(t *testing.T) {
	deps := newAttachTestDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable)
	seedSourceRow(t, deps, "cran-r4.4-2026q2", "ggplot2", "3.5.1", "abc")

	if _, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader("first"),
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "cran-r4.4-2026q2", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader("second"),
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("want ErrImmutableConflict; got %v", err)
	}
}

func TestAttachBinariesMutableReplace(t *testing.T) {
	deps := newAttachTestDeps(t, "dev", config.PolicyMutable)
	seedSourceRow(t, deps, "dev", "ggplot2", "3.5.1", "abc")

	if _, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "dev", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader("first"),
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	resp, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "dev", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader("second"),
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !resp.Overwritten {
		t.Errorf("expected Overwritten=true; got %+v", resp)
	}
	var n int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM binaries WHERE cell = ?`, "rhel9-amd64-r-4.4").Scan(&n); err != nil || n != 1 {
		t.Errorf("want exactly 1 binary row after replace; got %d (err %v)", n, err)
	}
}

func TestAttachBinariesRejectsUnknownCell(t *testing.T) {
	deps := newAttachTestDeps(t, "dev", config.PolicyMutable)
	seedSourceRow(t, deps, "dev", "ggplot2", "3.5.1", "abc")

	_, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "dev", Name: "ggplot2", Version: "3.5.1",
		Cell: "ubuntu-99-amd64-r-9.9", Binary: strings.NewReader("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "not declared in matrix.yaml") {
		t.Errorf("want matrix-rejection error; got %v", err)
	}
}

func TestAttachBinariesRejectsNilMatrix(t *testing.T) {
	// Use newImportTestDeps so Matrix stays nil.
	deps := newImportTestDeps(t, "dev", config.PolicyMutable)
	seedSourceRow(t, deps, "dev", "ggplot2", "3.5.1", "abc")

	_, err := AttachBinaries(context.Background(), deps, AttachInput{
		Channel: "dev", Name: "ggplot2", Version: "3.5.1",
		Cell: "rhel9-amd64-r-4.4", Binary: strings.NewReader("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "no matrix config") {
		t.Errorf("want no-matrix-config error; got %v", err)
	}
}
