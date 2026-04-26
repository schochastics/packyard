package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/config"
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

// ErrSourceRowMissing is returned by AttachBinaries when the
// (channel, name, version) package row doesn't exist. The bundle
// import flow imports source first and binaries second; an air-gap
// operator who tries to attach binaries before source sees this.
var ErrSourceRowMissing = errors.New("source row not found; import the source bundle first")

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
// Channel overwrite_policy semantics intentionally diverge from
// persistPublish on immutable channels:
//
//   - immutable + cell absent     → INSERT (adding a new cell to an
//     existing immutable version is allowed)
//   - immutable + same sha        → AlreadyExisted=true, no-op
//   - immutable + different sha   → ErrImmutableConflict
//   - mutable                     → INSERT or REPLACE
//
// persistPublish refuses to add binaries to an existing immutable
// version because the publish handler can't tell "operator forgot a
// cell" from "supply-chain attack". The bundle import path is
// operator-driven, has a sha256-validated manifest, and explicitly
// composes source + binary imports in separate steps; the diff is
// intentional.
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

	var (
		packageID  int64
		sourceSHA  string
		sourceSize int64
	)
	err = deps.DB.QueryRowContext(ctx, `
		SELECT id, source_sha256, source_size FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, in.Channel, in.Name, in.Version).Scan(&packageID, &sourceSHA, &sourceSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s@%s on channel %s", ErrSourceRowMissing, in.Name, in.Version, in.Channel)
	}
	if err != nil {
		return nil, fmt.Errorf("read package row: %w", err)
	}

	binSHA, binSize, err := deps.CAS.Write(in.Binary)
	if err != nil {
		return nil, fmt.Errorf("write binary to CAS: %w", err)
	}

	resp := &PublishResponse{
		Channel:      in.Channel,
		Name:         in.Name,
		Version:      in.Version,
		SourceSHA256: sourceSHA,
		SourceSize:   sourceSize,
		Binaries: []PublishedBinary{
			{Cell: in.Cell, SHA256: binSHA, Size: binSize},
		},
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback() // no-op after Commit
	}()

	var existingSHA string
	row := tx.QueryRowContext(ctx,
		`SELECT binary_sha256 FROM binaries WHERE package_id = ? AND cell = ?`,
		packageID, in.Cell)
	switch err := row.Scan(&existingSHA); {
	case errors.Is(err, sql.ErrNoRows):
		// Insert a new binary row. Allowed regardless of overwrite policy.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at)
			VALUES (?, ?, ?, ?, ?)
		`, packageID, in.Cell, binSHA, binSize, now); err != nil {
			return nil, fmt.Errorf("insert binary: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("read existing binary: %w", err)
	case existingSHA == binSHA:
		// Idempotent re-attach: identical bytes, no work to do.
		resp.AlreadyExisted = true
	case policy == config.PolicyImmutable:
		return nil, fmt.Errorf("%w: %s@%s on channel %s, cell %s",
			ErrImmutableConflict, in.Name, in.Version, in.Channel, in.Cell)
	default:
		// Mutable channel + different bytes: replace.
		if _, err := tx.ExecContext(ctx, `
			UPDATE binaries
			   SET binary_sha256 = ?, size = ?, uploaded_at = ?
			 WHERE package_id = ? AND cell = ?
		`, binSHA, binSize, now, packageID, in.Cell); err != nil {
			return nil, fmt.Errorf("update binary: %w", err)
		}
		resp.Overwritten = true
	}

	if !resp.AlreadyExisted {
		// Append an attribution event so /ui/events shows the binary
		// import alongside the source publish.
		note := fmt.Sprintf("cell=%s sha256=%s", in.Cell, binSHA)
		if in.Note != "" {
			note = in.Note + " " + note
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events(at, type, actor, channel, package, version, note)
			VALUES (?, 'import_binary', ?, ?, ?, ?, ?)
		`, now, nullIfEmpty(in.Actor), in.Channel, in.Name, in.Version, note); err != nil {
			return nil, fmt.Errorf("append event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	if deps.Index != nil && !resp.AlreadyExisted {
		deps.Index.InvalidateChannel(in.Channel)
	}
	if deps.Metrics != nil && !resp.AlreadyExisted {
		result := "created"
		if resp.Overwritten {
			result = "overwrote"
		}
		deps.Metrics.PublishTotal.WithLabelValues(in.Channel, result).Inc()
	}
	refreshCASBytes(ctx, deps)

	return resp, nil
}
