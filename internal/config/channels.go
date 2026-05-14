// Package config loads and validates packyard's YAML configuration:
// channels.yaml (which channels exist and their overwrite policy),
// matrix.yaml (which OS/arch/R-minor cells binaries are published for),
// and the top-level server config.
//
// All loaders run strict YAML parsing — unknown keys are rejected so
// typos like "overwite_policy" fail loudly at startup rather than
// silently being ignored. That's the only reasonable default for a
// config that drives a production service.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// channelNameRE is the allowed shape of a channel name. Channels appear in
// URL paths (e.g. /dev/src/contrib/PACKAGES) so we constrain them to
// lowercase alphanumerics with interior hyphens — the DNS-label subset —
// capped at 63 chars to match DNS and avoid path-length surprises.
var channelNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// OverwritePolicy values. These match the CHECK constraint on
// channels.overwrite_policy in 001_init.sql.
const (
	PolicyMutable   = "mutable"
	PolicyImmutable = "immutable"
)

// Channel kind values. These match the CHECK constraint on
// channels.kind in 002_channel_kinds.sql. An unset kind in YAML
// normalises to KindLocal during validate().
const (
	KindLocal = "local"
	KindProxy = "proxy"
)

// Default values for unset UpstreamConfig fields. Picked to be safe
// for the most common upstreams (cloud.r-project.org, PPM, r-universe)
// — operators with stranger upstreams override per-channel.
const (
	defaultUpstreamIndexTTL       = 5 * time.Minute
	defaultUpstreamTimeout        = 5 * time.Minute
	defaultUpstreamTarballMaxSize = 500 << 20 // 500 MiB
)

// Channel is one entry in channels.yaml.
type Channel struct {
	Name            string          `yaml:"name"`
	OverwritePolicy string          `yaml:"overwrite_policy"`
	Default         bool            `yaml:"default"`
	Kind            string          `yaml:"kind"`
	Upstream        *UpstreamConfig `yaml:"upstream"`
}

// UpstreamConfig holds the fields a proxy channel needs to fetch
// from an upstream CRAN-protocol server.
//
// SourceURL points at the base of an upstream's CRAN tree — anything
// that serves /src/contrib/PACKAGES and /src/contrib/<name>_<ver>.tar.gz.
// Examples: https://cloud.r-project.org, https://packagemanager.posit.co/cran/latest,
// https://packagemanager.posit.co/cran/2026-01-15 (a pinned snapshot).
//
// BinaryURLs maps a cell name (from matrix.yaml) to an upstream base
// that serves precompiled binaries for that cell. PPM serves binaries
// via its /__linux__/<codename>/<snapshot>/ URL prefix; r-universe
// uses /bin/linux/<codename>/<rver>/. Cells absent from this map fall
// back to source — clients on those cells compile locally, which is
// CRAN's Linux default anyway.
type UpstreamConfig struct {
	SourceURL      string            `yaml:"source_url"`
	BinaryURLs     map[string]string `yaml:"binary_urls"`
	IndexTTL       time.Duration     `yaml:"index_ttl"`
	Timeout        time.Duration     `yaml:"timeout"`
	TarballMaxSize int64             `yaml:"tarball_max_size"`
}

// Resolved returns u with zero-valued fields filled from packyard's
// defaults. Callers (the reconciler, the upstream fetcher) read off
// the resolved view rather than checking each field for zero.
func (u *UpstreamConfig) Resolved() UpstreamConfig {
	if u == nil {
		return UpstreamConfig{}
	}
	out := *u
	if out.IndexTTL == 0 {
		out.IndexTTL = defaultUpstreamIndexTTL
	}
	if out.Timeout == 0 {
		out.Timeout = defaultUpstreamTimeout
	}
	if out.TarballMaxSize == 0 {
		out.TarballMaxSize = defaultUpstreamTarballMaxSize
	}
	return out
}

// ChannelsConfig is the parsed contents of channels.yaml.
type ChannelsConfig struct {
	Channels []Channel `yaml:"channels"`
}

// Default returns the channel marked default:true. Validation guarantees
// exactly one exists, so Default never returns nil on a validated config.
func (c *ChannelsConfig) Default() *Channel {
	for i := range c.Channels {
		if c.Channels[i].Default {
			return &c.Channels[i]
		}
	}
	return nil
}

