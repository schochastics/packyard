package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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

	// PublicURL is the external base URL clients reach the server at,
	// e.g. https://packages.example.org. Set it when TLS terminates at
	// a reverse proxy: an https PublicURL marks UI cookies Secure, and
	// the UI uses it for copy-paste repository URLs. Only a scheme and
	// host are allowed — packyard must own the whole hostname.
	PublicURL string `yaml:"public_url"`

	// TrustedProxies lists the peers (CIDRs or bare IPs) whose
	// X-Forwarded-For / X-Real-IP headers are believed. Requests from
	// anywhere else are logged with their socket address.
	TrustedProxies []string `yaml:"trusted_proxies"`

	// MetricsListen, when set, serves /metrics on its own listener
	// (e.g. "127.0.0.1:9090") and removes it from the main one.
	MetricsListen string `yaml:"metrics_listen"`

	// Tokens are API tokens provisioned from config (typically from
	// secret files) rather than minted at runtime. See SyncConfigTokens.
	Tokens []TokenConfig `yaml:"tokens"`
}

// TokenConfig is one entry under server.yaml's tokens:. The secret
// comes from exactly one of TokenFile (the plaintext token) or
// SHA256File (the hex sha256 of it, so the plaintext never touches
// the server host).
type TokenConfig struct {
	Label      string `yaml:"label"`
	Scopes     string `yaml:"scopes"`
	TokenFile  string `yaml:"token_file"`
	SHA256File string `yaml:"sha256_file"`
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

// SecureCookies reports whether clients reach the server over HTTPS,
// either directly or through a TLS-terminating proxy (PublicURL).
func (c *ServerConfig) SecureCookies() bool {
	return c.TLSEnabled() || strings.HasPrefix(c.PublicURL, "https://")
}

// TrustedProxyPrefixes returns TrustedProxies parsed; bare IPs become
// single-address prefixes. validate has already rejected bad entries.
func (c *ServerConfig) TrustedProxyPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(c.TrustedProxies))
	for _, s := range c.TrustedProxies {
		if p, err := parsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
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

	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("public_url %q must be an absolute http(s) URL", c.PublicURL)
		}
		if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("public_url %q must not have a path: packyard needs its own hostname", c.PublicURL)
		}
		c.PublicURL = u.Scheme + "://" + u.Host
	}
	for _, s := range c.TrustedProxies {
		if _, err := parsePrefix(s); err != nil {
			return fmt.Errorf("trusted_proxies: %q is not an IP address or CIDR", s)
		}
	}
	if c.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(c.MetricsListen); err != nil {
			return fmt.Errorf("metrics_listen %q: %w", c.MetricsListen, err)
		}
		if c.MetricsListen == c.Listen {
			return errors.New("metrics_listen must differ from listen")
		}
	}

	labels := map[string]bool{}
	for i, t := range c.Tokens {
		switch {
		case t.Label == "":
			return fmt.Errorf("tokens[%d]: label is required", i)
		case labels[t.Label]:
			return fmt.Errorf("tokens: duplicate label %q", t.Label)
		case strings.TrimSpace(t.Scopes) == "":
			return fmt.Errorf("tokens[%s]: scopes is required", t.Label)
		case (t.TokenFile == "") == (t.SHA256File == ""):
			return fmt.Errorf("tokens[%s]: set exactly one of token_file or sha256_file", t.Label)
		}
		labels[t.Label] = true
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
	for i := range c.Tokens {
		c.Tokens[i].TokenFile = resolve(c.Tokens[i].TokenFile)
		c.Tokens[i].SHA256File = resolve(c.Tokens[i].SHA256File)
	}
}
