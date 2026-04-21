package cas

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// GCReport summarizes the work done by a single garbage-collection pass.
type GCReport struct {
	Scanned      int   // total blob files walked
	Removed      int   // blob files deleted
	FreedBytes   int64 // bytes reclaimed
	SkippedStray int   // files that don't look like valid blobs (left alone)
}

// GC removes every blob under the store root whose lowercase-hex SHA-256
// is not in liveSet. The caller is responsible for producing liveSet
// from the authoritative source (the DB's source_sha256 + binary_sha256
// columns) and for ensuring no concurrent publishes are in flight — GC
// is an admin-invoked op, not a background task.
//
// The tmp/ directory is skipped entirely: temp files there are either
// in-flight writes or abandoned writes. Leave them for a separate
// cleanup pass (future) rather than risk removing a file a concurrent
// writer is about to rename.
//
// Files that don't look like valid blobs (wrong name length,
// non-hex chars, files living directly under root rather than under a
// 2-char shard) are not deleted — they are almost certainly the
// operator's own probes or backups, and removing them silently would
// be a nasty surprise. They're counted in SkippedStray for visibility.
//
// A per-file error during removal is returned immediately; partial
// progress up to that point is reflected in the report.
func (s *Store) GC(liveSet map[string]struct{}) (GCReport, error) {
	var report GCReport

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", path, err)
		}
		if d.IsDir() {
			// Skip tmp/ entirely. Anything under it is the store's
			// own in-flight working space.
			if path == filepath.Join(s.root, tmpDir) {
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
		size := info.Size()

		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %q: %w", path, err)
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
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}
