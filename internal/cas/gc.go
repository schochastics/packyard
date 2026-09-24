package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// GCReport summarizes the work done by a single garbage-collection pass.
type GCReport struct {
	Scanned      int   // total blob files walked
	Removed      int   // blob files deleted (or, on a dry run, that would be)
	FreedBytes   int64 // bytes reclaimed (or reclaimable on a dry run)
	SkippedYoung int   // unreferenced blobs kept because they are newer than MinAge
	SkippedStray int   // files that don't look like valid blobs (left alone)
	TmpRemoved   int   // abandoned temp files older than MinAge removed from tmp/
}

// GCOptions tunes a GC pass.
type GCOptions struct {
	// MinAge protects unreferenced blobs and temp files whose mtime is
	// newer than now-MinAge. A publish writes its blobs before the DB
	// row that references them commits, and Write refreshes the mtime
	// of a blob it reuses, so a grace period longer than the slowest
	// upload keeps GC from deleting blobs a concurrent publish is about
	// to reference. Zero disables the protection (tests, stopped server).
	MinAge time.Duration
	// DryRun reports what would be removed without deleting anything.
	DryRun bool
	// OnRemove, when set, is called for each blob that is (or on a dry
	// run would be) removed.
	OnRemove func(sum string, size int64)
	// Now overrides the clock; zero means time.Now().
	Now time.Time
}

// GC removes every blob under the store root whose lowercase-hex SHA-256
// is not in liveSet and that is older than opts.MinAge. The caller
// produces liveSet from the authoritative source (the DB's
// source_sha256 + binary_sha256 columns).
//
// Files in tmp/ are in-flight or abandoned writes. Those older than
// MinAge are abandoned (a crash or kill mid-upload) and are removed;
// with MinAge zero tmp/ is left alone, since any file there could
// belong to a writer that is about to rename it.
//
// Files that don't look like valid blobs (wrong name length,
// non-hex chars, files living directly under root rather than under a
// 2-char shard) are not deleted — they are almost certainly the
// operator's own probes or backups, and removing them silently would
// be a nasty surprise. They're counted in SkippedStray for visibility.
//
// A per-file error during removal is returned immediately; partial
// progress up to that point is reflected in the report.
func (s *Store) GC(liveSet map[string]struct{}, opts GCOptions) (GCReport, error) {
	var report GCReport
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.MinAge)
	tmpRoot := filepath.Join(s.root, tmpDir)

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", path, err)
		}
		if d.IsDir() {
			if path == tmpRoot {
				if opts.MinAge > 0 {
					if err := s.sweepTmp(cutoff, opts.DryRun, &report); err != nil {
						return err
					}
				}
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("rel path for %q: %w", path, err)
		}

		sum, ok := blobSumFromRel(rel)
		if !ok {
			report.SkippedStray++
			return nil
		}
		report.Scanned++

		if _, live := liveSet[sum]; live {
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			return fmt.Errorf("stat %q: %w", path, statErr)
		}
		if opts.MinAge > 0 && info.ModTime().After(cutoff) {
			report.SkippedYoung++
			return nil
		}
		size := info.Size()

		if !opts.DryRun {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove %q: %w", path, err)
			}
		}
		if opts.OnRemove != nil {
			opts.OnRemove(sum, size)
		}
		report.Removed++
		report.FreedBytes += size
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

// sweepTmp removes temp files in tmp/ last modified before cutoff.
func (s *Store) sweepTmp(cutoff time.Time, dryRun bool, report *GCReport) error {
	entries, err := os.ReadDir(filepath.Join(s.root, tmpDir))
	if err != nil {
		return fmt.Errorf("read tmp dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // renamed into place since ReadDir
			}
			return fmt.Errorf("stat tmp %q: %w", e.Name(), err)
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(s.root, tmpDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove tmp %q: %w", e.Name(), err)
			}
		}
		report.TmpRemoved++
	}
	return nil
}

// blobSumFromRel reverses the <aa>/<rest> layout into the full 64-char
// SHA-256 hex, returning ok=false for anything that doesn't match so the
// caller can treat it as a stray file.
func blobSumFromRel(rel string) (string, bool) {
	dir, file := filepath.Split(rel)
	if dir == "" || file == "" {
		return "", false
	}
	// dir includes the trailing separator from filepath.Split; strip it.
	shard := filepath.Clean(dir)
	if len(shard) != 2 || !isHex(shard) {
		return "", false
	}
	if len(file) != 62 || !isHex(file) {
		return "", false
	}
	return shard + file, true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
