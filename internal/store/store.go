// Package store owns the package-materialization primitive: given a
// set of tarballs already written to CAS plus the metadata needed to
// describe them, reconcile the desired state with the packages /
// binaries / events tables inside a single transaction.
//
// Callers are the HTTP publish handler ([internal/api]), the bundle /
// drat importers ([internal/importers]), and — once it lands — the
// lazy-proxy fetcher ([internal/upstream]). Pulling the primitive out
// of [internal/api] breaks the import cycle that would otherwise form
// when the proxy fetcher (in its own package) needs to write the same
// kind of row from the read path.
//
// The store does not check authorisation, validate the URL shape of
// package names, or decide HTTP status codes — those are the caller's
// concerns. It assumes its inputs are already validated.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/schochastics/packyard/internal/config"
)

// BlobRef is the result of writing one tarball to CAS — the
// lowercase-hex sha256 of its bytes plus the byte count.
type BlobRef struct {
	SHA256 string
	Size   int64
}

// BinaryInput is one precompiled binary already in CAS, keyed by the
// cell name from matrix.yaml.
type BinaryInput struct {
	Cell string
	Blob BlobRef
}

// Input describes a Materialize request. Source and every Binaries
// entry must already be in CAS — call [Service.WriteBlob] first.
type Input struct {
	Channel  string
	Name     string
	Version  string
	Policy   string // config.PolicyMutable | config.PolicyImmutable
	Source   BlobRef
	Binaries []BinaryInput

	// Actor lands in events.actor and packages.published_by. Empty
	// becomes SQL NULL.
	Actor string
}

// Result summarizes what Materialize did. AlreadyExisted is set when
// the same bytes were already present on an immutable channel (the
// no-op retry case); Overwritten is set when a mutable channel
// replaced existing bytes.
type Result struct {
	Channel        string
	Name           string
	Version        string
	Source         BlobRef
	Binaries       []BinaryInput
	AlreadyExisted bool
	Overwritten    bool
}

// AttachInput describes a request to attach one binary to an existing
// (channel, name, version) package row. Used by the bundle importer
// when binary bundles are imported separately from the source bundle.
type AttachInput struct {
	Channel string
	Name    string
	Version string
	Policy  string
	Cell    string
	Binary  BlobRef
	Actor   string
	Note    string // free-form, lands on the import_binary event row
}

// AttachResult mirrors Result for the binary-attach path. The source
// fields describe the existing row that the binary was attached to;
// they are read back from the DB.
type AttachResult struct {
	Channel        string
	Name           string
	Version        string
	SourceSHA256   string
	SourceSize     int64
	Cell           string
	Binary         BlobRef
	AlreadyExisted bool
	Overwritten    bool
}

// ErrImmutableConflict is returned by Materialize / AttachBinary when
// the write would change bytes on an immutable channel.
var ErrImmutableConflict = errors.New("immutable channel already has this version with different content")

// ErrSourceRowMissing is returned by AttachBinary when the package row
// referenced by the input does not exist. The bundle importer surfaces
// this so operators see a clear error if they try to import a binary
// bundle before its matching source bundle.
var ErrSourceRowMissing = errors.New("source row not found; import the source bundle first")

// CASWriter is the surface the store needs from the CAS layer. Kept as
// an interface so tests can plug in a mock; the concrete impl is
// *cas.Store.
type CASWriter interface {
	Write(io.Reader) (string, int64, error)
}

// Service owns the materialization primitive. Construct with [New].
type Service struct {
	db  *sql.DB
	cas CASWriter
}

// New constructs a Service. Neither dependency is optional.
func New(db *sql.DB, cas CASWriter) *Service {
	return &Service{db: db, cas: cas}
}

// WriteBlob streams r to CAS and returns a BlobRef. Convenience over
// the [CASWriter.Write] tuple shape for callers that don't otherwise
// want to think about CAS.
func (s *Service) WriteBlob(r io.Reader) (BlobRef, error) {
	sum, size, err := s.cas.Write(r)
	if err != nil {
		return BlobRef{}, err
	}
	return BlobRef{SHA256: sum, Size: size}, nil
}

