package importers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schochastics/packyard/internal/api"
)

// Bundle schemas the importer accepts. v1 is the original source-only
// shape from design.md §10.2. v2 adds a Kind discriminator + a
// per-package union of source / binaries blobs so a single bundle can
// carry pre-built tarballs for one cell. Both schemas are still
// produced in the wild; v1 manifests are normalised to v2 on read so
// the rest of the importer only walks one shape.
const (
	BundleSchemaV1 = "packyard-bundle/1"
	BundleSchemaV2 = "packyard-bundle/2"
)

// BundleSchema is the schema string newly-built bundles emit. Tests
// and producers reach for it; we point it at the latest version.
const BundleSchema = BundleSchemaV2

// Bundle kinds. v2 only.
const (
	BundleKindSource = "source"
	BundleKindBinary = "binary"
)

// BundleManifest is the in-memory shape after reading. Mirrors the v2
// schema; v1 archives are upgraded into this shape inside readManifest
// so callers don't see the legacy flat layout.
type BundleManifest struct {
	Schema        string                  `json:"schema"`
	SnapshotID    string                  `json:"snapshot_id"`
	RVersion      string                  `json:"r_version"`
	SourceURL     string                  `json:"source_url"`
	Mode          string                  `json:"mode"`
	Kind          string                  `json:"kind,omitempty"`
	Cell          string                  `json:"cell,omitempty"`
	CreatedAt     string                  `json:"created_at"`
	Tool          string                  `json:"tool"`
	InputPackages []string                `json:"input_packages,omitempty"`
	Packages      []BundleManifestPackage `json:"packages"`
}

// BundleManifestPackage is one entry in the manifest. Exactly one of
// Source / Binaries is populated, matching the bundle's Kind.
type BundleManifestPackage struct {
	Name     string                 `json:"name"`
	Version  string                 `json:"version"`
	Source   *BundleManifestBlob    `json:"source,omitempty"`
	Binaries []BundleManifestBinary `json:"binaries,omitempty"`
}

// BundleManifestBlob is the locator + integrity metadata for one
// tarball inside the bundle. Path is relative to the bundle root.
type BundleManifestBlob struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// BundleManifestBinary is a per-cell binary entry. Cell must match a
// cell declared in matrix.yaml on the receiving server.
type BundleManifestBinary struct {
	Cell string `json:"cell"`
	BundleManifestBlob
}

// bundleManifestV1 is the on-disk v1 layout: flat Path/Sha256/Size
// per package, no Kind, no per-package union. We unmarshal into this
// when the schema header says v1 and convert to v2 immediately so the
// rest of the importer doesn't branch.
type bundleManifestV1 struct {
	Schema        string                    `json:"schema"`
	SnapshotID    string                    `json:"snapshot_id"`
	RVersion      string                    `json:"r_version"`
	SourceURL     string                    `json:"source_url"`
	Mode          string                    `json:"mode"`
	CreatedAt     string                    `json:"created_at"`
	Tool          string                    `json:"tool"`
	InputPackages []string                  `json:"input_packages,omitempty"`
	Packages      []bundleManifestV1Package `json:"packages"`
}

type bundleManifestV1Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Sha256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// BundleImporter consumes a packyard-bundle/{1,2} bundle (directory or
// .tar.gz) and imports each package into the configured packyard
// channel. The channel must already exist with a documented overwrite
// policy — the importer doesn't auto-create channels because
// channels.yaml is the source of truth for policy.
type BundleImporter struct {
	Deps    api.Deps
	Channel string
	Actor   string // event.actor tag; defaults to "import-bundle"
}

// BundleResult is the outcome of one BundleImporter.Run.
type BundleResult struct {
	Manifest *BundleManifest
	Imported []string // "<pkg>@<ver>" of packages newly created
	Skipped  []string // already-existing entries (idempotent replays)
	Failed   []BundleFailure
}

// BundleFailure is one package that couldn't be imported. Pre-flight
// sha256 mismatches are returned as a fatal error from Run, not
// recorded here; this list is for per-package errors during the
// import phase.
type BundleFailure struct {
	Package string
	Version string
	Err     error
}

// NewBundleImporter constructs a BundleImporter with sensible
// defaults. The deps must be the same Deps a running server uses —
// the importer writes to the same CAS and DB, and reads matrix.yaml
// for cell validation on binary bundles.
func NewBundleImporter(deps api.Deps, channel string) *BundleImporter {
	return &BundleImporter{
		Deps:    deps,
		Channel: channel,
		Actor:   "import-bundle",
	}
}

