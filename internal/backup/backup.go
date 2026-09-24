// Package backup snapshots a packyard data directory, verifies a
// snapshot, and restores one.
//
// A backup directory looks like
//
//	<dir>/db.sqlite          VACUUM INTO snapshot of the live DB
//	<dir>/cas/<aa>/<rest>    every blob the snapshot references
//	<dir>/config/*.yaml      server.yaml, channels.yaml, matrix.yaml
//	<dir>/manifest.json      what was written, by which version
//
// Blobs are content-addressed and immutable, so backing up into the
// same directory again only adds new blobs, and a directory synced
// with `rsync --link-dest` stays incremental.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for snapshot reads
)

// Manifest is written to <dir>/manifest.json.
type Manifest struct {
	CreatedAt       string   `json:"created_at"`
	PackyardVersion string   `json:"packyard_version"`
	SchemaVersion   int      `json:"schema_version"`
	Blobs           int      `json:"blobs"`
	BlobBytes       int64    `json:"blob_bytes"`
	ConfigFiles     []string `json:"config_files"`
}

// Source describes the data directory being backed up.
type Source struct {
	DB      *sql.DB // the live database
	CASRoot string  // <data>/cas
	// ConfigFiles maps a name in <dir>/config/ to its current path.
	// Missing files are skipped.
	ConfigFiles map[string]string
	Version     string // packyard version for the manifest
}

// Result reports what Backup did.
type Result struct {
	Manifest   Manifest
	BlobsAdded int // blobs not already present in the backup dir
}

// Backup snapshots src into dir, creating it if needed. Safe to run
// against a live server: the DB snapshot is transactionally
// consistent, and the blob set is read from the snapshot, not the
// live DB.
func Backup(ctx context.Context, src Source, dir string) (Result, error) {
	var res Result
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return res, err
	}

	// VACUUM INTO refuses to overwrite, so snapshot beside the old
	// one. The snapshot only replaces the previous db.sqlite once every
	// blob it references is in place: a backup that fails midway (disk
	// full, missing blob) leaves the previous backup usable.
	dbPath := filepath.Join(dir, "db.sqlite")
	tmp := fmt.Sprintf("%s.tmp-%d", dbPath, time.Now().UnixNano())
	defer func() { _ = os.Remove(tmp) }()
	if _, err := src.DB.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return res, fmt.Errorf("snapshot db: %w", err)
	}

	snap, err := openReadOnly(tmp)
	if err != nil {
		return res, err
	}
	defer func() { _ = snap.Close() }()

	sums, err := referencedBlobs(ctx, snap)
	if err != nil {
		return res, err
	}
	m := Manifest{
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		PackyardVersion: src.Version,
		Blobs:           len(sums),
	}
	if m.SchemaVersion, err = schemaVersion(ctx, snap); err != nil {
		return res, err
	}

	for _, sum := range sums {
		from := blobPath(src.CASRoot, sum)
		to := blobPath(filepath.Join(dir, "cas"), sum)
		fi, err := os.Stat(from)
		if err != nil {
			return res, fmt.Errorf("blob %s referenced by the DB is missing from CAS: %w", sum, err)
		}
		m.BlobBytes += fi.Size()
		if _, err := os.Stat(to); err == nil {
			continue
		}
		if err := linkOrCopy(from, to); err != nil {
			return res, fmt.Errorf("copy blob %s: %w", sum, err)
		}
		res.BlobsAdded++
	}
	if err := snap.Close(); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return res, err
	}

	names := make([]string, 0, len(src.ConfigFiles))
	for name := range src.ConfigFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := src.ConfigFiles[name]
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := copyFile(path, filepath.Join(dir, "config", name), 0o640); err != nil {
			return res, fmt.Errorf("copy %s: %w", name, err)
		}
		m.ConfigFiles = append(m.ConfigFiles, name)
	}

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return res, err
	}
	if err := writeAtomic(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o640); err != nil {
		return res, err
	}
	res.Manifest = m
	return res, nil
}

