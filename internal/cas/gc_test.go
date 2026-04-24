package cas_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/schochastics/packyard/internal/cas"
)

func seedBlob(t *testing.T, s *cas.Store, data []byte) (sum string, size int64) {
	t.Helper()
	sum, size, err := s.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return sum, size
}

func TestGCRemovesOnlyDeadBlobs(t *testing.T) {
	t.Parallel()

	s := newStore(t)

	live1, size1 := seedBlob(t, s, []byte("live one"))
	live2, size2 := seedBlob(t, s, []byte("live two"))
	dead, sizeDead := seedBlob(t, s, []byte("will be collected"))

	liveSet := map[string]struct{}{live1: {}, live2: {}}

	report, err := s.GC(liveSet)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if report.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", report.Scanned)
	}
	if report.Removed != 1 {
		t.Errorf("Removed = %d, want 1", report.Removed)
	}
	if report.FreedBytes != sizeDead {
		t.Errorf("FreedBytes = %d, want %d", report.FreedBytes, sizeDead)
	}

	if !s.Has(live1) || !s.Has(live2) {
		t.Error("live blobs missing after GC")
	}
	if s.Has(dead) {
		t.Error("dead blob survived GC")
	}

	_ = size1
	_ = size2
}

func TestGCEmptyLiveSetWipesAllBlobs(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, _ = seedBlob(t, s, []byte("a"))
	_, _ = seedBlob(t, s, []byte("b"))
	_, _ = seedBlob(t, s, []byte("c"))

	report, err := s.GC(map[string]struct{}{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if report.Removed != 3 {
		t.Errorf("Removed = %d, want 3", report.Removed)
	}
}

func TestGCLeavesTmpDirAlone(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	live, _ := seedBlob(t, s, []byte("keep me"))

	// Drop a stray file under tmp/ — GC must not touch it.
	stray := filepath.Join(s.Root(), "tmp", "blob-halfwritten.part")
	if err := os.WriteFile(stray, []byte("half-written"), 0o600); err != nil {
		t.Fatalf("create stray: %v", err)
	}

	_, err := s.GC(map[string]struct{}{live: {}})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if _, err := os.Stat(stray); err != nil {
		t.Errorf("GC removed a file under tmp/: %v", err)
	}
}

func TestGCIgnoresStrayFilesOutsideShards(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	live, _ := seedBlob(t, s, []byte("keep me"))

	// Operator left a README at the root — not our business to delete.
	note := filepath.Join(s.Root(), "NOTES.txt")
	if err := os.WriteFile(note, []byte("admin probe"), 0o600); err != nil {
		t.Fatalf("create stray: %v", err)
	}

	// And a file inside a dir that isn't a 2-char hex shard.
	bogusDir := filepath.Join(s.Root(), "not-a-shard")
	if err := os.MkdirAll(bogusDir, 0o755); err != nil {
		t.Fatalf("mkdir bogus: %v", err)
	}
	bogusFile := filepath.Join(bogusDir, "something.bin")
	if err := os.WriteFile(bogusFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("create bogus: %v", err)
	}

	report, err := s.GC(map[string]struct{}{live: {}})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if report.Removed != 0 {
		t.Errorf("Removed = %d, want 0", report.Removed)
	}
	if report.SkippedStray < 2 {
		t.Errorf("SkippedStray = %d, want >= 2", report.SkippedStray)
	}

	if _, err := os.Stat(note); err != nil {
		t.Errorf("NOTES.txt was removed: %v", err)
	}
	if _, err := os.Stat(bogusFile); err != nil {
		t.Errorf("non-shard file was removed: %v", err)
	}
	if !s.Has(live) {
		t.Error("live blob was removed")
	}
}