// Run imports a bundle from path. path may be either a directory laid
// out as documented in design.md §10.2, or a .tar.gz / .tgz archive
// of one. progress, when non-nil, receives a short status line at
// each pre-flight + import step.
//
// Atomicity: if any sha256 mismatches the manifest, Run returns
// before touching CAS or the DB. Per-package failures during import
// go into result.Failed and Run continues so one bad tarball doesn't
// block a 500-package bundle.
func (b *BundleImporter) Run(ctx context.Context, path string, progress func(string)) (*BundleResult, error) {
	root, cleanup, err := b.resolveRoot(path)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle path: %w", err)
	}
	defer cleanup()

	manifest, err := readManifest(root)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if progress != nil {
		progress(fmt.Sprintf("manifest ok: schema=%s snapshot=%s mode=%s kind=%s packages=%d",
			manifest.Schema, manifest.SnapshotID, manifest.Mode, manifest.Kind, len(manifest.Packages)))
	}

	// For binary bundles, fail fast if the cell isn't declared in
	// matrix.yaml. The per-package AttachBinaries call would reject it
	// too, but doing it here means we don't pre-flight 500 sha256s of
	// blobs that can never be imported.
	if manifest.Kind == BundleKindBinary {
		if b.Deps.Matrix == nil {
			return nil, errors.New("server has no matrix config; cannot import a binary bundle")
		}
		if b.Deps.Matrix.Lookup(manifest.Cell) == nil {
			return nil, fmt.Errorf("bundle cell %q is not declared in matrix.yaml", manifest.Cell)
		}
	}

	if progress != nil {
		progress(fmt.Sprintf("pre-flight: verifying sha256 of %d tarballs...", countBlobs(manifest)))
	}
	if err := preflightSha256(root, manifest); err != nil {
		return nil, err
	}

	res := &BundleResult{Manifest: manifest}
	for _, p := range manifest.Packages {
		tag := p.Name + "@" + p.Version
		if progress != nil {
			progress("importing " + tag)
		}

		resp, err := b.importOne(ctx, root, manifest, p)
		if err != nil {
			res.Failed = append(res.Failed, BundleFailure{Package: p.Name, Version: p.Version, Err: err})
			if progress != nil {
				progress("  failed: " + err.Error())
			}
			continue
		}
		if resp.AlreadyExisted {
			res.Skipped = append(res.Skipped, tag)
			if progress != nil {
				progress("  skipped (already present)")
			}
		} else {
			res.Imported = append(res.Imported, tag)
		}
	}
	return res, nil
}

// resolveRoot returns a directory to import from, plus a cleanup
// callback. Directories are used in place; archives are extracted to
// a tempdir.
func (b *BundleImporter) resolveRoot(path string) (string, func(), error) {
	noop := func() {}
	info, err := os.Stat(path)
	if err != nil {
		return "", noop, err
	}
	if info.IsDir() {
		return path, noop, nil
	}

	low := strings.ToLower(path)
	if !strings.HasSuffix(low, ".tar.gz") && !strings.HasSuffix(low, ".tgz") {
		return "", noop, fmt.Errorf("path %q is neither a directory nor a .tar.gz archive", path)
	}

	tmpdir, err := os.MkdirTemp("", "packyard-bundle-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpdir) }

	if err := extractTarGz(path, tmpdir); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("extract %s: %w", path, err)
	}
	return tmpdir, cleanup, nil
}

// readManifest accepts both v1 and v2. v1 is upgraded into v2 in
// memory so the rest of the importer can ignore the legacy shape.
func readManifest(root string) (*BundleManifest, error) {
	body, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}

	// Peek at the schema field to dispatch.
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}

	switch head.Schema {
	case BundleSchemaV1:
		var v1 bundleManifestV1
		if err := json.Unmarshal(body, &v1); err != nil {
			return nil, fmt.Errorf("parse manifest.json: %w", err)
		}
		return upgradeV1Manifest(&v1)
	case BundleSchemaV2:
		var m BundleManifest
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("parse manifest.json: %w", err)
		}
		if err := validateV2Manifest(&m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("unsupported manifest schema %q (this importer accepts %q and %q)",
			head.Schema, BundleSchemaV1, BundleSchemaV2)
	}
}

