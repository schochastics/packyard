package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/schochastics/packyard/internal/config"
)

func TestEmbeddedDefaultsAreValid(t *testing.T) {
	t.Parallel()

	// Sanity check: the channels.yaml and matrix.yaml that ship inside
	// the binary must themselves pass their own validators. If this test
	// ever fails the fix is to edit internal/config/defaults/*.yaml —
	// never to loosen the validators to accommodate broken defaults.
	chanBytes, err := config.EmbeddedDefault("channels.yaml")
	if err != nil {
		t.Fatalf("EmbeddedDefault(channels.yaml): %v", err)
	}
	if _, err := config.DecodeChannels(bytes.NewReader(chanBytes)); err != nil {
		t.Errorf("default channels.yaml fails validation: %v", err)
	}

	matBytes, err := config.EmbeddedDefault("matrix.yaml")
	if err != nil {
		t.Fatalf("EmbeddedDefault(matrix.yaml): %v", err)
	}
	if _, err := config.DecodeMatrix(bytes.NewReader(matBytes)); err != nil {
		t.Errorf("default matrix.yaml fails validation: %v", err)
	}
}

func TestBootstrapDefaultsWritesMissingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	result, err := config.BootstrapDefaults(dir)
	if err != nil {
		t.Fatalf("BootstrapDefaults: %v", err)
	}

	written := append([]string(nil), result.Written...)
	sort.Strings(written)
	wantFiles := []string{"channels.yaml", "matrix.yaml"}
	for i, base := range wantFiles {
		if i >= len(written) {
			t.Fatalf("fewer files written than expected: %v", written)
		}
		if filepath.Base(written[i]) != base {
			t.Errorf("wrote %q, want basename %q", written[i], base)
		}
		if _, err := os.Stat(written[i]); err != nil {
			t.Errorf("reported-written file missing: %v", err)
		}
	}
	if len(result.Skipped) != 0 {
		t.Errorf("Skipped = %v on fresh dir, want empty", result.Skipped)
	}
}

func TestBootstrapDefaultsLeavesCustomisedFilesAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Operator has already customized channels.yaml; matrix.yaml is absent.
	customBody := []byte("# customized — do not touch\nchannels: []\n")
	customPath := filepath.Join(dir, "channels.yaml")
	if err := os.WriteFile(customPath, customBody, 0o600); err != nil {
		t.Fatalf("write custom: %v", err)
	}

	result, err := config.BootstrapDefaults(dir)
	if err != nil {
		t.Fatalf("BootstrapDefaults: %v", err)
	}

	// channels.yaml must survive untouched.
	got, err := os.ReadFile(customPath)
	if err != nil {
		t.Fatalf("read custom after bootstrap: %v", err)
	}
	if !bytes.Equal(got, customBody) {
		t.Error("bootstrap overwrote a customized channels.yaml")
	}
	if !contains(result.Skipped, customPath) {
		t.Errorf("Skipped = %v, want to contain %q", result.Skipped, customPath)
	}

	// matrix.yaml was missing — must be written.
	matrixPath := filepath.Join(dir, "matrix.yaml")
	if _, err := os.Stat(matrixPath); err != nil {
		t.Errorf("matrix.yaml not bootstrapped: %v", err)
	}
	if !contains(result.Written, matrixPath) {
		t.Errorf("Written = %v, want to contain %q", result.Written, matrixPath)
	}
}

func TestBootstrapDefaultsIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := config.BootstrapDefaults(dir); err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := config.BootstrapDefaults(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(second.Written) != 0 {
		t.Errorf("second pass wrote %v, want none", second.Written)
	}
	if len(second.Skipped) == 0 {
		t.Error("second pass skipped nothing; expected to skip existing files")
	}
}

func TestBootstrapDefaultsRejectsEmptyDir(t *testing.T) {
	t.Parallel()
	if _, err := config.BootstrapDefaults(""); err == nil {
		t.Fatal("BootstrapDefaults(\"\") succeeded; expected error")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
