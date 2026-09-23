package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.cynkra.com/david.schoch/packyard/internal/config"
)

func decodeServer(t *testing.T, src string) (*config.ServerConfig, error) {
	t.Helper()
	return config.DecodeServer(strings.NewReader(src))
}

func TestDecodeServerAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := decodeServer(t, ``)
	if err != nil {
		t.Fatalf("DecodeServer(empty): %v", err)
	}
	def := config.DefaultServerConfig()
	if cfg.Listen != def.Listen {
		t.Errorf("Listen = %q, want %q", cfg.Listen, def.Listen)
	}
	if cfg.DataDir != def.DataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, def.DataDir)
	}
	if cfg.TLSEnabled() {
		t.Error("TLSEnabled() = true on empty config")
	}
}

func TestDecodeServerMergesDefaults(t *testing.T) {
	t.Parallel()

	// Only listen is overridden; data_dir must still default.
	cfg, err := decodeServer(t, `listen: :9000`)
	if err != nil {
		t.Fatalf("DecodeServer: %v", err)
	}
	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", cfg.Listen)
	}
	if cfg.DataDir != config.DefaultServerConfig().DataDir {
		t.Errorf("DataDir not defaulted: %q", cfg.DataDir)
	}
}

func TestDecodeServerRejectsUnknownField(t *testing.T) {
	t.Parallel()
	if _, err := decodeServer(t, "listen: :8080\nbogus: true"); err == nil {
		t.Fatal("expected error for unknown field 'bogus'")
	}
}

func TestDecodeServerValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{
			name:    "tls cert without key",
			yaml:    "tls_cert: /etc/packyard/cert.pem",
			wantMsg: "tls_key is empty",
		},
		{
			name:    "tls key without cert",
			yaml:    "tls_key: /etc/packyard/key.pem",
			wantMsg: "tls_cert is empty",
		},
		{"public_url relative", "public_url: packages.example.org", "absolute http(s) URL"},
		{"public_url with path", "public_url: https://example.org/packyard", "must not have a path"},
		{"trusted proxy garbage", "trusted_proxies: [10.0.0.0/8, nope]", `"nope" is not an IP`},
		{"metrics_listen no port", "metrics_listen: localhost", "metrics_listen"},
		{"metrics_listen equals listen", "listen: :8080\nmetrics_listen: :8080", "must differ"},
		{"token without label", "tokens:\n  - scopes: admin\n    token_file: /x", "label is required"},
		{"token without scopes", "tokens:\n  - label: a\n    token_file: /x", "scopes is required"},
		{"token without secret", "tokens:\n  - label: a\n    scopes: admin", "exactly one of"},
		{"token with both secrets", "tokens:\n  - label: a\n    scopes: admin\n    token_file: /x\n    sha256_file: /y", "exactly one of"},
		{"duplicate token labels", "tokens:\n  - {label: a, scopes: admin, token_file: /x}\n  - {label: a, scopes: admin, token_file: /y}", "duplicate label"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeServer(t, tc.yaml)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestDecodeServerProxyAndMetricsKeys(t *testing.T) {
	t.Parallel()

	cfg, err := decodeServer(t, `
public_url: https://packages.example.org/
trusted_proxies: [10.0.0.0/8, 192.168.1.7, "::1"]
metrics_listen: 127.0.0.1:9090
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://packages.example.org" {
		t.Errorf("PublicURL = %q, want trailing slash trimmed", cfg.PublicURL)
	}
	if !cfg.SecureCookies() {
		t.Error("SecureCookies() = false with an https public_url")
	}
	got := cfg.TrustedProxyPrefixes()
	if len(got) != 3 || got[0].String() != "10.0.0.0/8" || got[1].String() != "192.168.1.7/32" || got[2].String() != "::1/128" {
		t.Errorf("TrustedProxyPrefixes = %v", got)
	}

	plain, err := decodeServer(t, "public_url: http://packages.internal")
	if err != nil {
		t.Fatal(err)
	}
	if plain.SecureCookies() {
		t.Error("SecureCookies() = true with an http public_url and no TLS")
	}
}

func TestServerPathHelpers(t *testing.T) {
	t.Parallel()

	cfg := &config.ServerConfig{DataDir: "/var/lib/packyard"}
	if got := cfg.ChannelsPath(); got != "/var/lib/packyard/channels.yaml" {
		t.Errorf("ChannelsPath default = %q", got)
	}
	if got := cfg.MatrixPath(); got != "/var/lib/packyard/matrix.yaml" {
		t.Errorf("MatrixPath default = %q", got)
	}

	cfg.ChannelsFile = "/etc/packyard/channels.yaml"
	if got := cfg.ChannelsPath(); got != "/etc/packyard/channels.yaml" {
		t.Errorf("ChannelsPath override = %q", got)
	}
}

func TestLoadServerResolvesRelativePaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.yaml")
	body := `
listen: :9000
data_dir: subdir
channels_file: ../channels.yaml
tls_cert: /abs/cert.pem
tls_key: /abs/key.pem
tokens:
  - label: ci
    scopes: publish:prod
    token_file: secrets/ci
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	wantDataDir := filepath.Join(dir, "subdir")
	if cfg.DataDir != wantDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantDataDir)
	}
	wantChannels := filepath.Join(dir, "../channels.yaml")
	if cfg.ChannelsFile != wantChannels {
		t.Errorf("ChannelsFile = %q, want %q", cfg.ChannelsFile, wantChannels)
	}
	if cfg.TLSCert != "/abs/cert.pem" {
		t.Errorf("absolute TLSCert mangled: %q", cfg.TLSCert)
	}
	if !cfg.TLSEnabled() {
		t.Error("TLSEnabled() = false with both cert and key set")
	}
	if want := filepath.Join(dir, "secrets/ci"); cfg.Tokens[0].TokenFile != want {
		t.Errorf("Tokens[0].TokenFile = %q, want %q", cfg.Tokens[0].TokenFile, want)
	}
}

func TestLoadServerMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := config.LoadServer(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing server config")
	}
}