// upgradeV1Manifest converts a parsed v1 manifest into the v2 shape.
// v1 is implicitly source-only; every package gets a synthetic Source
// blob populated from the flat fields.
func upgradeV1Manifest(v1 *bundleManifestV1) (*BundleManifest, error) {
	if v1.SnapshotID == "" {
		return nil, errors.New("manifest.snapshot_id is empty")
	}
	if len(v1.Packages) == 0 {
		return nil, errors.New("manifest.packages is empty")
	}
	out := &BundleManifest{
		Schema:        BundleSchemaV1,
		SnapshotID:    v1.SnapshotID,
		RVersion:      v1.RVersion,
		SourceURL:     v1.SourceURL,
		Mode:          v1.Mode,
		Kind:          BundleKindSource,
		CreatedAt:     v1.CreatedAt,
		Tool:          v1.Tool,
		InputPackages: v1.InputPackages,
	}
	for _, p := range v1.Packages {
		out.Packages = append(out.Packages, BundleManifestPackage{
			Name:    p.Name,
			Version: p.Version,
			Source: &BundleManifestBlob{
				Path:   p.Path,
				Sha256: p.Sha256,
				Size:   p.Size,
			},
		})
	}
	return out, nil
}

// validateV2Manifest enforces the schema invariants that JSON
// unmarshaling can't express: kind discriminator matches per-package
// shape, cell field is consistent, etc.
func validateV2Manifest(m *BundleManifest) error {
	if m.SnapshotID == "" {
		return errors.New("manifest.snapshot_id is empty")
	}
	if len(m.Packages) == 0 {
		return errors.New("manifest.packages is empty")
	}
	switch m.Kind {
	case BundleKindSource:
		if m.Cell != "" {
			return errors.New("manifest.cell must be empty for source bundles")
		}
		for i, p := range m.Packages {
			if p.Source == nil {
				return fmt.Errorf("packages[%d] (%s): source is required for source bundles", i, p.Name)
			}
			if len(p.Binaries) > 0 {
				return fmt.Errorf("packages[%d] (%s): binaries[] is not allowed in a source bundle", i, p.Name)
			}
		}
	case BundleKindBinary:
		if m.Cell == "" {
			return errors.New("manifest.cell is required for binary bundles")
		}
		for i, p := range m.Packages {
			if p.Source != nil {
				return fmt.Errorf("packages[%d] (%s): source is not allowed in a binary bundle", i, p.Name)
			}
			if len(p.Binaries) == 0 {
				return fmt.Errorf("packages[%d] (%s): binaries[] is required for binary bundles", i, p.Name)
			}
			for j, b := range p.Binaries {
				if b.Cell != m.Cell {
					return fmt.Errorf("packages[%d].binaries[%d] (%s): cell %q does not match bundle cell %q",
						i, j, p.Name, b.Cell, m.Cell)
				}
			}
		}
	default:
		return fmt.Errorf("manifest.kind %q is not recognized (expected %q or %q)",
			m.Kind, BundleKindSource, BundleKindBinary)
	}
	return nil
}

// countBlobs is the total number of files we'll sha256-verify. Used
// for the progress message; doesn't affect import semantics.
func countBlobs(m *BundleManifest) int {
	n := 0
	for _, p := range m.Packages {
		if p.Source != nil {
			n++
		}
		n += len(p.Binaries)
	}
	return n
}

