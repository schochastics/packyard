package api

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/schochastics/packyard/internal/store"
)

// storeService returns deps.Store, lazy-initializing it from deps.DB
// and deps.CAS if the caller didn't wire one up. NewMux does the same
// during HTTP server start-up; the lazy fallback exists so test
// helpers that build Deps by hand keep working.
func storeService(deps Deps) *store.Service {
	if deps.Store != nil {
		return deps.Store
	}
	return store.New(deps.DB.DB, deps.CAS)
}

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

	svc := storeService(deps)
	blob, err := svc.WriteBlob(in.Source)
	if err != nil {
		return nil, fmt.Errorf("write source to CAS: %w", err)
	}

	res, err := svc.Materialize(ctx, store.Input{
		Channel: in.Channel,
		Name:    in.Name,
		Version: in.Version,
		Policy:  policy,
		Source:  blob,
		Actor:   in.Actor,
	})
	if err != nil {
		// Map the store-layer error to a sentinel CLI callers can
		// switch on. Other errors flow through unchanged.
		if errors.Is(err, store.ErrImmutableConflict) {
			return nil, err // already wraps ErrImmutableConflict
		}
		return nil, err
	}

	if deps.Index != nil && !res.AlreadyExisted {
		deps.Index.InvalidateChannel(in.Channel)
	}
	resp := publishResponseFromStore(res)
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

// ErrImmutableConflict is the api-package alias of
// [store.ErrImmutableConflict]. Re-exported so existing callers
// (bundle importer, tests) keep working without an import shuffle.
var ErrImmutableConflict = store.ErrImmutableConflict

// ErrSourceRowMissing is the api-package alias of
// [store.ErrSourceRowMissing]. See ErrImmutableConflict.
var ErrSourceRowMissing = store.ErrSourceRowMissing

// AttachInput specifies a single binary to attach to an existing
// (channel, name, version) row. The package row MUST already exist —
// see ErrSourceRowMissing.
type AttachInput struct {
	Channel string
	Name    string
	Version string

	// Cell must match a name declared in matrix.yaml on the running
	// server. The publish path validates this at request time; we
	// validate it here too so an importer can't sneak past.
	Cell string

	// Binary is the precompiled tarball as a stream. Caller closes;
	// AttachBinaries drains into CAS.
	Binary io.Reader

	// Actor and Note follow the same convention as ImportInput —
	// recorded on the import_binary event row.
	Actor string
	Note  string
}

// AttachBinaries attaches one binary tarball to an existing package row
// without touching the source row. This is how separate source/binary
// air-gap bundles compose: the operator imports the source bundle to
// create packages rows, then imports one or more cell-scoped binary
// bundles to populate the binaries table.
//
// See [store.Service.AttachBinary] for the per-policy semantics.
func AttachBinaries(ctx context.Context, deps Deps, in AttachInput) (*PublishResponse, error) {
	if !packageNameRE.MatchString(in.Name) {
		return nil, fmt.Errorf("invalid package name %q", in.Name)
	}
	if !versionRE.MatchString(in.Version) {
		return nil, fmt.Errorf("invalid version %q", in.Version)
	}
	if in.Cell == "" {
		return nil, errors.New("cell is required")
	}
	if deps.Matrix == nil {
		return nil, errors.New("server has no matrix config; cannot attach binaries")
	}
	if deps.Matrix.Lookup(in.Cell) == nil {
		return nil, fmt.Errorf("cell %q is not declared in matrix.yaml", in.Cell)
	}

	policy, ok, err := lookupChannelPolicy(ctx, deps.DB.DB, in.Channel)
	if err != nil {
		return nil, fmt.Errorf("channel lookup: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("channel %q not found", in.Channel)
	}

	svc := storeService(deps)
	blob, err := svc.WriteBlob(in.Binary)
	if err != nil {
		return nil, fmt.Errorf("write binary to CAS: %w", err)
	}

	res, err := svc.AttachBinary(ctx, store.AttachInput{
		Channel: in.Channel,
		Name:    in.Name,
		Version: in.Version,
		Policy:  policy,
		Cell:    in.Cell,
		Binary:  blob,
		Actor:   in.Actor,
		Note:    in.Note,
	})
	if err != nil {
		return nil, err
	}

	resp := &PublishResponse{
		Channel:        res.Channel,
		Name:           res.Name,
		Version:        res.Version,
		SourceSHA256:   res.SourceSHA256,
		SourceSize:     res.SourceSize,
		Binaries:       []PublishedBinary{{Cell: res.Cell, SHA256: res.Binary.SHA256, Size: res.Binary.Size}},
		AlreadyExisted: res.AlreadyExisted,
		Overwritten:    res.Overwritten,
	}

	if deps.Index != nil && !res.AlreadyExisted {
		deps.Index.InvalidateChannel(in.Channel)
	}
	if deps.Metrics != nil && !res.AlreadyExisted {
		result := "created"
		if res.Overwritten {
			result = "overwrote"
		}
		deps.Metrics.PublishTotal.WithLabelValues(in.Channel, result).Inc()
	}
	refreshCASBytes(ctx, deps)

	return resp, nil
}

// publishResponseFromStore converts a store.Result into the API's
// PublishResponse shape. Used by both ImportSource and the HTTP
// publish handler (via publishViaStore).
func publishResponseFromStore(r *store.Result) *PublishResponse {
	resp := &PublishResponse{
		Channel:        r.Channel,
		Name:           r.Name,
		Version:        r.Version,
		SourceSHA256:   r.Source.SHA256,
		SourceSize:     r.Source.Size,
		AlreadyExisted: r.AlreadyExisted,
		Overwritten:    r.Overwritten,
	}
	for _, b := range r.Binaries {
		resp.Binaries = append(resp.Binaries, PublishedBinary{
			Cell: b.Cell, SHA256: b.Blob.SHA256, Size: b.Blob.Size,
		})
	}
	return resp
}
