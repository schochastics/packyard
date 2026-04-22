package importers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/api"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/db"
	"github.com/schochastics/pakman/internal/importers"
)

// mockDrat stands up a tiny HTTP server that mimics a drat/CRAN-shaped
// repo. PACKAGES lists the map keys; each key is served as its own
// tarball at /src/contrib/<file>.
func mockDrat(t *testing.T, pkgs map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for filename := range pkgs {
			name, version := splitDratFilename(filename)
			b.WriteString("Package: " + name + "\n")
			b.WriteString("Version: " + version + "\n\n")
		}
		_, _ = w.Write([]byte(b.String()))
	})
	for filename, body := range pkgs {
		filename, body := filename, body
		mux.HandleFunc("/src/contrib/"+filename, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
	}
	return httptest.NewServer(mux)
}

func splitDratFilename(fn string) (string, string) {
	// e.g. "foo_1.0.0.tar.gz" -> ("foo", "1.0.0")
	base := strings.TrimSuffix(fn, ".tar.gz")
	i := strings.Index(base, "_")
	return base[:i], base[i+1:]
}

func newImportDeps(t *testing.T, channel, policy string) api.Deps {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(dir, "pakman.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES (?, ?)`, channel, policy,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return api.Deps{DB: database, CAS: store}
}

func TestDratImporterSuccess(t *testing.T) {
	drat := mockDrat(t, map[string]string{
		"foo_1.0.0.tar.gz": "foo-1.0.0-bytes",
		"bar_0.2.1.tar.gz": "bar-0.2.1-bytes",
	})
	t.Cleanup(drat.Close)

	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewDratImporter(deps, "dev")

	res, err := imp.Run(context.Background(), drat.URL, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Imported) != 2 {
		t.Errorf("Imported = %v; want 2 entries", res.Imported)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v; want empty", res.Failed)
	}

	var count int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages WHERE channel='dev'`,
	).Scan(&count); err != nil || count != 2 {
		t.Errorf("packages count = %d (err %v); want 2", count, err)
	}
}

func TestDratImporterIdempotent(t *testing.T) {
	drat := mockDrat(t, map[string]string{
		"foo_1.0.0.tar.gz": "same-bytes",
	})
	t.Cleanup(drat.Close)

	// Immutable channel so a re-run hits the AlreadyExisted path.
	deps := newImportDeps(t, "prod", config.PolicyImmutable)
	imp := importers.NewDratImporter(deps, "prod")

	if _, err := imp.Run(context.Background(), drat.URL, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := imp.Run(context.Background(), drat.URL, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("Skipped = %v; want 1", res.Skipped)
	}
	if len(res.Imported) != 0 {
		t.Errorf("Imported = %v; should be empty on replay", res.Imported)
	}
}

func TestDratImporterTarballFetchFailsRecorded(t *testing.T) {
	// PACKAGES lists foo but we don't register the tarball route, so
	// the GET returns 404 and that single package should end up in Failed.
	mux := http.NewServeMux()
	mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Package: foo\nVersion: 1.0.0\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewDratImporter(deps, "dev")
	res, err := imp.Run(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("Failed = %v; want 1 entry", res.Failed)
	}
	if res.Failed[0].Package != "foo" {
		t.Errorf("Failed[0].Package = %q; want foo", res.Failed[0].Package)
	}
}

func TestDratImporterEmptyPackagesIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(""))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewDratImporter(deps, "dev")
	_, err := imp.Run(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatalf("expected error on empty PACKAGES")
	}
}
