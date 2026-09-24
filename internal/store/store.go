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
	"github.com/schochastics/packyard/internal/rversion"
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
// when binary bundles are imported separately from the source bundle,
// by the proxy fetcher, and by the attach-binary API endpoint.
type AttachInput struct {
	Channel string
	Name    string
	Version string
	Policy  string
	Cell    string
	Binary  BlobRef
	Actor   string
	Note    string // free-form, lands on the event row
	// EventType names the event row; empty means "import_binary".
	EventType string
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

// ErrEquivalentVersion is returned by Materialize when the channel
// already holds the package under a different spelling of the same R
// version ("1.0" vs "1.0.0", "1.0-1" vs "1.0.1"). R treats them as one
// version, so PACKAGES could only ever show one of them.
var ErrEquivalentVersion = errors.New("an equivalent version is already published")

// ErrSourceRowMissing is returned by AttachBinary when the package row
// referenced by the input does not exist. The bundle importer surfaces
// this so operators see a clear error if they try to import a binary
// bundle before its matching source bundle.
var ErrSourceRowMissing = errors.New("source row not found; import the source bundle first")

// BlobStore is the surface the store needs from the CAS layer. Kept as
// an interface so tests can plug in a mock; the concrete impl is
// *cas.Store. Read is used to extract DESCRIPTION metadata from
// tarballs at materialization time.
type BlobStore interface {
	Write(io.Reader) (string, int64, error)
	Read(sum string) (io.ReadCloser, error)
}

// Service owns the materialization primitive. Construct with [New].
type Service struct {
	db  *sql.DB
	cas BlobStore
}

// New constructs a Service. Neither dependency is optional.
func New(db *sql.DB, cas BlobStore) *Service {
	return &Service{db: db, cas: cas}
}

// WriteBlob streams r to CAS and returns a BlobRef. Convenience over
// the [BlobStore.Write] tuple shape for callers that don't otherwise
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
//   - existing + immutable + same  → emit "publish_idempotent",
//     Result.AlreadyExisted=true; binaries in the input follow
//     AttachBinary rules (absent cell → inserted, same bytes → no-op,
//     different bytes → ErrImmutableConflict)
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

	// Tarball metadata is read before the write transaction so the tx
	// never holds the SQLite write lock across CAS I/O.
	fields := s.indexFieldsJSON(in.Source.SHA256, in.Name)
	built := make(map[string]string, len(in.Binaries))
	for _, b := range in.Binaries {
		built[b.Cell] = s.builtField(b.Blob.SHA256, in.Name)
	}

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
		if other, err := equivalentVersion(ctx, tx, in); err != nil {
			return nil, err
		} else if other != "" {
			return nil, fmt.Errorf("%w: %s@%s on channel %s is the same R version as %s",
				ErrEquivalentVersion, in.Name, other, in.Channel, in.Version)
		}
		if err := insertPackageAndBinaries(ctx, tx, in, fields, built, now); err != nil {
			return nil, fmt.Errorf("insert package: %w", err)
		}
		eventType = "publish"

	case in.Policy == config.PolicyImmutable:
		if existingSHA != in.Source.SHA256 {
			return nil, fmt.Errorf("%w: %s@%s on channel %s",
				ErrImmutableConflict, in.Name, in.Version, in.Channel)
		}
		// Idempotent replay on immutable. A replay that carries binaries
		// (CI re-running a publish with a cell that failed last time)
		// gets the same rules as AttachBinary, so the response never
		// claims a binary the DB doesn't have.
		if err := replayBinaries(ctx, tx, existingID, in, built, now); err != nil {
			return nil, err
		}
		result.AlreadyExisted = true
		eventType = "publish_idempotent"

	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE packages
			   SET source_sha256 = ?, source_size = ?, published_at = ?,
			       published_by = ?, yanked = 0, yank_reason = NULL,
			       index_fields = ?
			 WHERE id = ?
		`, in.Source.SHA256, in.Source.Size, now, nullIfEmpty(in.Actor), fields, existingID); err != nil {
			return nil, fmt.Errorf("update package: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM binaries WHERE package_id = ?`, existingID); err != nil {
			return nil, fmt.Errorf("delete old binaries: %w", err)
		}
		if err := insertBinariesFor(ctx, tx, existingID, in.Binaries, built, now); err != nil {
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
	built := s.builtField(in.Binary.SHA256, in.Name)

	// The package row is read inside the write transaction so a
	// concurrent delete can't slip in between the read and the insert.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		packageID  int64
		sourceSHA  string
		sourceSize int64
	)
	err = tx.QueryRowContext(ctx, `
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

	var existingSHA string
	row := tx.QueryRowContext(ctx,
		`SELECT binary_sha256 FROM binaries WHERE package_id = ? AND cell = ?`,
		packageID, in.Cell)
	switch err := row.Scan(&existingSHA); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at, built)
			VALUES (?, ?, ?, ?, ?, ?)
		`, packageID, in.Cell, in.Binary.SHA256, in.Binary.Size, now, built); err != nil {
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
			   SET binary_sha256 = ?, size = ?, uploaded_at = ?, built = ?
			 WHERE package_id = ? AND cell = ?
		`, in.Binary.SHA256, in.Binary.Size, now, built, packageID, in.Cell); err != nil {
			return nil, fmt.Errorf("update binary: %w", err)
		}
		result.Overwritten = true
	}

	if !result.AlreadyExisted {
		eventType := in.EventType
		if eventType == "" {
			eventType = "import_binary"
		}
		note := fmt.Sprintf("cell=%s sha256=%s", in.Cell, in.Binary.SHA256)
		if in.Note != "" {
			note = in.Note + " " + note
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events(at, type, actor, channel, package, version, note)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, now, eventType, nullIfEmpty(in.Actor), in.Channel, in.Name, in.Version, note); err != nil {
			return nil, fmt.Errorf("append event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// equivalentVersion returns an existing version of in.Name in
// in.Channel that R considers equal to in.Version, or "".
func equivalentVersion(ctx context.Context, tx *sql.Tx, in Input) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT version FROM packages WHERE channel = ? AND name = ?`, in.Channel, in.Name)
	if err != nil {
		return "", fmt.Errorf("read versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
		if v != in.Version && rversion.Compare(v, in.Version) == 0 {
			return v, nil
		}
	}
	return "", rows.Err()
}