// preflightSha256 walks every blob (source or binary) and confirms
// the on-disk file's sha256 matches the manifest. Returns the first
// mismatch (or I/O error) wrapped with the offending package name.
func preflightSha256(root string, m *BundleManifest) error {
	for _, p := range m.Packages {
		if p.Source != nil {
			if err := verifyBlob(root, p.Name, p.Source); err != nil {
				return err
			}
		}
		for _, b := range p.Binaries {
			if err := verifyBlob(root, p.Name, &b.BundleManifestBlob); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyBlob(root, name string, blob *BundleManifestBlob) error {
	full, err := safeJoin(root, blob.Path)
	if err != nil {
		return fmt.Errorf("preflight %s: %w", name, err)
	}
	got, err := sha256File(full)
	if err != nil {
		return fmt.Errorf("preflight %s: %w", name, err)
	}
	if got != strings.ToLower(blob.Sha256) {
		return fmt.Errorf("preflight %s: sha256 mismatch (manifest=%s file=%s)",
			name, blob.Sha256, got)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from the operator-supplied bundle they trust
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// importOne dispatches on the bundle's Kind. Source bundles route to
// api.ImportSource (existing behavior). Binary bundles route to
// api.AttachBinaries — one call per binary in the package's binaries
// list (single-cell per bundle in v1, but the loop is correct if a
// future bundle carries more).
func (b *BundleImporter) importOne(ctx context.Context, root string, m *BundleManifest, p BundleManifestPackage) (*api.PublishResponse, error) {
	switch m.Kind {
	case BundleKindSource:
		return b.importSource(ctx, root, m, p)
	case BundleKindBinary:
		return b.importBinaries(ctx, root, m, p)
	default:
		return nil, fmt.Errorf("unsupported bundle kind %q", m.Kind)
	}
}

func (b *BundleImporter) importSource(ctx context.Context, root string, m *BundleManifest, p BundleManifestPackage) (*api.PublishResponse, error) {
	full, err := safeJoin(root, p.Source.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full) //nolint:gosec // safeJoin enforces containment under the bundle root
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return api.ImportSource(ctx, b.Deps, api.ImportInput{
		Channel: b.Channel,
		Name:    p.Name,
		Version: p.Version,
		Source:  f,
		Actor:   b.Actor,
		Note:    fmt.Sprintf("bundle %s (%s)", p.Source.Path, m.SnapshotID),
	})
}

// importBinaries attaches every binary listed for the package and
// summarizes the result. AlreadyExisted sticks only when ALL binaries
// were already present; if even one was newly attached, the package
// counts as "imported" rather than "skipped".
func (b *BundleImporter) importBinaries(ctx context.Context, root string, m *BundleManifest, p BundleManifestPackage) (*api.PublishResponse, error) {
	if len(p.Binaries) == 0 {
		return nil, errors.New("no binaries listed for package")
	}

	combined := &api.PublishResponse{
		Channel:        b.Channel,
		Name:           p.Name,
		Version:        p.Version,
		AlreadyExisted: true, // flipped to false below as soon as any binary is new
	}
	for _, bin := range p.Binaries {
		full, err := safeJoin(root, bin.Path)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(full) //nolint:gosec // safeJoin enforces containment under the bundle root
		if err != nil {
			return nil, err
		}
		resp, err := api.AttachBinaries(ctx, b.Deps, api.AttachInput{
			Channel: b.Channel,
			Name:    p.Name,
			Version: p.Version,
			Cell:    bin.Cell,
			Binary:  f,
			Actor:   b.Actor,
			Note:    fmt.Sprintf("bundle %s (%s)", bin.Path, m.SnapshotID),
		})
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if !resp.AlreadyExisted {
			combined.AlreadyExisted = false
		}
		if resp.Overwritten {
			combined.Overwritten = true
		}
		combined.SourceSHA256 = resp.SourceSHA256
		combined.SourceSize = resp.SourceSize
		combined.Binaries = append(combined.Binaries, resp.Binaries...)
	}
	return combined, nil
}

// safeJoin joins root and rel and rejects results that escape root.
// Bundle manifests are operator-supplied JSON — treat path values as
// untrusted to keep "../../../etc/shadow" out of the importer.
func safeJoin(root, rel string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(rel))
	cleaned := filepath.Clean(joined)
	rootClean := filepath.Clean(root) + string(filepath.Separator)
	if !strings.HasPrefix(cleaned+string(filepath.Separator), rootClean) {
		return "", fmt.Errorf("manifest entry path %q escapes bundle root", rel)
	}
	return cleaned, nil
}

// extractTarGz reads src (a .tar.gz / .tgz archive) and writes its
// contents under dst. Refuses entries that would escape dst, refuses
// hard- or symlink entries entirely, refuses absolute names. Caps
// per-entry size at 16 GiB so a maliciously crafted archive can't
// fill the disk silently.
func extractTarGz(src, dst string) error {
	const maxEntryBytes int64 = 16 << 30 // 16 GiB

	f, err := os.Open(src) //nolint:gosec // src is the operator-supplied bundle path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("tar entry %q is absolute", hdr.Name)
		}

		out, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // bundle file, not secret
			if err != nil {
				return err
			}
			//nolint:gosec // bounded by maxEntryBytes; non-malicious archives sit well under this
			n, err := io.CopyN(w, tr, maxEntryBytes+1)
			_ = w.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if n > maxEntryBytes {
				return fmt.Errorf("tar entry %q exceeds %d bytes", hdr.Name, maxEntryBytes)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("tar entry %q is a link; not supported in bundles", hdr.Name)
		default:
			// Skip anything we don't recognize (PAX headers, etc.) silently.
			continue
		}
	}
}
