package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/schochastics/pakman/internal/auth"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
)

// maxRequestBytes caps a single publish upload. 2 GiB is well above the
// largest R package we've seen in the wild (a few hundred MB for
// Bioconductor heavyweights) while still blocking trivial DoS uploads.
// Admins who need more can patch this; v1 doesn't expose it as config.
const maxRequestBytes = 2 << 30

// maxManifestBytes is the limit on the manifest JSON part. Manifests
// are small (a few cells × a few fields); anything over 1 MiB is
// almost certainly a malformed body or an attack.
const maxManifestBytes = 1 << 20

// packageNameRE is the allowed shape of an R package name. R's own
// rules: letters, digits, and dots; must start with a letter and must
// not end with a dot. We enforce this at the URL boundary so a
// deliberately malformed name can't slip into the DB or an event row.
var packageNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*[A-Za-z0-9]$`)

// versionRE matches an R package version. R accepts dot- and
// dash-separated numbers; we accept the same subset.
var versionRE = regexp.MustCompile(`^[0-9][0-9.\-]*[0-9]$`)

// Manifest is the JSON body carried in the "manifest" multipart part.
// Clients describe their upload as:
//
//	{
//	  "source": "source",
//	  "binaries": [{"cell": "ubuntu-22.04-amd64-r-4.4", "part": "bin1"}]
//	}
//
// "source" and "binaries[].part" name other multipart parts. Fields
// not listed are rejected (DisallowUnknownFields) so typos surface.
type Manifest struct {
	Source             string            `json:"source"`
	DescriptionVersion string            `json:"description_version,omitempty"`
	Binaries           []ManifestBinary  `json:"binaries,omitempty"`
	PublishedBy        string            `json:"-"` // populated from token label
	Extra              map[string]string `json:"-"`
}

// ManifestBinary describes a single binary in a publish manifest.
type ManifestBinary struct {
	Cell string `json:"cell"`
	Part string `json:"part"`
}

// PublishResponse is the JSON body returned on a successful publish.
type PublishResponse struct {
	Channel        string            `json:"channel"`
	Name           string            `json:"name"`
	Version        string            `json:"version"`
	SourceSHA256   string            `json:"source_sha256"`
	SourceSize     int64             `json:"source_size"`
	Binaries       []PublishedBinary `json:"binaries"`
	AlreadyExisted bool              `json:"already_existed"`
	Overwritten    bool              `json:"overwritten"`
}

// PublishedBinary is one row in PublishResponse.Binaries.
type PublishedBinary struct {
	Cell   string `json:"cell"`
	SHA256 string `json:"binary_sha256"`
	Size   int64  `json:"size"`
}

// handlePublish accepts POST /api/v1/packages/{channel}/{name}/{version}
// with a multipart body: exactly one "manifest" part (small JSON), a
// source tarball part whose name matches manifest.source, and zero or
// more binary parts whose names match manifest.binaries[].part.
//
// Flow, condensed:
//  1. Validate URL params and scope.
//  2. Look up channel's overwrite_policy; 404 if the channel is unknown.
//  3. Stream every non-manifest part to CAS, collecting {name: (sum,size)}.
//     Content-addressable storage makes duplicate writes free; if the
//     request ultimately fails we orphan blobs for GC to reclaim.
//  4. Parse manifest, resolve source and binary parts, validate cells.
//  5. Reconcile with existing (channel, name, version) row in a single tx.
//     Immutable + identical source → 200 already_existed. Immutable +
//     different bytes → 409. Mutable → replace package + binaries rows.
//  6. Append an event row inside the same tx.
func handlePublish(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		name := r.PathValue("name")
		version := r.PathValue("version")

		if !packageNameRE.MatchString(name) {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, "invalid package name",
				"R package names start with a letter, contain letters/digits/dots, end alphanumeric")
			return
		}
		if !versionRE.MatchString(version) {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, "invalid version",
				"versions must be numeric, dot- and dash-separated (e.g. 1.2.3 or 0.9-1)")
			return
		}
		if !requireScope(w, r, "publish:"+channel) {
			return
		}

		policy, ok, err := lookupChannelPolicy(r.Context(), deps.DB.DB, channel)
		switch {
		case err != nil:
			writeError(w, r, http.StatusInternalServerError,
				CodeInternal, "channel lookup failed", "see server logs")
			return
		case !ok:
			writeError(w, r, http.StatusNotFound,
				CodeNotFound, fmt.Sprintf("channel %q not found", channel),
				"add the channel to channels.yaml and restart the server")
			return
		}

		// Cap the request size. MaxBytesReader returns a specific error
		// when the limit is hit; we translate that to 413.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

		manifest, parts, perr := streamMultipartToCAS(r, deps.CAS)
		if perr != nil {
			perr.write(w, r)
			return
		}

		if err := validateManifest(manifest, parts, deps.Matrix, version); err != nil {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, err.Error(),
				"see /api/v1/cells for the list of configured cells")
			return
		}

		id, _ := IdentityFromContext(r.Context())
		manifest.PublishedBy = id.Label

		resp, herr := persistPublish(r.Context(), deps.DB.DB, publishInput{
			channel:   channel,
			name:      name,
			version:   version,
			policy:    policy,
			manifest:  manifest,
			parts:     parts,
			publisher: id,
		})
		if herr != nil {
			herr.write(w, r)
			return
		}

		// New content means the cached PACKAGES is stale. Idempotent
		// replays leave state unchanged, so we skip invalidation on
		// AlreadyExisted to avoid needless churn on replay floods.
		if !resp.AlreadyExisted && deps.Index != nil {
			deps.Index.InvalidateChannel(channel)
		}

		recordPublishMetric(deps, channel, resp)
		refreshCASBytes(r.Context(), deps)

		status := http.StatusCreated
		if resp.AlreadyExisted {
			status = http.StatusOK
		}
		writeJSON(w, r, status, resp)
	}
}

// recordPublishMetric bumps pakman_publish_total with a result label
// that distinguishes created / overwrote / already_existed so dashboards
// can separate "new versions" from "replay traffic" from "CI overwriting
// the same dev version 100 times".
func recordPublishMetric(deps Deps, channel string, resp *PublishResponse) {
	if deps.Metrics == nil {
		return
	}
	result := "created"
	switch {
	case resp.AlreadyExisted:
		result = "already_existed"
	case resp.Overwritten:
		result = "overwrote"
	}
	deps.Metrics.PublishTotal.WithLabelValues(channel, result).Inc()
}

// lookupChannelPolicy returns the overwrite_policy for a channel and a
// present flag. An error here is a DB error, not an absent channel.
func lookupChannelPolicy(ctx context.Context, db *sql.DB, name string) (string, bool, error) {
	var policy string
	err := db.QueryRowContext(ctx,
		`SELECT overwrite_policy FROM channels WHERE name = ?`, name,
	).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return policy, true, nil
}

// httpError is an error that already knows its HTTP representation.
// Handlers and helpers pass these around to avoid threading
// (status, code, message, hint) tuples through every return site.
type httpError struct {
	status int
	code   string
	msg    string
	hint   string
}

func (e *httpError) Error() string { return e.msg }
func (e *httpError) write(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, e.status, e.code, e.msg, e.hint)
}

// partRef is the CAS result for one non-manifest multipart part.
type partRef struct {
	sha256 string
	size   int64
}

// streamMultipartToCAS consumes the multipart body. The "manifest"
// part is buffered in memory and JSON-decoded; every other part is
// streamed directly into CAS. Returns an httpError for client-facing
// failures (malformed body, oversize manifest, duplicate parts).
func streamMultipartToCAS(r *http.Request, store *cas.Store) (Manifest, map[string]partRef, *httpError) {
	mr, err := r.MultipartReader()
	if err != nil {
		return Manifest{}, nil, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "request must be multipart/form-data",
			hint:   "set Content-Type: multipart/form-data; boundary=... and include a 'manifest' part",
		}
	}

	var manifest Manifest
	manifestSeen := false
	parts := map[string]partRef{}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, nil, multipartErr(err)
		}

		name := part.FormName()
		if name == "" {
			_ = part.Close()
			return Manifest{}, nil, &httpError{
				status: http.StatusBadRequest,
				code:   CodeBadRequest,
				msg:    "multipart part has no name",
				hint:   "every part needs a Content-Disposition: form-data; name=...",
			}
		}

		if name == "manifest" {
			if manifestSeen {
				_ = part.Close()
				return Manifest{}, nil, &httpError{
					status: http.StatusBadRequest,
					code:   CodeBadRequest,
					msg:    "multiple 'manifest' parts",
					hint:   "send exactly one manifest part per publish",
				}
			}
			manifestSeen = true
			if herr := decodeManifest(part, &manifest); herr != nil {
				_ = part.Close()
				return Manifest{}, nil, herr
			}
			_ = part.Close()
			continue
		}

		if _, dup := parts[name]; dup {
			_ = part.Close()
			return Manifest{}, nil, &httpError{
				status: http.StatusBadRequest,
				code:   CodeBadRequest,
				msg:    fmt.Sprintf("duplicate multipart part name %q", name),
				hint:   "every data part needs a unique name",
			}
		}

		sum, size, cerr := store.Write(part)
		_ = part.Close()
		if cerr != nil {
			return Manifest{}, nil, &httpError{
				status: http.StatusInternalServerError,
				code:   CodeInternal,
				msg:    "failed to write blob to CAS",
				hint:   "see server logs",
			}
		}
		parts[name] = partRef{sha256: sum, size: size}
	}

	if !manifestSeen {
		return Manifest{}, nil, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "missing 'manifest' part",
			hint:   "include a multipart part named 'manifest' with the publish JSON",
		}
	}

	return manifest, parts, nil
}

// decodeManifest reads up to maxManifestBytes from r and JSON-decodes
// into m. Strict: unknown fields fail.
func decodeManifest(r io.Reader, m *Manifest) *httpError {
	// Read one extra byte to detect oversize.
	limited := io.LimitReader(r, maxManifestBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "failed to read manifest part",
			hint:   err.Error(),
		}
	}
	if int64(len(buf)) > maxManifestBytes {
		return &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "manifest part is too large",
			hint:   fmt.Sprintf("limit is %d bytes", maxManifestBytes),
		}
	}

	dec := json.NewDecoder(io.NopCloser(bytesReader(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(m); err != nil {
		return &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "manifest is not valid JSON",
			hint:   err.Error(),
		}
	}
	return nil
}

// bytesReader is a tiny helper so decodeManifest doesn't need an extra
// dep on bytes.NewReader while keeping the stdlib import list tight.
type bytesReader []byte

func (b bytesReader) Read(p []byte) (int, error) {
	if len(b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b)
	// We return EOF on the final read so callers see it in one shot.
	if n == len(b) {
		return n, io.EOF
	}
	return n, nil
}

// multipartErr maps a MaxBytesReader or parser error to an httpError.
func multipartErr(err error) *httpError {
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) {
		return &httpError{
			status: http.StatusRequestEntityTooLarge,
			code:   CodePayloadTooLarge,
			msg:    "request body exceeds publish size limit",
			hint:   fmt.Sprintf("limit is %d bytes", maxRequestBytes),
		}
	}
	return &httpError{
		status: http.StatusBadRequest,
		code:   CodeBadRequest,
		msg:    "failed to parse multipart body",
		hint:   err.Error(),
	}
}

// validateManifest checks the manifest against the known parts and the
// server's matrix config. Returns a plain error — the caller wraps as
// an httpError to keep this function easy to call from tests.
func validateManifest(m Manifest, parts map[string]partRef, matrix *config.MatrixConfig, urlVersion string) error {
	if m.Source == "" {
		return errors.New("manifest.source is required")
	}
	if _, ok := parts[m.Source]; !ok {
		return fmt.Errorf("manifest.source references part %q, which was not uploaded", m.Source)
	}
	if m.DescriptionVersion != "" && m.DescriptionVersion != urlVersion {
		return fmt.Errorf("manifest.description_version %q does not match URL version %q",
			m.DescriptionVersion, urlVersion)
	}

	seenCells := map[string]struct{}{}
	for i, b := range m.Binaries {
		if b.Cell == "" {
			return fmt.Errorf("binaries[%d].cell is required", i)
		}
		if b.Part == "" {
			return fmt.Errorf("binaries[%d].part is required", i)
		}
		if _, dup := seenCells[b.Cell]; dup {
			return fmt.Errorf("binaries[%d].cell %q appears twice", i, b.Cell)
		}
		seenCells[b.Cell] = struct{}{}
		if matrix != nil && matrix.Lookup(b.Cell) == nil {
			return fmt.Errorf("binaries[%d].cell %q is not declared in matrix.yaml", i, b.Cell)
		}
		if _, ok := parts[b.Part]; !ok {
			return fmt.Errorf("binaries[%d].part %q was not uploaded", i, b.Part)
		}
	}
	return nil
}

// publishInput is the assembled state the persist step works from.
type publishInput struct {
	channel   string
	name      string
	version   string
	policy    string
	manifest  Manifest
	parts     map[string]partRef
	publisher auth.Identity
}

// persistPublish reconciles a validated publish with the DB inside a
// single transaction. Returns a PublishResponse or an httpError.
func persistPublish(ctx context.Context, db *sql.DB, in publishInput) (*PublishResponse, *httpError) {
	src := in.parts[in.manifest.Source]
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalErr("begin tx", err)
	}
	defer func() {
		_ = tx.Rollback() // no-op after Commit
	}()

	var (
		existingID     int64
		existingSHA    string
		existingPolicy = in.policy
		exists         bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, source_sha256 FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, in.channel, in.name, in.version).Scan(&existingID, &existingSHA)
	switch {
	case err == nil:
		exists = true
	case errors.Is(err, sql.ErrNoRows):
		exists = false
	default:
		return nil, internalErr("read existing package", err)
	}

	resp := &PublishResponse{
		Channel:      in.channel,
		Name:         in.name,
		Version:      in.version,
		SourceSHA256: src.sha256,
		SourceSize:   src.size,
	}
	for _, b := range in.manifest.Binaries {
		p := in.parts[b.Part]
		resp.Binaries = append(resp.Binaries, PublishedBinary{
			Cell: b.Cell, SHA256: p.sha256, Size: p.size,
		})
	}

	switch {
	case !exists:
		if err := insertPackageAndBinaries(ctx, tx, in, now); err != nil {
			return nil, internalErr("insert package", err)
		}
		if err := appendEvent(ctx, tx, "publish", in, now); err != nil {
			return nil, internalErr("append event", err)
		}
	case existingPolicy == config.PolicyImmutable:
		if existingSHA != src.sha256 {
			return nil, &httpError{
				status: http.StatusConflict,
				code:   CodeVersionImmutable,
				msg:    fmt.Sprintf("%s@%s already exists on immutable channel %s with different content", in.name, in.version, in.channel),
				hint:   "bump the version, or republish with byte-identical content",
			}
		}
		// Idempotent re-publish on immutable: tell the caller no new
		// work was done. We don't touch binaries here; to add a cell to
		// an already-published immutable version, delete and republish.
		resp.AlreadyExisted = true
		if err := appendEvent(ctx, tx, "publish_idempotent", in, now); err != nil {
			return nil, internalErr("append event", err)
		}
	default:
		// Mutable channel: replace. Old source and old binary blobs
		// stay in CAS until GC reclaims them.
		if _, err := tx.ExecContext(ctx, `
			UPDATE packages
			   SET source_sha256 = ?, source_size = ?, published_at = ?,
			       published_by = ?, yanked = 0, yank_reason = NULL
			 WHERE id = ?
		`, src.sha256, src.size, now, nullIfEmpty(in.publisher.Label), existingID); err != nil {
			return nil, internalErr("update package", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM binaries WHERE package_id = ?`, existingID); err != nil {
			return nil, internalErr("delete old binaries", err)
		}
		if err := insertBinariesFor(ctx, tx, existingID, in, now); err != nil {
			return nil, internalErr("insert replacement binaries", err)
		}
		resp.Overwritten = true
		if err := appendEvent(ctx, tx, "publish_overwrite", in, now); err != nil {
			return nil, internalErr("append event", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, internalErr("commit", err)
	}
	return resp, nil
}

