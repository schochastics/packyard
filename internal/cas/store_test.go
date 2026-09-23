package cas_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/schochastics/packyard/internal/cas"
)

func newStore(t *testing.T) *cas.Store {
	t.Helper()
	s, err := cas.New(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	return s
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	content := []byte("hello packyard")
	wantSum := sha256.Sum256(content)
	wantHex := hex.EncodeToString(wantSum[:])

	gotSum, gotSize, err := s.Write(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if gotSum != wantHex {
		t.Errorf("Write sum = %s, want %s", gotSum, wantHex)
	}
	if gotSize != int64(len(content)) {
		t.Errorf("Write size = %d, want %d", gotSize, len(content))
	}
	if !s.Has(gotSum) {
		t.Error("Has returned false after Write")
	}

	rc, err := s.Read(gotSum)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("read content differs from written content")
	}
}

func TestWriteIsIdempotent(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	content := []byte("identical bytes")

	sum1, _, err := s.Write(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	sum2, _, err := s.Write(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if sum1 != sum2 {
		t.Errorf("idempotent write returned different sums: %s vs %s", sum1, sum2)
	}

	path, err := s.Path(sum1)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	// Only one copy on disk (plus whatever else is in the shard dir).
	shardDir := filepath.Dir(path)
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatalf("ReadDir shard: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("shard dir has %d entries, want 1", len(entries))
	}

	// And no stale temp files survive.
	tmpEntries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(tmpEntries) != 0 {
		t.Errorf("tmp dir has %d stragglers: %v", len(tmpEntries), tmpEntries)
	}
}

func TestConcurrentWritesOfSameContent(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	content := make([]byte, 64*1024) // 64 KiB; enough to make a real copy
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	const workers = 8
	sums := make([]string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			sum, _, err := s.Write(bytes.NewReader(content))
			if err != nil {
				t.Errorf("concurrent Write: %v", err)
				return
			}
			sums[i] = sum
		}(i)
	}
	wg.Wait()

	for i, s := range sums {
		if s != sums[0] {
			t.Errorf("worker %d returned %s, want %s", i, s, sums[0])
		}
	}

	// Verify the single stored blob matches what was written.
	rc, err := s.Read(sums[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("concurrent writes produced corrupted blob")
	}
}

func TestReadRejectsInvalidSum(t *testing.T) {
	t.Parallel()

	s := newStore(t)

	cases := []string{
		"",                      // empty
		"abc",                   // too short
		strings.Repeat("z", 64), // non-hex
		strings.Repeat("A", 64), // uppercase — we normalize on write, so uppercase input is bogus
		"../../../etc/passwd",   // path traversal
		strings.Repeat("0", 65), // too long
	}
	for _, tc := range cases {
		if _, err := s.Read(tc); err == nil {
			t.Errorf("Read(%q) succeeded, want error", tc)
		}
		if s.Has(tc) {
			t.Errorf("Has(%q) = true, want false", tc)
		}
		if _, err := s.Path(tc); err == nil {
			t.Errorf("Path(%q) succeeded, want error", tc)
		}
	}
}

func TestReadMissingReturnsNotExist(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	sum := strings.Repeat("0", 64)

	_, err := s.Read(sum)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read of missing blob returned %v, want os.ErrNotExist", err)
	}
	if s.Has(sum) {
		t.Error("Has returned true for absent blob")
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := cas.New(""); err == nil {
		t.Error("New(\"\") succeeded; expected error")
	}
}
