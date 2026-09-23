package backup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitea.cynkra.com/david.schoch/packyard/internal/backup"
	"gitea.cynkra.com/david.schoch/packyard/internal/cas"
	"gitea.cynkra.com/david.schoch/packyard/internal/db"
)

type dataDir struct {
	dir string
	db  *db.DB
	cas *cas.Store
}

func newDataDir(t *testing.T) *dataDir {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.MigrateEmbedded(ctx, d); err != nil {
		t.Fatal(err)
	}
	store, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "channels.yaml"), []byte("channels: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &dataDir{dir: dir, db: d, cas: store}
}

// publish stores a source (and optionally a binary) blob and the rows
// referencing them.
func (d *dataDir) publish(t *testing.T, name, src, bin string) {
	t.Helper()
	ctx := context.Background()
	sum, size, err := d.cas.Write(bytes.NewReader([]byte(src)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO channels(name, overwrite_policy, is_default) VALUES ('dev', 'mutable', 1)`); err != nil {
		t.Fatal(err)
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size)
		VALUES ('dev', ?, '1.0.0', ?, ?)`, name, sum, size)
	if err != nil {
		t.Fatal(err)
	}
	if bin == "" {
		return
	}
	id, _ := res.LastInsertId()
	bsum, bsize, err := d.cas.Write(bytes.NewReader([]byte(bin)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO binaries(package_id, cell, binary_sha256, size) VALUES (?, 'r-4.4', ?, ?)`,
		id, bsum, bsize); err != nil {
		t.Fatal(err)
	}
}

func (d *dataDir) source() backup.Source {
	return backup.Source{
		DB:      d.db.DB,
		CASRoot: filepath.Join(d.dir, "cas"),
		ConfigFiles: map[string]string{
			"channels.yaml": filepath.Join(d.dir, "channels.yaml"),
			"matrix.yaml":   filepath.Join(d.dir, "matrix.yaml"), // absent: skipped
		},
		Version: "test",
	}
}

func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newDataDir(t)
	src.publish(t, "alpha", "alpha source", "alpha binary")
	src.publish(t, "beta", "beta source", "")

	out := filepath.Join(t.TempDir(), "backup")
	res, err := backup.Backup(ctx, src.source(), out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Blobs != 3 || res.BlobsAdded != 3 || res.Manifest.SchemaVersion < 4 {
		t.Errorf("first backup = %+v", res)
	}
	if len(res.Manifest.ConfigFiles) != 1 || res.Manifest.ConfigFiles[0] != "channels.yaml" {
		t.Errorf("config files = %v", res.Manifest.ConfigFiles)
	}

	// Incremental: a second backup into the same dir copies only new blobs.
	src.publish(t, "gamma", "gamma source", "")
	res, err = backup.Backup(ctx, src.source(), out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Blobs != 4 || res.BlobsAdded != 1 {
		t.Errorf("second backup = %+v", res)
	}

	rep, err := backup.Verify(ctx, out)
	if err != nil || !rep.OK() || rep.Checked != 4 {
		t.Fatalf("verify = %+v, %v", rep, err)
	}

	target := t.TempDir()
	rr, err := backup.Restore(ctx, out, backup.Target{
		DataDir:     target,
		ConfigFiles: map[string]string{"channels.yaml": filepath.Join(target, "channels.yaml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Blobs != 4 || len(rr.ConfigWritten) != 1 {
		t.Errorf("restore = %+v", rr)
	}

	restored, err := db.Open(ctx, filepath.Join(target, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	var n int
	if err := restored.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages`).Scan(&n); err != nil || n != 3 {
		t.Errorf("restored packages = %d, %v", n, err)
	}
	store, err := cas.New(filepath.Join(target, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	var sum string
	_ = restored.QueryRowContext(ctx, `SELECT binary_sha256 FROM binaries`).Scan(&sum)
	if !store.Has(sum) {
		t.Error("restored CAS lacks the binary blob")
	}

	// A second restore needs -force.
	if _, err := backup.Restore(ctx, out, backup.Target{DataDir: target}); !errors.Is(err, backup.ErrDataDirNotEmpty) {
		t.Errorf("restore over existing data: err = %v", err)
	}
	if _, err := backup.Restore(ctx, out, backup.Target{DataDir: target, Force: true}); err != nil {
		t.Errorf("forced restore: %v", err)
	}
}

func TestRestoreSkipsConfigOutsideDataDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newDataDir(t)
	out := t.TempDir()
	if _, err := backup.Backup(ctx, src.source(), out); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	managed := filepath.Join(t.TempDir(), "channels.yaml")
	rr, err := backup.Restore(ctx, out, backup.Target{
		DataDir:     target,
		ConfigFiles: map[string]string{"channels.yaml": managed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.ConfigSkipped) != 1 || len(rr.ConfigWritten) != 0 {
		t.Errorf("restore = %+v, want channels.yaml skipped", rr)
	}
	if _, err := os.Stat(managed); err == nil {
		t.Error("restore wrote a config file outside the data dir")
	}
}

func TestVerifyDetectsDamage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newDataDir(t)
	src.publish(t, "alpha", "alpha source", "alpha binary")
	out := t.TempDir()
	if _, err := backup.Backup(ctx, src.source(), out); err != nil {
		t.Fatal(err)
	}

	var srcSum, binSum string
	_ = src.db.QueryRowContext(ctx, `SELECT source_sha256 FROM packages`).Scan(&srcSum)
	_ = src.db.QueryRowContext(ctx, `SELECT binary_sha256 FROM binaries`).Scan(&binSum)

	// Corrupt one blob (break the hardlink first so the source stays
	// intact) and delete the other.
	corrupt := filepath.Join(out, "cas", srcSum[:2], srcSum[2:])
	if err := os.Remove(corrupt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(out, "cas", binSum[:2], binSum[2:])); err != nil {
		t.Fatal(err)
	}

	rep, err := backup.Verify(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || len(rep.Corrupt) != 1 || len(rep.Missing) != 1 {
		t.Errorf("verify = %+v, want one corrupt and one missing", rep)
	}
	if _, err := backup.Restore(ctx, out, backup.Target{DataDir: t.TempDir()}); err == nil {
		t.Error("restore of a damaged backup succeeded")
	}
	if _, err := backup.Verify(ctx, t.TempDir()); err == nil {
		t.Error("verify of a non-backup dir succeeded")
	}
}
