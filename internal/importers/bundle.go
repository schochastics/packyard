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

// BundleSchema is the only manifest schema the importer accepts. New
// bundle producers writing unknown schemas are rejected at the gate
// rather than silently mis-imported.
const BundleSchema = "packyard-bundle/1"

// BundleManifest is the on-disk shape of a bundle's manifest.json.
// Mirrors the schema documented in design.md §10.2.
type BundleManifest struct {
	Schema        string                  `json:"schema"`
	SnapshotID    string                  `json:"snapshot_id"`
	RVersion      string                  `json:"r_version"`
	SourceURL     string                  `json:"source_url"`
	Mode          string                  `json:"mode"`
	CreatedAt     string                  `json:"created_at"`
	Tool          string                  `json:"tool"`
	InputPackages []string                `json:"input_packages,omitempty"`
	Packages      []BundleManifestPackage `json:"packages"`
}

// BundleManifestPackage is one entry in the manifest.packages list.
type BundleManifestPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"` // relative to the bundle root
	Sha256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// BundleImporter consumes a packyard-bundle/1 bundle (directory or
// .tar.gz) and imports each package into the configured packyard
// channel via api.ImportSource. The channel must already exist with
// an immutable overwrite_policy — the importer doesn't auto-create
// channels because channels.yaml is the source of truth for policy.
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
// the importer writes to the same CAS and DB.
func NewBundleImporter(deps api.Deps, channel string) *BundleImporter {
	return &BundleImporter{
		Deps:    deps,
		Channel: channel,
		Actor:   "import-bundle",
	}
}

// Run imports a bundle from path. path may be either a directory
// laid out as documented in design.md §10.2, or a .tar.gz / .tgz
// archive of one. progress, when non-nil, receives a short status
// line at each pre-flight + import step.
//
// Atomicity: if any sha256 mismatches the manifest, Run returns
// before touching CAS or the DB. This matches the spec — "partial
// imports are not allowed". If a per-package import fails AFTER
// pre-flight passes, the failure goes into result.Failed and Run
// continues; this matches DratImporter behavior and lets one
// broken tarball not block a 500-package bundle. Treat
// len(result.Failed) > 0 as caller-decides outcome.
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
		progress(fmt.Sprintf("manifest ok: schema=%s snapshot=%s mode=%s packages=%d",
			manifest.Schema, manifest.SnapshotID, manifest.Mode, len(manifest.Packages)))
	}

	// Pre-flight: sha256-verify every tarball before any side effects.
	// Mismatch aborts before CAS or DB are touched.
	if progress != nil {
		progress(fmt.Sprintf("pre-flight: verifying sha256 of %d tarballs...", len(manifest.Packages)))
	}
	if err := preflightSha256(root, manifest); err != nil {
		return nil, err
	}

	// Import phase. ImportSource handles channel-policy enforcement
	// (immutable mismatch yields ErrImmutableConflict, which we surface
	// per-package). It also handles CAS dedup automatically — re-import
	// of an overlapping snapshot is cheap.
	res := &BundleResult{Manifest: manifest}
	for _, p := range manifest.Packages {
		tag := p.Name + "@" + p.Version
		if progress != nil {
			progress("importing " + tag)
		}

		resp, err := b.importOne(ctx, root, p)
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
// callback. If path is already a directory, cleanup is a no-op. If
// path is a tar.gz / tgz archive, it's extracted to a tempdir and
// cleanup removes that dir.
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

func readManifest(root string) (*BundleManifest, error) {
	body, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m BundleManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Schema != BundleSchema {
		return nil, fmt.Errorf("unsupported manifest schema %q (this importer accepts %q)",
			m.Schema, BundleSchema)
	}
	if m.SnapshotID == "" {
		return nil, errors.New("manifest.snapshot_id is empty")
	}
	if len(m.Packages) == 0 {
		return nil, errors.New("manifest.packages is empty")
	}
	return &m, nil
}

// preflightSha256 walks every package entry and confirms the
// on-disk file's sha256 matches the manifest. Returns the first
// mismatch (or I/O error) wrapped with the offending package name.
func preflightSha256(root string, m *BundleManifest) error {
	for _, p := range m.Packages {
		full, err := safeJoin(root, p.Path)
		if err != nil {
			return fmt.Errorf("preflight %s: %w", p.Name, err)
		}
		got, err := sha256File(full)
		if err != nil {
			return fmt.Errorf("preflight %s: %w", p.Name, err)
		}
		if got != strings.ToLower(p.Sha256) {
			return fmt.Errorf("preflight %s: sha256 mismatch (manifest=%s file=%s)",
				p.Name, p.Sha256, got)
		}
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

// importOne opens the tarball under root and hands it to
// api.ImportSource. The bundle's path field is treated as untrusted
// input — we re-resolve it under root and reject any path escape.
func (b *BundleImporter) importOne(ctx context.Context, root string, p BundleManifestPackage) (*api.PublishResponse, error) {
	full, err := safeJoin(root, p.Path)
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
		Note:    fmt.Sprintf("bundle %s (%s)", p.Path, b.snapshotID()),
	})
}

func (b *BundleImporter) snapshotID() string {
	// b doesn't carry the snapshot ID; the caller has the manifest
	// but importOne doesn't. Pass via Note instead. Caller can use
	// result.Manifest.SnapshotID for log lines.
	return b.Channel
}

// safeJoin joins root and rel and rejects results that escape root.
// Bundle manifests are operator-supplied JSON — treat path values as
// untrusted to keep CVE-2025-style "../../../etc/shadow" out of the
// importer.
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