// VerifyReport lists what Verify found wrong. It is clean when every
// slice is empty.
type VerifyReport struct {
	Manifest  Manifest
	Checked   int      // blob files re-hashed
	Missing   []string // referenced by db.sqlite, absent from cas/
	Corrupt   []string // content doesn't match the file name
	Integrity string   // PRAGMA integrity_check result ("ok" when fine)
}

// OK reports whether the backup is intact.
func (r VerifyReport) OK() bool {
	return r.Integrity == "ok" && len(r.Missing) == 0 && len(r.Corrupt) == 0
}

// Verify checks a backup directory: the manifest parses, the DB passes
// PRAGMA integrity_check, every blob it references is present, and
// every blob file hashes to its name.
func Verify(ctx context.Context, dir string) (VerifyReport, error) {
	var rep VerifyReport
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return rep, fmt.Errorf("not a packyard backup: %w", err)
	}
	if err := json.Unmarshal(b, &rep.Manifest); err != nil {
		return rep, fmt.Errorf("manifest.json: %w", err)
	}

	snap, err := openReadOnly(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		return rep, err
	}
	defer func() { _ = snap.Close() }()
	if err := snap.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&rep.Integrity); err != nil {
		return rep, fmt.Errorf("integrity_check: %w", err)
	}

	sums, err := referencedBlobs(ctx, snap)
	if err != nil {
		return rep, err
	}
	casDir := filepath.Join(dir, "cas")
	for _, sum := range sums {
		if _, err := os.Stat(blobPath(casDir, sum)); err != nil {
			rep.Missing = append(rep.Missing, sum)
		}
	}

	err = walkBlobs(casDir, func(path, rel string) error {
		want := strings.ReplaceAll(filepath.ToSlash(rel), "/", "")
		got, err := hashFile(path)
		if err != nil {
			return err
		}
		rep.Checked++
		if got != want {
			rep.Corrupt = append(rep.Corrupt, rel)
		}
		return nil
	})
	return rep, err
}

// Target describes where Restore writes.
type Target struct {
	DataDir string
	// ConfigFiles maps a name in <backup>/config/ to where it goes.
	// Only entries whose destination is inside DataDir are written;
	// the rest belong to config management and are reported instead.
	ConfigFiles map[string]string
	Force       bool // replace an existing DB and CAS in DataDir
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Blobs         int
	ConfigWritten []string
	ConfigSkipped []string // present in the backup, destination managed elsewhere
}

// ErrDataDirNotEmpty is returned when the target already holds a
// database or blobs and Force is unset.
var ErrDataDirNotEmpty = errors.New("data directory already holds a packyard database; pass -force to replace it")

