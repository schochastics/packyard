package api

import (
	"context"

	"github.com/schochastics/packyard/internal/config"
)

// channelMeta bundles the config-side facts about a channel that
// proxy-aware handlers need together: name, overwrite policy, kind
// (local | proxy), and — for proxy channels — the resolved upstream
// config with defaults applied.
//
// Handlers reach for it via [lookupChannelMeta] after they've already
// validated channel name shape. Existence is implicit: a nil result
// means the channel is unknown to the in-memory config and the caller
// should fall back to its existing 404 path.
type channelMeta struct {
	Name     string
	Policy   string
	Kind     string
	Upstream config.UpstreamConfig // zero value when Kind == KindLocal
}

// IsProxy reports whether the channel materializes from an upstream.
func (m *channelMeta) IsProxy() bool {
	return m != nil && m.Kind == config.KindProxy
}

// lookupChannelMeta returns the channel-config metadata for name, or
// nil if the channel is not known to the in-memory config. Reading
// from the YAML-loaded config rather than the channels table is
// intentional: every reachable channel is reconciled into the DB at
// startup, and the YAML is the source of truth for kind/upstream.
//
// Tests that build a Deps without a Channels config see a nil result,
// which surfaces as "channel not found" — matching the local-only
// behavior that pre-dates this helper.
func lookupChannelMeta(_ context.Context, deps Deps, name string) *channelMeta {
	if deps.Channels == nil {
		return nil
	}
	ch := deps.Channels.Lookup(name)
	if ch == nil {
		return nil
	}
	m := &channelMeta{
		Name:   ch.Name,
		Policy: ch.OverwritePolicy,
		Kind:   ch.Kind,
	}
	if ch.Upstream != nil {
		m.Upstream = ch.Upstream.Resolved()
	}
	return m
}
