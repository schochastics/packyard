package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ServerConfig is the top-level server config read from `-config <path>`.
// All fields are optional — zero values fall back to defaults. Most
// production installs will override only listen, data_dir, and the tls_*
// pair.
type ServerConfig struct {
	// Listen is the host:port the HTTP server binds to. Default ":8080".
	Listen string `yaml:"listen"`

	// DataDir is the root for the SQLite database (db.sqlite) and the
	// CAS blob store (cas/). Default "./data".
	DataDir string `yaml:"data_dir"`

	// ChannelsFile points at channels.yaml. When empty, packyard uses
	// <DataDir>/channels.yaml.
	ChannelsFile string `yaml:"channels_file"`

	// MatrixFile points at matrix.yaml. When empty, packyard uses
	// <DataDir>/matrix.yaml.
	MatrixFile string `yaml:"matrix_file"`

	// TLSCert / TLSKey enable HTTPS when both are set. The server falls
	// back to plain HTTP when either is empty.
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`

	// AllowAnonymousReads lets unauthenticated clients hit the default
	// channel's CRAN-protocol read endpoints. Everything else still
	// requires a valid token. Default false.
	AllowAnonymousReads bool `yaml:"allow_anonymous_reads"`
}

// DefaultServerConfig returns a ServerConfig with defaults applied for
// every field. It never fails.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Listen:  ":8080",
		DataDir: "./data",
	}
}

// LoadServer reads server config from path and applies defaults and
// validation. Paths inside the config are resolved relative to the
// config file's directory — that's what most operators expect from a
// "server.yaml" when they deploy via a config-management tool.
func LoadServer(path string) (*ServerConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open server config: %w", err)
	}
	defer func() { _ = f.Close() }()

	cfg, err := DecodeServer(f)
	if err != nil {
		return nil, err
	}

	base := filepath.Dir(path)
	cfg.resolveRelative(base)
	return cfg, nil
}

// DecodeServer parses server YAML from r and applies defaults; unlike
// LoadServer it does NOT resolve relative paths (callers using an
// embed.FS or raw bytes have no meaningful base directory to resolve
// against). Strict YAML parsing: unknown keys fail.
func DecodeServer(r io.Reader) (*ServerConfig, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	cfg := DefaultServerConfig()
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is fine — keep the defaults.
			return &cfg, cfg.validate()
		}
		return nil, fmt.Errorf("decode server config: %w", err)
	}
	// Re-apply defaults for any fields the YAML left empty.
	if cfg.Listen == "" {
		cfg.Listen = DefaultServerConfig().Listen
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultServerConfig().DataDir
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ChannelsPath returns the effective path to channels.yaml, defaulting
// to <DataDir>/channels.yaml when ChannelsFile is empty.
func (c *ServerConfig) ChannelsPath() string {
	if c.ChannelsFile != "" {
		return c.ChannelsFile
	}
	return filepath.Join(c.DataDir, "channels.yaml")
}

// MatrixPath returns the effective path to matrix.yaml.
func (c *ServerConfig) MatrixPath() string {
	if c.MatrixFile != "" {
		return c.MatrixFile
	}
	return filepath.Join(c.DataDir, "matrix.yaml")
}

// TLSEnabled reports whether HTTPS should be served.
func (c *ServerConfig) TLSEnabled() bool {
	return c.TLSCert != "" && c.TLSKey != ""
}

func (c *ServerConfig) validate() error {
	// Listen must look at least plausible — a bare port or host:port. We
	// don't try to dial it; net/http will surface any real binding error.
	if c.Listen == "" {
		return errors.New("listen must not be empty")
	}
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}

	// TLS is all-or-nothing: half a pair is a misconfiguration we can
	// detect before the listener starts.
	switch {
	case c.TLSCert != "" && c.TLSKey == "":
		return errors.New("tls_cert is set but tls_key is empty")
	case c.TLSCert == "" && c.TLSKey != "":
		return errors.New("tls_key is set but tls_cert is empty")
	}

	return nil
}

// resolveRelative converts every path field that isn't already absolute
// into an absolute path relative to base. Called from LoadServer so
// paths in a config read from /etc/packyard/server.yaml don't silently
// resolve against the server's working directory.
func (c *ServerConfig) resolveRelative(base string) {
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.DataDir = resolve(c.DataDir)
	c.ChannelsFile = resolve(c.ChannelsFile)
	c.MatrixFile = resolve(c.MatrixFile)
	c.TLSCert = resolve(c.TLSCert)
	c.TLSKey = resolve(c.TLSKey)
}
