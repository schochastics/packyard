package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/schochastics/pakman/internal/auth"
)

// ImportInput is the in-process equivalent of the multipart publish
// payload. Importers (drat, git, one-off ops) use this instead of
// round-tripping through HTTP so they don't need a token and the event
// log can attribute work to a named importer actor rather than a
// bearer-token label.
type ImportInput struct {
	Channel string
	Name    string
	Version string

	// Source is the R source tarball as a stream. The caller is
	// responsible for closing it; ImportSource drains it into CAS.
	Source io.Reader

	// Actor tags the event row. Conventional values are "import-drat",
	// "import-git", "admin-cli" etc. Empty -> NULL in the DB.
	Actor string

	// Note is an optional free-form string saved to the event row
	// alongside the publish. Importers typically set the upstream URL.
	Note string
}

// ImportSource streams a source tarball into CAS and persists a
// package row with no binaries. The channel's overwrite_policy is
// honored exactly as in the HTTP publish path: immutable + different
// bytes yields ErrImmutableConflict; immutable + identical yields a
// response with AlreadyExisted=true; mutable replaces the row.
//
// Binaries are not part of this surface — importers produce source
// tarballs only. Operators wanting cell-specific binaries should
// publish via CI, either into the same (channel, name, version) tuple
// on a mutable channel, or by bumping the version.
func ImportSource(ctx context.Context, deps Deps, in ImportInput) (*PublishResponse, error) {
	if !packageNameRE.MatchString(in.Name) {
		return nil, fmt.Errorf("invalid package name %q", in.Name)
	}
	if !versionRE.MatchString(in.Version) {
		return nil, fmt.Errorf("invalid version %q", in.Version)
	}

	policy, ok, err := lookupChannelPolicy(ctx, deps.DB.DB, in.Channel)
	if err != nil {
		return nil, fmt.Errorf("channel lookup: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("channel %q not found", in.Channel)
	}

	sum, size, err := deps.CAS.Write(in.Source)
	if err != nil {
		return nil, fmt.Errorf("write source to CAS: %w", err)
	}

	const sourceKey = "source"
	parts := map[string]partRef{sourceKey: {sha256: sum, size: size}}
	manifest := Manifest{Source: sourceKey, PublishedBy: in.Actor}

	resp, herr := persistPublish(ctx, deps.DB.DB, publishInput{
		channel:   in.Channel,
		name:      in.Name,
		version:   in.Version,
		policy:    policy,
		manifest:  manifest,
		parts:     parts,
		publisher: auth.Identity{Label: in.Actor},
	})
	if herr != nil {
		// Map the HTTP-shaped error to a plain Go error so CLI callers
		// aren't tied to api's envelope. Conflict on immutable gets a
		// sentinel so importers can choose to skip vs abort.
		if herr.status == http.StatusConflict {
			return nil, fmt.Errorf("%w: %s", ErrImmutableConflict, herr.msg)
		}
		return nil, errors.New(herr.msg)
	}

	if deps.Index != nil && !resp.AlreadyExisted {
		deps.Index.InvalidateChannel(in.Channel)
	}
	recordPublishMetric(deps, in.Channel, resp)
	refreshCASBytes(ctx, deps)

	// Post-publish annotation so operators can tell in /ui/events which
	// publishes came from an importer vs a real CI push. Best-effort —
	// the package is already in the DB at this point.
	if in.Note != "" {
		if _, err := deps.DB.ExecContext(ctx, `
			INSERT INTO events(type, actor, channel, package, version, note)
			VALUES ('import', ?, ?, ?, ?, ?)
		`, nullIfEmpty(in.Actor), in.Channel, in.Name, in.Version, in.Note); err != nil {
			// Intentionally not fatal; the publish itself succeeded.
			return resp, nil
		}
	}

	return resp, nil
}

// ErrImmutableConflict is returned by ImportSource when publishing
// would overwrite an immutable channel with different bytes. Importers
// surface this to the operator so they can decide whether to skip,
// bump, or abort.
var ErrImmutableConflict = errors.New("immutable channel already has this version with different content")
