package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
