package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// cellNameRE mirrors channelNameRE but with a slightly higher length
// cap. Cell names appear in publish manifests and attach URLs, so the
// DNS-label alphabet keeps them URL-safe without percent-encoding.
var cellNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,125}[a-z0-9])?$`)

// rMinorRE matches an R minor-version string like "4.4" or "4.3". We
// deliberately accept only "MAJOR.MINOR" (no patch) because R's package
// binaries are pinned per minor version, not per patch.
var rMinorRE = regexp.MustCompile(`^\d+\.\d+$`)

// distroRE matches a Posit Package Manager distribution codename
// ("jammy", "noble", "rhel9"). It appears verbatim in the
// /__linux__/{distro}/ URL segment.
var distroRE = regexp.MustCompile(`^[a-z0-9]+$`)

// validArchs is a small closed set — adding an arch should be a
// conscious code change, not a YAML surprise.
var validArchs = map[string]struct{}{"amd64": {}, "arm64": {}}

// Cell is one entry in matrix.yaml: an R minor version for which
// publishers may upload binaries. Every cell in a deployment shares
// the matrix-level distro and arch.
type Cell struct {
	Name   string `yaml:"name"`
	RMinor string `yaml:"r_minor"`
}

// MatrixConfig is the parsed contents of matrix.yaml.
//
// A deployment serves exactly one Linux distribution. Cells vary
// only by R minor version; in practice they mirror the R versions
// installed in the deployment's Workbench/Connect images.
type MatrixConfig struct {
	// Distro is the PPM codename served under /__linux__/{distro}/.
	Distro string `yaml:"distro"`
	// Arch is the CPU architecture binaries are built for.
	Arch string `yaml:"arch"`
	// DefaultRMinor picks the cell for clients whose User-Agent
	// carries no R version. Must be one of the cells' r_minor.
	DefaultRMinor string `yaml:"default_r_minor"`
	// BuildImageHint is advisory, for CI only; the server ignores it.
	BuildImageHint string `yaml:"build_image_hint"`
	Cells          []Cell `yaml:"cells"`
}

// Lookup returns the cell with the given name, or nil if absent. Used
// by the publish handler to reject manifest entries referencing cells
// the operator hasn't declared.
func (m *MatrixConfig) Lookup(name string) *Cell {
	for i := range m.Cells {
		if m.Cells[i].Name == name {
			return &m.Cells[i]
		}
	}
	return nil
}

// CellForRMinor returns the cell serving R minor version rMinor
// ("4.4"), or nil when no binaries are built for it.
func (m *MatrixConfig) CellForRMinor(rMinor string) *Cell {
	for i := range m.Cells {
		if m.Cells[i].RMinor == rMinor {
			return &m.Cells[i]
		}
	}
	return nil
}

// LoadMatrix reads and validates matrix.yaml at path.
func LoadMatrix(path string) (*MatrixConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open matrix config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return DecodeMatrix(f)
}

// DecodeMatrix parses and validates matrix YAML from r.
func DecodeMatrix(r io.Reader) (*MatrixConfig, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg MatrixConfig
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("matrix config is empty")
		}
		return nil, fmt.Errorf("decode matrix config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (m *MatrixConfig) validate() error {
	if m.Distro == "" {
		return errors.New("matrix config: distro is required (e.g. jammy, rhel9)")
	}
	if !distroRE.MatchString(m.Distro) {
		return fmt.Errorf("matrix config: distro must match %s, got %q", distroRE, m.Distro)
	}
	if _, ok := validArchs[m.Arch]; !ok {
		return fmt.Errorf("matrix config: arch must be one of amd64/arm64, got %q", m.Arch)
	}
	if len(m.Cells) == 0 {
		return errors.New("matrix config must define at least one cell")
	}

	names := map[string]struct{}{}
	minors := map[string]struct{}{}
	for i, c := range m.Cells {
		where := fmt.Sprintf("cells[%d] (name=%q)", i, c.Name)

		if c.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if !cellNameRE.MatchString(c.Name) {
			return fmt.Errorf("%s: name must match %s", where, cellNameRE)
		}
		if _, dup := names[c.Name]; dup {
			return fmt.Errorf("%s: duplicate cell name", where)
		}
		names[c.Name] = struct{}{}

		if !rMinorRE.MatchString(c.RMinor) {
			return fmt.Errorf("%s: r_minor must be MAJOR.MINOR (e.g. 4.4), got %q", where, c.RMinor)
		}
		if _, dup := minors[c.RMinor]; dup {
			return fmt.Errorf("%s: duplicate r_minor %q; one cell per R minor version", where, c.RMinor)
		}
		minors[c.RMinor] = struct{}{}
	}

	if m.DefaultRMinor == "" {
		return errors.New("matrix config: default_r_minor is required")
	}
	if _, ok := minors[m.DefaultRMinor]; !ok {
		return fmt.Errorf("matrix config: default_r_minor %q is not the r_minor of any cell", m.DefaultRMinor)
	}
	return nil
}