// replayBinaries inserts the binaries of an idempotent immutable replay
// whose cell is still absent and rejects any whose bytes differ from
// the stored binary.
func replayBinaries(ctx context.Context, tx *sql.Tx, packageID int64, in Input, built map[string]string, now string) error {
	for _, b := range in.Binaries {
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT binary_sha256 FROM binaries WHERE package_id = ? AND cell = ?`,
			packageID, b.Cell).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := insertBinariesFor(ctx, tx, packageID, []BinaryInput{b}, built, now); err != nil {
				return fmt.Errorf("insert binary: %w", err)
			}
		case err != nil:
			return fmt.Errorf("read existing binary: %w", err)
		case existing != b.Blob.SHA256:
			return fmt.Errorf("%w: %s@%s on channel %s, cell %s",
				ErrImmutableConflict, in.Name, in.Version, in.Channel, b.Cell)
		}
	}
	return nil
}

func insertPackageAndBinaries(ctx context.Context, tx *sql.Tx, in Input, fields string, built map[string]string, now string) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size, published_at, published_by, index_fields)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, in.Channel, in.Name, in.Version, in.Source.SHA256, in.Source.Size, now, nullIfEmpty(in.Actor), fields)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return insertBinariesFor(ctx, tx, id, in.Binaries, built, now)
}

func insertBinariesFor(ctx context.Context, tx *sql.Tx, packageID int64, binaries []BinaryInput, built map[string]string, now string) error {
	for _, b := range binaries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at, built)
			VALUES (?, ?, ?, ?, ?, ?)
		`, packageID, b.Cell, b.Blob.SHA256, b.Blob.Size, now, built[b.Cell]); err != nil {
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
