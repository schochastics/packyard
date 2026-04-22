package importers_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/importers"
)

func TestGitImporterHappyPath(t *testing.T) {
	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewGitImporter(deps, "dev")

	// Fake Clone: write a DESCRIPTION into dest.
	imp.Clone = func(ctx context.Context, repoURL, branch, dest string) error {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "DESCRIPTION"),
			[]byte("Package: foo\nVersion: 1.2.3\nTitle: fake\n"), 0o644)
	}
	// Fake Build: write a fake tarball next to the source dir.
	imp.Build = func(ctx context.Context, sourceDir string) (string, error) {
		tar := filepath.Join(sourceDir, "..", "foo_1.2.3.tar.gz")
		return tar, os.WriteFile(tar, []byte("pretend this is a source tarball"), 0o644)
	}

	res, err := imp.Run(context.Background(), "https://example.org/foo.git", "main", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Package != "foo" || res.Version != "1.2.3" {
		t.Errorf("got %s@%s; want foo@1.2.3", res.Package, res.Version)
	}
	if res.Response.AlreadyExisted {
		t.Errorf("fresh import should not be AlreadyExisted")
	}
}

func TestGitImporterCloneFailurePropagates(t *testing.T) {
	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewGitImporter(deps, "dev")
	imp.Clone = func(ctx context.Context, repoURL, branch, dest string) error {
		return context.DeadlineExceeded
	}
	imp.Build = func(ctx context.Context, sourceDir string) (string, error) {
		t.Fatalf("Build should not be called when Clone fails")
		return "", nil
	}

	_, err := imp.Run(context.Background(), "x", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "clone") {
		t.Errorf("want clone-failure error; got %v", err)
	}
}

func TestGitImporterMissingDescriptionFails(t *testing.T) {
	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewGitImporter(deps, "dev")
	imp.Clone = func(ctx context.Context, repoURL, branch, dest string) error {
		return os.MkdirAll(dest, 0o755) // no DESCRIPTION written
	}

	_, err := imp.Run(context.Background(), "x", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "DESCRIPTION") {
		t.Errorf("want DESCRIPTION error; got %v", err)
	}
}

func TestGitImporterBuildFailurePropagates(t *testing.T) {
	deps := newImportDeps(t, "dev", config.PolicyMutable)
	imp := importers.NewGitImporter(deps, "dev")
	imp.Clone = func(ctx context.Context, repoURL, branch, dest string) error {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "DESCRIPTION"),
			[]byte("Package: foo\nVersion: 1.0.0\n"), 0o644)
	}
	imp.Build = func(ctx context.Context, sourceDir string) (string, error) {
		return "", context.Canceled
	}
	_, err := imp.Run(context.Background(), "x", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "build") {
		t.Errorf("want build-failure error; got %v", err)
	}
}
