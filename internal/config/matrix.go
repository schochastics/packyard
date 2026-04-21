package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// cellNameRE mirrors channelNameRE but with a slightly higher length cap
// because cell names often encode os+version+arch+r-minor (e.g.
// "ubuntu-22.04-amd64-r-4.4"). A cell name appears in URL paths like
// .../bin/linux/<cell>/PACKAGES, so the DNS-label alphabet keeps it
// URL-safe without percent-encoding.
var cellNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,125}[a-z0-9])?$`)

// rMinorRE matches an R minor-version string like "4.4" or "4.3". We
// deliberately accept only "MAJOR.MINOR" (no patch) because R's package
// binaries are pinned per minor version, not per patch.
var rMinorRE = regexp.MustCompile(`^\d+\.\d+$`)

// OS/arch enums. Deliberately a small closed set — adding a new OS or
// arch should be a conscious code change, not a YAML surprise.
var (
	validOSes  = map[string]struct{}{"linux": {}, "darwin": {}, "windows": {}}
	validArchs = map[string]struct{}{"amd64": {}, "arm64": {}, "i386": {}}
)

// Cell is one entry in matrix.yaml — a (os, os_version, arch, r_minor)
// combination for which publishers may upload binaries.
type Cell struct {
	Name      string `yaml:"name"`
	OS        string `yaml:"os"`
	OSVersion string `yaml:"os_version"`
	Arch      string `yaml:"arch"`
	RMinor    string `yaml:"r_minor"`
}

// MatrixConfig is the parsed contents of matrix.yaml.
type MatrixConfig struct {
	Cells []Cell `yaml:"cells"`
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
	if len(m.Cells) == 0 {
		return errors.New("matrix config must define at least one cell")
	}

	seen := map[string]struct{}{}
	for i, c := range m.Cells {
		where := fmt.Sprintf("cells[%d] (name=%q)", i, c.Name)

		if c.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if !cellNameRE.MatchString(c.Name) {
			return fmt.Errorf("%s: name must match %s", where, cellNameRE)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("%s: duplicate cell name", where)
		}
		seen[c.Name] = struct{}{}

		if _, ok := validOSes[c.OS]; !ok {
			return fmt.Errorf("%s: os must be one of linux/darwin/windows, got %q", where, c.OS)
		}
		if c.OSVersion == "" {
			return fmt.Errorf("%s: os_version is required", where)
		}
		if _, ok := validArchs[c.Arch]; !ok {
			return fmt.Errorf("%s: arch must be one of amd64/arm64/i386, got %q", where, c.Arch)
		}
		if !rMinorRE.MatchString(c.RMinor) {
			return fmt.Errorf("%s: r_minor must be MAJOR.MINOR (e.g. 4.4), got %q", where, c.RMinor)
		}
	}
	return nil
}
