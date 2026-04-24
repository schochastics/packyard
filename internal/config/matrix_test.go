package config_test

import (
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/config"
)

func decodeMatrix(t *testing.T, src string) (*config.MatrixConfig, error) {
	t.Helper()
	return config.DecodeMatrix(strings.NewReader(src))
}

func TestDecodeMatrixHappyPath(t *testing.T) {
	t.Parallel()

	src := `
cells:
  - name: ubuntu-22.04-amd64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
  - name: ubuntu-22.04-arm64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: arm64
    r_minor: "4.4"
  - name: darwin-arm64-r-4.4
    os: darwin
    os_version: "14"
    arch: arm64
    r_minor: "4.4"
`
	cfg, err := decodeMatrix(t, src)
	if err != nil {
		t.Fatalf("DecodeMatrix: %v", err)
	}
	if n := len(cfg.Cells); n != 3 {
		t.Fatalf("got %d cells, want 3", n)
	}
	if got := cfg.Lookup("darwin-arm64-r-4.4"); got == nil || got.OS != "darwin" {
		t.Errorf("Lookup(darwin-arm64-r-4.4) = %v", got)
	}
	if cfg.Lookup("nope") != nil {
		t.Error("Lookup for absent cell returned non-nil")
	}
}

func TestDecodeMatrixRejectsUnknownField(t *testing.T) {
	t.Parallel()

	src := `
cells:
  - name: ubuntu-22.04-amd64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
    patch: "1"
`
	if _, err := decodeMatrix(t, src); err == nil {
		t.Fatal("expected error for unknown field 'patch', got nil")
	}
}

func TestDecodeMatrixValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{
			name:    "empty list",
			yaml:    `cells: []`,
			wantMsg: "at least one cell",
		},
		{
			name: "missing name",
			yaml: `
cells:
  - os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
`,
			wantMsg: "name is required",
		},
		{
			name: "invalid os",
			yaml: `
cells:
  - name: freebsd-amd64-r-4.4
    os: freebsd
    os_version: "14"
    arch: amd64
    r_minor: "4.4"
`,
			wantMsg: "os must be one of",
		},
		{
			name: "invalid arch",
			yaml: `
cells:
  - name: linux-sparc-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: sparc64
    r_minor: "4.4"
`,
			wantMsg: "arch must be one of",
		},
		{
			name: "missing os_version",
			yaml: `
cells:
  - name: linux-amd64-r-4.4
    os: linux
    arch: amd64
    r_minor: "4.4"
`,
			wantMsg: "os_version is required",
		},
		{
			name: "bad r_minor (patch included)",
			yaml: `
cells:
  - name: linux-amd64-r-4-4-1
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4.1"
`,
			wantMsg: "r_minor must be MAJOR.MINOR",
		},
		{
			name: "bad r_minor (letters)",
			yaml: `
cells:
  - name: linux-amd64-r-devel
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: devel
`,
			wantMsg: "r_minor must be MAJOR.MINOR",
		},
		{
			name: "duplicate name",
			yaml: `
cells:
  - name: linux-amd64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
  - name: linux-amd64-r-4.4
    os: linux
    os_version: ubuntu-24.04
    arch: amd64
    r_minor: "4.4"
`,
			wantMsg: "duplicate cell name",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeMatrix(t, tc.yaml)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