// Lookup returns the channel with the given name, or nil if absent.
func (c *ChannelsConfig) Lookup(name string) *Channel {
	for i := range c.Channels {
		if c.Channels[i].Name == name {
			return &c.Channels[i]
		}
	}
	return nil
}

// LoadChannels reads and validates channels.yaml at path.
func LoadChannels(path string) (*ChannelsConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open channels config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return DecodeChannels(f)
}

// DecodeChannels parses and validates channels YAML from r. It's split out
// from LoadChannels so callers can decode from embed.FS or tests without
// first writing to disk.
func DecodeChannels(r io.Reader) (*ChannelsConfig, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg ChannelsConfig
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("channels config is empty")
		}
		return nil, fmt.Errorf("decode channels config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *ChannelsConfig) validate() error {
	if len(c.Channels) == 0 {
		return errors.New("channels config must define at least one channel")
	}

	seen := map[string]struct{}{}
	defaults := 0
	for i := range c.Channels {
		ch := &c.Channels[i]
		where := fmt.Sprintf("channels[%d] (name=%q)", i, ch.Name)

		if ch.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if !channelNameRE.MatchString(ch.Name) {
			return fmt.Errorf("%s: name must match %s", where, channelNameRE)
		}
		if _, dup := seen[ch.Name]; dup {
			return fmt.Errorf("%s: duplicate channel name", where)
		}
		seen[ch.Name] = struct{}{}

		switch ch.OverwritePolicy {
		case PolicyMutable, PolicyImmutable:
			// ok
		case "":
			return fmt.Errorf("%s: overwrite_policy is required (mutable or immutable)", where)
		default:
			return fmt.Errorf("%s: overwrite_policy must be mutable or immutable, got %q", where, ch.OverwritePolicy)
		}

		if err := validateChannelKind(ch, where); err != nil {
			return err
		}

		if ch.Default {
			defaults++
		}
	}

	switch defaults {
	case 1:
		// ok
	case 0:
		return errors.New("channels config must mark exactly one channel as default: true")
	default:
		return fmt.Errorf("channels config marks %d channels as default; exactly one is required", defaults)
	}

	return nil
}

// validateChannelKind normalises ch.Kind and enforces the kind/upstream
// invariants. Called from validate() with a pointer so an unset Kind
// can be normalised to "local" on the in-memory config.
func validateChannelKind(ch *Channel, where string) error {
	switch ch.Kind {
	case "":
		ch.Kind = KindLocal
	case KindLocal, KindProxy:
		// ok
	default:
		return fmt.Errorf("%s: kind must be %q or %q, got %q", where, KindLocal, KindProxy, ch.Kind)
	}

	switch ch.Kind {
	case KindLocal:
		if ch.Upstream != nil {
			return fmt.Errorf("%s: upstream is only valid on proxy channels (kind: proxy)", where)
		}
	case KindProxy:
		// default+proxy+anonymous-reads = an open cache-fill relay for
		// the public internet. Forbid the combination at config-load
		// time so it's impossible to deploy by accident, regardless of
		// the AllowAnonymousReads flag in server config.
		if ch.Default {
			return fmt.Errorf("%s: default channel cannot be a proxy (open-relay risk if anon reads are enabled)", where)
		}
		if ch.Upstream == nil {
			return fmt.Errorf("%s: kind: proxy requires upstream.source_url", where)
		}
		if err := validateUpstream(ch.Upstream, where); err != nil {
			return err
		}
	}
	return nil
}

func validateUpstream(u *UpstreamConfig, where string) error {
	if u.SourceURL == "" {
		return fmt.Errorf("%s: upstream.source_url is required", where)
	}
	if err := validateHTTPSURL(u.SourceURL); err != nil {
		return fmt.Errorf("%s: upstream.source_url: %w", where, err)
	}
	for cell, raw := range u.BinaryURLs {
		if cell == "" {
			return fmt.Errorf("%s: upstream.binary_urls has an empty cell key", where)
		}
		if err := validateHTTPSURL(raw); err != nil {
			return fmt.Errorf("%s: upstream.binary_urls[%q]: %w", where, cell, err)
		}
	}
	if u.IndexTTL < 0 {
		return fmt.Errorf("%s: upstream.index_ttl must not be negative", where)
	}
	if u.Timeout < 0 {
		return fmt.Errorf("%s: upstream.timeout must not be negative", where)
	}
	if u.TarballMaxSize < 0 {
		return fmt.Errorf("%s: upstream.tarball_max_size must not be negative", where)
	}
	return nil
}

func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be https or http, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}
