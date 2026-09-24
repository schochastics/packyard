package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

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

// writableChannel checks that channel accepts writes (publish, attach,
// yank, delete, import) and returns its overwrite policy. Kind and
// policy come from the channels table, so the check also holds for the
// CLI importers, which run without the YAML config. When the YAML
// config is loaded, a channel that is still in the DB but no longer in
// channels.yaml is refused too: reconcile never deletes rows, so
// "removed from the config" would otherwise still accept publishes.
func writableChannel(ctx context.Context, deps Deps, channel, verb string) (string, *httpError) {
	var policy, kind string
	err := deps.DB.QueryRowContext(ctx,
		`SELECT overwrite_policy, kind FROM channels WHERE name = ?`, channel,
	).Scan(&policy, &kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", &httpError{status: http.StatusNotFound, code: CodeNotFound,
			msg:  fmt.Sprintf("channel %q not found", channel),
			hint: "add the channel to channels.yaml and restart the server"}
	case err != nil:
		return "", internalErr("channel lookup", err)
	case kind == config.KindProxy:
		return "", &httpError{status: http.StatusConflict, code: CodeChannelIsProxy,
			msg:  fmt.Sprintf("channel %q is a proxy; %s is not accepted", channel, verb),
			hint: "Proxy channels mirror upstream. Pick a local channel."}
	}
	if deps.Channels != nil && deps.Channels.Lookup(channel) == nil {
		return "", &httpError{status: http.StatusNotFound, code: CodeNotFound,
			msg:  fmt.Sprintf("channel %q is no longer in channels.yaml; %s is not accepted", channel, verb),
			hint: "add the channel back to channels.yaml and restart, or use another channel"}
	}
	return policy, nil
}

// CheckWritableChannel is [writableChannel] for callers outside the
// HTTP layer (the bundle importer's up-front check).
func CheckWritableChannel(ctx context.Context, deps Deps, channel, verb string) error {
	if _, herr := writableChannel(ctx, deps, channel, verb); herr != nil {
		return herr
	}
	return nil
}
