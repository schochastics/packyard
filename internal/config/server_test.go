package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/config"
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
			yaml:    "tls_cert: /etc/pakman/cert.pem",
			wantMsg: "tls_key is empty",
		},
		{
			name:    "tls key without cert",
			yaml:    "tls_key: /etc/pakman/key.pem",
			wantMsg: "tls_cert is empty",
		},
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

func TestServerPathHelpers(t *testing.T) {
	t.Parallel()

	cfg := &config.ServerConfig{DataDir: "/var/lib/pakman"}
	if got := cfg.ChannelsPath(); got != "/var/lib/pakman/channels.yaml" {
		t.Errorf("ChannelsPath default = %q", got)
	}
	if got := cfg.MatrixPath(); got != "/var/lib/pakman/matrix.yaml" {
		t.Errorf("MatrixPath default = %q", got)
	}

	cfg.ChannelsFile = "/etc/pakman/channels.yaml"
	if got := cfg.ChannelsPath(); got != "/etc/pakman/channels.yaml" {
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
}

func TestLoadServerMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := config.LoadServer(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing server config")
	}
}