func insertPackageAndBinaries(ctx context.Context, tx *sql.Tx, in publishInput, now string) error {
	src := in.parts[in.manifest.Source]
	res, err := tx.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size, published_at, published_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, in.channel, in.name, in.version, src.sha256, src.size, now, nullIfEmpty(in.publisher.Label))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return insertBinariesFor(ctx, tx, id, in, now)
}

func insertBinariesFor(ctx context.Context, tx *sql.Tx, packageID int64, in publishInput, now string) error {
	for _, b := range in.manifest.Binaries {
		p := in.parts[b.Part]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO binaries(package_id, cell, binary_sha256, size, uploaded_at)
			VALUES (?, ?, ?, ?, ?)
		`, packageID, b.Cell, p.sha256, p.size, now); err != nil {
			return err
		}
	}
	return nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType string, in publishInput, now string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events(at, type, actor, channel, package, version, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, now, eventType, nullIfEmpty(in.publisher.Label), in.channel, in.name, in.version, nil)
	return err
}

// nullIfEmpty converts "" → NULL for sql.NullString columns we want to
// store as NULL rather than the empty string.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// internalErr is a convenience wrapper that logs via the standard error
// envelope and returns a pointer suitable for a handler's return site.
func internalErr(what string, err error) *httpError {
	return &httpError{
		status: http.StatusInternalServerError,
		code:   CodeInternal,
		msg:    what + ": " + err.Error(),
		hint:   "see server logs",
	}
}
