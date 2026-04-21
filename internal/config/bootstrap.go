package config

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// defaultConfigFS holds the channels.yaml and matrix.yaml that ship
// inside the pakman binary. First-run bootstrap copies these onto
// disk; subsequent runs read the on-disk files so operators can edit
// them without rebuilding.
//
//go:embed defaults/*.yaml
var defaultConfigFS embed.FS

// BootstrapResult reports which default files were written during a
// bootstrap pass. Files that already existed are NOT rewritten — we
// never silently overwrite an operator's customized config.
type BootstrapResult struct {
	Written []string // absolute paths written in this pass
	Skipped []string // absolute paths that already existed
}

// BootstrapDefaults ensures channels.yaml and matrix.yaml exist under
// dataDir. Missing files are created from the embedded defaults;
// existing files are left exactly as they are, regardless of content.
//
// Idempotent: running twice against the same data dir is a no-op on
// the second pass.
func BootstrapDefaults(dataDir string) (BootstrapResult, error) {
	var result BootstrapResult

	if dataDir == "" {
		return result, errors.New("bootstrap: empty data dir")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return result, fmt.Errorf("bootstrap: ensure data dir: %w", err)
	}

	entries, err := fs.ReadDir(defaultConfigFS, "defaults")
	if err != nil {
		return result, fmt.Errorf("bootstrap: read embedded defaults: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		dst := filepath.Join(dataDir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			result.Skipped = append(result.Skipped, dst)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("bootstrap: stat %q: %w", dst, err)
		}

		body, err := fs.ReadFile(defaultConfigFS, filepath.Join("defaults", e.Name()))
		if err != nil {
			return result, fmt.Errorf("bootstrap: read embedded %q: %w", e.Name(), err)
		}
		// 0o644 so operators can read the file without sudo when debugging.
		// This is a bootstrap helper for config files, not secrets; the
		// gosec recommendation of 0o600 would require sudo to view.
		if err := os.WriteFile(dst, body, 0o644); err != nil { //nolint:gosec // config file, not secret
			return result, fmt.Errorf("bootstrap: write %q: %w", dst, err)
		}
		result.Written = append(result.Written, dst)
	}

	return result, nil
}

// EmbeddedDefault returns the bytes of one of the shipped default files.
// name is the base filename (e.g. "channels.yaml"). Used by tests and
// by tooling that wants to compare a modified file against the default.
func EmbeddedDefault(name string) ([]byte, error) {
	return fs.ReadFile(defaultConfigFS, filepath.Join("defaults", name))
}
