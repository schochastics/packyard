package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/config"
)

func decodeChannels(t *testing.T, src string) (*config.ChannelsConfig, error) {
	t.Helper()
	return config.DecodeChannels(strings.NewReader(src))
}

func TestDecodeChannelsHappyPath(t *testing.T) {
	t.Parallel()

	src := `
channels:
  - name: dev
    overwrite_policy: mutable
    default: false
  - name: test
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
`
	cfg, err := decodeChannels(t, src)
	if err != nil {
		t.Fatalf("DecodeChannels: %v", err)
	}
	if n := len(cfg.Channels); n != 3 {
		t.Fatalf("got %d channels, want 3", n)
	}
	if d := cfg.Default(); d == nil || d.Name != "prod" {
		t.Errorf("Default() = %v, want prod", d)
	}
	if ch := cfg.Lookup("test"); ch == nil || ch.OverwritePolicy != config.PolicyMutable {
		t.Errorf("Lookup(test) = %v, want mutable policy", ch)
	}
	if cfg.Lookup("nope") != nil {
		t.Error("Lookup for absent channel returned non-nil")
	}
}

func TestDecodeChannelsProxyHappyPath(t *testing.T) {
	t.Parallel()

	src := `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran-r44
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://packagemanager.posit.co/cran/2026-01-15
      binary_urls:
        ubuntu-22.04-amd64-r4.4: https://packagemanager.posit.co/cran/__linux__/jammy/2026-01-15
      index_ttl: 10m
      timeout: 2m
      tarball_max_size: 209715200
`
	cfg, err := decodeChannels(t, src)
	if err != nil {
		t.Fatalf("DecodeChannels: %v", err)
	}
	prod := cfg.Lookup("prod")
	if prod == nil || prod.Kind != config.KindLocal {
		t.Errorf("prod.Kind = %q, want %q (unset normalises to local)", prod.Kind, config.KindLocal)
	}
	ch := cfg.Lookup("cran-r44")
	if ch == nil {
		t.Fatal("cran-r44 channel not found")
	}
	if ch.Kind != config.KindProxy {
		t.Errorf("Kind = %q, want %q", ch.Kind, config.KindProxy)
	}
	if ch.Upstream == nil {
		t.Fatal("Upstream is nil")
	}
	if ch.Upstream.SourceURL != "https://packagemanager.posit.co/cran/2026-01-15" {
		t.Errorf("SourceURL = %q", ch.Upstream.SourceURL)
	}
	got := ch.Upstream.BinaryURLs["ubuntu-22.04-amd64-r4.4"]
	if got != "https://packagemanager.posit.co/cran/__linux__/jammy/2026-01-15" {
		t.Errorf("BinaryURLs[ubuntu-22.04-amd64-r4.4] = %q", got)
	}
	resolved := ch.Upstream.Resolved()
	if resolved.IndexTTL != 10*time.Minute {
		t.Errorf("Resolved().IndexTTL = %v", resolved.IndexTTL)
	}
	if resolved.TarballMaxSize != 200<<20 {
		t.Errorf("Resolved().TarballMaxSize = %d", resolved.TarballMaxSize)
	}
}

func TestUpstreamResolvedFillsDefaults(t *testing.T) {
	t.Parallel()

	u := config.UpstreamConfig{SourceURL: "https://example.org"}
	r := u.Resolved()
	if r.IndexTTL != 5*time.Minute {
		t.Errorf("IndexTTL default = %v, want 5m", r.IndexTTL)
	}
	if r.Timeout != 5*time.Minute {
		t.Errorf("Timeout default = %v, want 5m", r.Timeout)
	}
	if r.TarballMaxSize != 500<<20 {
		t.Errorf("TarballMaxSize default = %d, want %d", r.TarballMaxSize, 500<<20)
	}
	// Resolved must not mutate the source.
	if u.IndexTTL != 0 || u.Timeout != 0 || u.TarballMaxSize != 0 {
		t.Errorf("Resolved mutated receiver: %+v", u)
	}
}

func TestDecodeChannelsRejectsUnknownField(t *testing.T) {
	t.Parallel()

	// "overwite_policy" is a deliberate typo — strict mode must flag it.
	src := `
channels:
  - name: prod
    overwite_policy: immutable
    default: true
`
	if _, err := decodeChannels(t, src); err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestDecodeChannelsValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		yaml    string
		wantMsg string // substring of the error
	}{
		{
			name:    "empty list",
			yaml:    `channels: []`,
			wantMsg: "at least one channel",
		},
		{
			name: "missing name",
			yaml: `
channels:
  - overwrite_policy: mutable
    default: true
`,
			wantMsg: "name is required",
		},
		{
			name: "bad name shape (uppercase)",
			yaml: `
channels:
  - name: Prod
    overwrite_policy: immutable
    default: true
`,
			wantMsg: "name must match",
		},
		{
			name: "bad name shape (trailing hyphen)",
			yaml: `
channels:
  - name: prod-
    overwrite_policy: immutable
    default: true
`,
			wantMsg: "name must match",
		},
		{
			name: "duplicate name",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: prod
    overwrite_policy: mutable
`,
			wantMsg: "duplicate channel name",
		},
		{
			name: "missing overwrite_policy",
			yaml: `
channels:
  - name: prod
    default: true
`,
			wantMsg: "overwrite_policy is required",
		},
		{
			name: "invalid overwrite_policy",
			yaml: `
channels:
  - name: prod
    overwrite_policy: append-only
    default: true
`,
			wantMsg: "mutable or immutable",
		},
		{
			name: "no default channel",
			yaml: `
channels:
  - name: dev
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
`,
			wantMsg: "exactly one channel as default",
		},
		{
			name: "two defaults",
			yaml: `
channels:
  - name: dev
    overwrite_policy: mutable
    default: true
  - name: prod
    overwrite_policy: immutable
    default: true
`,
			wantMsg: "marks 2 channels as default",
		},
		{
			name: "invalid kind value",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: rogue
    overwrite_policy: immutable
    kind: weird
`,
			wantMsg: `kind must be "local" or "proxy"`,
		},
		{
			name: "local channel with upstream block",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: rogue
    overwrite_policy: immutable
    upstream:
      source_url: https://cloud.r-project.org
`,
			wantMsg: "upstream is only valid on proxy channels",
		},
		{
			name: "proxy channel without upstream",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
`,
			wantMsg: "kind: proxy requires upstream.source_url",
		},
		{
			name: "proxy channel without source_url",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      binary_urls:
        ubuntu-22.04-amd64-r4.4: https://packagemanager.posit.co/cran/__linux__/jammy/latest
`,
			wantMsg: "upstream.source_url is required",
		},
		{
			name: "proxy channel with non-https upstream scheme",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: ftp://cran.r-project.org
`,
			wantMsg: "scheme must be https or http",
		},
		{
			name: "default channel cannot be a proxy",
			yaml: `
channels:
  - name: cran
    overwrite_policy: immutable
    default: true
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
`,
			wantMsg: "default channel cannot be a proxy",
		},
		{
			name: "negative index_ttl",
			yaml: `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
  - name: cran
    overwrite_policy: immutable
    kind: proxy
    upstream:
      source_url: https://cloud.r-project.org
      index_ttl: -5m
`,
			wantMsg: "index_ttl must not be negative",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeChannels(t, tc.yaml)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestLoadChannelsFromFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "channels.yaml")
	body := `
channels:
  - name: prod
    overwrite_policy: immutable
    default: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.LoadChannels(path)
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Name != "prod" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadChannelsMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := config.LoadChannels(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
