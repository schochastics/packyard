// Package config loads and validates pakman's YAML configuration:
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
	"os"
	"regexp"

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

// Channel is one entry in channels.yaml.
type Channel struct {
	Name            string `yaml:"name"`
	OverwritePolicy string `yaml:"overwrite_policy"`
	Default         bool   `yaml:"default"`
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
	for i, ch := range c.Channels {
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
