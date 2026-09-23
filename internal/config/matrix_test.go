package config_test

import (
	"bytes"
	"strings"
	"testing"

	"gitea.cynkra.com/david.schoch/packyard/internal/config"
)

func decodeMatrix(t *testing.T, src string) (*config.MatrixConfig, error) {
	t.Helper()
	return config.DecodeMatrix(strings.NewReader(src))
}

const validMatrix = `
distro: jammy
arch: amd64
default_r_minor: "4.4"
build_image_hint: example/image:tag
cells:
  - { name: r-4.3, r_minor: "4.3" }
  - { name: r-4.4, r_minor: "4.4" }
  - { name: r-4.5, r_minor: "4.5" }
`

func TestDecodeMatrixHappyPath(t *testing.T) {
	t.Parallel()

	cfg, err := decodeMatrix(t, validMatrix)
	if err != nil {
		t.Fatalf("DecodeMatrix: %v", err)
	}
	if cfg.Distro != "jammy" || cfg.Arch != "amd64" || cfg.DefaultRMinor != "4.4" {
		t.Errorf("top-level fields = %+v", cfg)
	}
	if n := len(cfg.Cells); n != 3 {
		t.Fatalf("got %d cells, want 3", n)
	}
	if got := cfg.Lookup("r-4.5"); got == nil || got.RMinor != "4.5" {
		t.Errorf("Lookup(r-4.5) = %v", got)
	}
	if cfg.Lookup("nope") != nil {
		t.Error("Lookup for absent cell returned non-nil")
	}
	if got := cfg.CellForRMinor("4.3"); got == nil || got.Name != "r-4.3" {
		t.Errorf("CellForRMinor(4.3) = %v", got)
	}
	if cfg.CellForRMinor("4.6") != nil {
		t.Error("CellForRMinor for absent minor returned non-nil")
	}
}

func TestDecodeMatrixRejectsUnknownField(t *testing.T) {
	t.Parallel()

	// The pre-v1.3 per-cell os/os_version/arch fields must fail loudly
	// rather than be silently ignored.
	src := `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells:
  - name: r-4.4
    os: linux
    r_minor: "4.4"
`
	if _, err := decodeMatrix(t, src); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestDecodeMatrixValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{
			name:    "empty",
			src:     ``,
			wantMsg: "empty",
		},
		{
			name: "missing distro",
			src: `
arch: amd64
default_r_minor: "4.4"
cells: [{ name: r-4.4, r_minor: "4.4" }]
`,
			wantMsg: "distro is required",
		},
		{
			name: "bad distro",
			src: `
distro: Ubuntu 22.04
arch: amd64
default_r_minor: "4.4"
cells: [{ name: r-4.4, r_minor: "4.4" }]
`,
			wantMsg: "distro must match",
		},
		{
			name: "bad arch",
			src: `
distro: jammy
arch: sparc
default_r_minor: "4.4"
cells: [{ name: r-4.4, r_minor: "4.4" }]
`,
			wantMsg: "arch must be one of",
		},
		{
			name: "no cells",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells: []
`,
			wantMsg: "at least one cell",
		},
		{
			name: "bad cell name",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells: [{ name: R_4.4, r_minor: "4.4" }]
`,
			wantMsg: "name must match",
		},
		{
			name: "duplicate cell name",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells:
  - { name: r-4.4, r_minor: "4.4" }
  - { name: r-4.4, r_minor: "4.5" }
`,
			wantMsg: "duplicate cell name",
		},
		{
			name: "patch-level r_minor",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells: [{ name: r-4.4, r_minor: "4.4.1" }]
`,
			wantMsg: "r_minor must be MAJOR.MINOR",
		},
		{
			name: "duplicate r_minor",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.4"
cells:
  - { name: r-4.4, r_minor: "4.4" }
  - { name: r-4.4b, r_minor: "4.4" }
`,
			wantMsg: "duplicate r_minor",
		},
		{
			name: "missing default_r_minor",
			src: `
distro: jammy
arch: amd64
cells: [{ name: r-4.4, r_minor: "4.4" }]
`,
			wantMsg: "default_r_minor is required",
		},
		{
			name: "default_r_minor without cell",
			src: `
distro: jammy
arch: amd64
default_r_minor: "4.5"
cells: [{ name: r-4.4, r_minor: "4.4" }]
`,
			wantMsg: "not the r_minor of any cell",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeMatrix(t, tc.src)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want substring %q", err, tc.wantMsg)
			}
		})
	}
}

func TestEmbeddedDefaultMatrixIsValid(t *testing.T) {
	t.Parallel()

	body, err := config.EmbeddedDefault("matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DecodeMatrix(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("embedded default matrix.yaml is invalid: %v", err)
	}
	if cfg.Distro != "jammy" {
		t.Errorf("default distro = %q, want jammy", cfg.Distro)
	}
}