// Materialize reconciles a publish/import with the DB inside a single
// transaction. Source and binary blobs must already be in CAS.
//
// Behavior by (existing-row, policy):
//
//   - no existing row              → INSERT packages + binaries, emit "publish"
//   - existing + immutable + same  → no-op, emit "publish_idempotent",
//     Result.AlreadyExisted=true
//   - existing + immutable + diff  → return ErrImmutableConflict, no DB change
//   - existing + mutable           → UPDATE packages, replace binaries, emit
//     "publish_overwrite", Result.Overwritten=true
//
// Event-row attribution: Actor lands in events.actor; the event note
// stays NULL here. Callers that want to add additional event rows
// (e.g. the importers' "import" attribution event) append them after
// Materialize returns.
func (s *Service) Materialize(ctx context.Context, in Input) (*Result, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var (
		existingID  int64
		existingSHA string
		exists      bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, source_sha256 FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, in.Channel, in.Name, in.Version).Scan(&existingID, &existingSHA)
	switch {
	case err == nil:
		exists = true
	case errors.Is(err, sql.ErrNoRows):
		exists = false
	default:
		return nil, fmt.Errorf("read existing package: %w", err)
	}

	result := &Result{
		Channel:  in.Channel,
		Name:     in.Name,
		Version:  in.Version,
		Source:   in.Source,
		Binaries: append([]BinaryInput(nil), in.Binaries...),
	}

	var eventType string
	switch {
	case !exists:
		if err := insertPackageAndBinaries(ctx, tx, in, now); err != nil {
			return nil, fmt.Errorf("insert package: %w", err)
		}
		eventType = "publish"

	case in.Policy == config.PolicyImmutable:
		if existingSHA != in.Source.SHA256 {
			return nil, fmt.Errorf("%w: %s@%s on channel %s",
				ErrImmutableConflict, in.Name, in.Version, in.Channel)
		}
		// Idempotent replay on immutable: no DB write beyond the event.
		// Binaries are not touched here — to add a cell to an existing
		// immutable version, use AttachBinary.
		result.AlreadyExisted = true
		eventType = "publish_idempotent"

	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE packages
			   SET source_sha256 = ?, source_size = ?, published_at = ?,
			       published_by = ?, yanked = 0, yank_reason = NULL
			 WHERE id = ?
		`, in.Source.SHA256, in.Source.Size, now, nullIfEmpty(in.Actor), existingID); err != nil {
			return nil, fmt.Errorf("update package: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM binaries WHERE package_id = ?`, existingID); err != nil {
			return nil, fmt.Errorf("delete old binaries: %w", err)
		}
		if err := insertBinariesFor(ctx, tx, existingID, in.Binaries, now); err != nil {
			return nil, fmt.Errorf("insert replacement binaries: %w", err)
		}
		result.Overwritten = true
		eventType = "publish_overwrite"
	}

	if err := appendEvent(ctx, tx, eventType, in.Channel, in.Name, in.Version, in.Actor, ""); err != nil {
		return nil, fmt.Errorf("append event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// AttachBinary attaches one binary tarball to an existing
// (channel, name, version) package row. The binary blob must already
// be in CAS.
//
// Behavior intentionally differs from Materialize on immutable
// channels:
//
//   - immutable + cell absent     → INSERT (adding a new cell to an
//     existing immutable version is allowed)
//   - immutable + same sha        → AlreadyExisted=true, no-op
//   - immutable + different sha   → ErrImmutableConflict
//   - mutable                     → INSERT or UPDATE
//
// Materialize refuses to add binaries to an existing immutable version
// because the publish handler can't tell "operator forgot a cell" from
// "supply-chain attack". The bundle import path is operator-driven and
// explicitly composes source + binary imports in separate steps, so
// the diff is intentional.
func (s *Service) AttachBinary(ctx context.Context, in AttachInput) (*AttachResult, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	var (
		packageID  int64
		sourceSHA  string
		sourceSize int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, source_sha256, source_size FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, in.Channel, in.Name, in.Version).Scan(&packageID, &sourceSHA, &sourceSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s@%s on channel %s", ErrSourceRowMissing, in.Name, in.Version, in.Channel)
	}
	if err != nil {
		return nil, fmt.Errorf("read package row: %w", err)
	}

	result := &AttachResult{
		Channel:      in.Channel,
		Name:         in.Name,
		Version:      in.Version,
		SourceSHA256: sourceSHA,
		SourceSize:   sourceSize,
		Cell:         in.Cell,
		Binary:       in.Binary,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSHA string
	row := tx.QueryRowContext(ctx,
		`SELECT binary_sha256 FROM binaries WHERE package_id = ? AND cell = ?`,
		packageID, in.Cell)
	switch err := row.Scan(&existingSHA); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at)
			VALUES (?, ?, ?, ?, ?)
		`, packageID, in.Cell, in.Binary.SHA256, in.Binary.Size, now); err != nil {
			return nil, fmt.Errorf("insert binary: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("read existing binary: %w", err)
	case existingSHA == in.Binary.SHA256:
		result.AlreadyExisted = true
	case in.Policy == config.PolicyImmutable:
		return nil, fmt.Errorf("%w: %s@%s on channel %s, cell %s",
			ErrImmutableConflict, in.Name, in.Version, in.Channel, in.Cell)
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE binaries
			   SET binary_sha256 = ?, size = ?, uploaded_at = ?
			 WHERE package_id = ? AND cell = ?
		`, in.Binary.SHA256, in.Binary.Size, now, packageID, in.Cell); err != nil {
			return nil, fmt.Errorf("update binary: %w", err)
		}
		result.Overwritten = true
	}

	if !result.AlreadyExisted {
		note := fmt.Sprintf("cell=%s sha256=%s", in.Cell, in.Binary.SHA256)
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
	return result, nil
}

func insertPackageAndBinaries(ctx context.Context, tx *sql.Tx, in Input, now string) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size, published_at, published_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, in.Channel, in.Name, in.Version, in.Source.SHA256, in.Source.Size, now, nullIfEmpty(in.Actor))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return insertBinariesFor(ctx, tx, id, in.Binaries, now)
}

func insertBinariesFor(ctx context.Context, tx *sql.Tx, packageID int64, binaries []BinaryInput, now string) error {
	for _, b := range binaries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at)
			VALUES (?, ?, ?, ?, ?)
		`, packageID, b.Cell, b.Blob.SHA256, b.Blob.Size, now); err != nil {
			return err
		}
	}
	return nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType, channel, name, version, actor, note string) error {
	var noteArg any
	if note != "" {
		noteArg = note
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events(at, type, actor, channel, package, version, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, now, eventType, nullIfEmpty(actor), channel, name, version, noteArg)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