// Restore verifies the backup in from and writes it into t.DataDir.
// The server must not be running against t.DataDir.
func Restore(ctx context.Context, from string, t Target) (RestoreResult, error) {
	var res RestoreResult
	rep, err := Verify(ctx, from)
	if err != nil {
		return res, err
	}
	if !rep.OK() {
		return res, fmt.Errorf("backup failed verification (integrity=%s, missing=%d, corrupt=%d); refusing to restore",
			rep.Integrity, len(rep.Missing), len(rep.Corrupt))
	}

	dbPath := filepath.Join(t.DataDir, "db.sqlite")
	casDir := filepath.Join(t.DataDir, "cas")
	occupied := exists(dbPath) || hasBlobs(casDir)
	if occupied && !t.Force {
		return res, ErrDataDirNotEmpty
	}
	if err := os.MkdirAll(t.DataDir, 0o750); err != nil {
		return res, err
	}

	// Stage the whole copy inside the data dir (same filesystem, so the
	// final moves are renames). Until the swap below, a failure leaves
	// the existing data untouched.
	stage, err := os.MkdirTemp(t.DataDir, ".restore-")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	stagedDB := filepath.Join(stage, "db.sqlite")
	stagedCAS := filepath.Join(stage, "cas")
	if err := copyFile(filepath.Join(from, "db.sqlite"), stagedDB, 0o640); err != nil {
		return res, fmt.Errorf("restore db: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stagedCAS, "tmp"), 0o750); err != nil {
		return res, err
	}
	fromCAS := filepath.Join(from, "cas")
	err = walkBlobs(fromCAS, func(path, rel string) error {
		res.Blobs++
		return linkOrCopy(path, filepath.Join(stagedCAS, rel))
	})
	if err != nil {
		return res, fmt.Errorf("restore blobs: %w", err)
	}

	// Swap: move the old data aside, the blobs in, and the DB last, so
	// an interrupted swap never leaves a DB pointing at missing blobs.
	if occupied {
		old := filepath.Join(stage, "replaced")
		if err := os.Mkdir(old, 0o750); err != nil {
			return res, err
		}
		for _, name := range []string{"db.sqlite", "db.sqlite-wal", "db.sqlite-shm", "cas"} {
			p := filepath.Join(t.DataDir, name)
			if err := os.Rename(p, filepath.Join(old, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, fmt.Errorf("move old %s aside: %w", name, err)
			}
		}
	} else if err := os.RemoveAll(casDir); err != nil {
		// An empty or tmp-only cas/ from a bootstrap.
		return res, err
	}
	if err := os.Rename(stagedCAS, casDir); err != nil {
		return res, fmt.Errorf("move blobs into place: %w", err)
	}
	if err := os.Rename(stagedDB, dbPath); err != nil {
		return res, fmt.Errorf("move db into place: %w", err)
	}

	for _, name := range rep.Manifest.ConfigFiles {
		dst, ok := t.ConfigFiles[name]
		if !ok || !inside(t.DataDir, dst) {
			res.ConfigSkipped = append(res.ConfigSkipped, name)
			continue
		}
		if exists(dst) && !t.Force {
			res.ConfigSkipped = append(res.ConfigSkipped, name)
			continue
		}
		if err := copyFile(filepath.Join(from, "config", name), dst, 0o640); err != nil {
			return res, fmt.Errorf("restore %s: %w", name, err)
		}
		res.ConfigWritten = append(res.ConfigWritten, name)
	}
	return res, nil
}

// walkBlobs calls fn for every file under casDir except tmp/ and dot
// files (temp files a killed copy left behind). A missing casDir is an
// empty store.
func walkBlobs(casDir string, fn func(path, rel string) error) error {
	return filepath.WalkDir(casDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == casDir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "tmp" && path != casDir {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(casDir, path)
		if err != nil {
			return err
		}
		return fn(path, rel)
	})
}

func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return d, nil
}

func referencedBlobs(ctx context.Context, d *sql.DB) ([]string, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT source_sha256 FROM packages
		UNION SELECT binary_sha256 FROM binaries
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list referenced blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func schemaVersion(ctx context.Context, d *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := d.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("schema version: %w", err)
	}
	return int(v.Int64), nil
}

// blobPath mirrors the CAS layout: <root>/<aa>/<rest>.
func blobPath(root, sum string) string {
	if len(sum) < 3 {
		return filepath.Join(root, sum)
	}
	return filepath.Join(root, sum[:2], sum[2:])
}

// linkOrCopy hardlinks from to to, falling back to a copy across
// filesystems. Blobs are never modified in place, so sharing an inode
// between the data dir and a backup is safe.
func linkOrCopy(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return err
	}
	if err := os.Link(from, to); err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	return copyFile(from, to, 0o440)
}

func copyFile(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(to), ".restore-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), to)
}

func writeAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
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

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hasBlobs reports whether a CAS directory holds any blob (cas/tmp
// leftovers don't count).
func hasBlobs(casDir string) bool {
	entries, err := os.ReadDir(casDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != "tmp" {
			if sub, _ := os.ReadDir(filepath.Join(casDir, e.Name())); len(sub) > 0 {
				return true
			}
		}
	}
	return false
}

func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
