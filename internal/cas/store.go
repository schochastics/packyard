// Package cas is a content-addressable blob store backed by the local
// filesystem. Blobs are keyed by the lowercase hex SHA-256 of their bytes
// and live at "<root>/<aa>/<rest>", where <aa> is the first two hex chars
// of the hash. The two-char shard keeps directory sizes manageable even
// for installations with 100k+ blobs.
//
// The store is deliberately minimal: no reference counting, no
// expiration, no compression. Deletion is handled separately by GC
// (see gc.go) which is invoked from an admin command, not during normal
// publish traffic.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// tmpDir is the subdirectory under the store root used for partial
// writes. Blobs land here first and are atomically renamed into place
// once hashing completes. The name is reserved — GC ignores it when
// walking.
const tmpDir = "tmp"

// sha256Hex matches a valid lowercase 64-char hex SHA-256 digest. Inputs
// to Read/Has/Path are validated against this so a malformed key can
// never escape the root via "../" style tricks.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store is a content-addressable blob store rooted at a local directory.
type Store struct {
	root string
}

// New prepares a store at root, creating it and its tmp/ subdirectory
// if either is missing. Passing an existing non-empty directory is
// fine — New never removes data.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("cas: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("cas: resolve root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, tmpDir), 0o755); err != nil {
		return nil, fmt.Errorf("cas: ensure tmp dir: %w", err)
	}
	return &Store{root: abs}, nil
}

// Root returns the absolute path to the store root. Exposed for admin
// tooling and tests; callers should not write under it directly.
func (s *Store) Root() string { return s.root }

// Write streams r into the store, returning the SHA-256 (lowercase hex)
// and byte count of the content. The file lands under a tmp/ dir first
// and is then atomically renamed to its final path, so concurrent
// readers never observe a partial blob. If a blob with the same hash
// already exists the temp file is discarded and the existing blob is
// left untouched — this makes re-publishing the same source a no-op
// at the storage layer.
//
// Write does not read the whole body into memory; large tarballs stream
// through the SHA-256 hasher at disk speed.
func (s *Store) Write(r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "blob-*")
	if err != nil {
		return "", 0, fmt.Errorf("cas: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any error path below we remove tmpPath; on success the rename
	// below moves it out of the way and this removal becomes a no-op.
	defer func() { _ = os.Remove(tmpPath) }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, fmt.Errorf("cas: stream blob: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	finalPath := s.pathFor(sum)

	// Cheap fast path: another concurrent writer (or an earlier publish)
	// already has this content. Leave that file alone; os.Remove on the
	// temp file will run via defer.
	if _, err := os.Stat(finalPath); err == nil {
		return sum, n, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", 0, fmt.Errorf("cas: stat dest: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return "", 0, fmt.Errorf("cas: ensure shard dir: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// If a competitor landed the same blob between our stat and
		// rename, the destination exists now and that's equivalent to
		// us winning. Anything else is a real failure.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			return sum, n, nil
		}
		return "", 0, fmt.Errorf("cas: rename into store: %w", err)
	}
	return sum, n, nil
}

// Read opens the blob identified by sum for reading. The caller owns
// the returned ReadCloser and must Close it. Returns os.ErrNotExist
// when the blob is absent so callers can use errors.Is.
func (s *Store) Read(sum string) (io.ReadCloser, error) {
	if !sha256Hex.MatchString(sum) {
		return nil, fmt.Errorf("cas: invalid sha256 %q", sum)
	}
	return os.Open(s.pathFor(sum))
}

// Has reports whether a blob with the given sum is present. An
// invalid sum returns false (rather than an error) so callers can use
// Has as a cheap idempotency check without wrapping in if/err.
func (s *Store) Has(sum string) bool {
	if !sha256Hex.MatchString(sum) {
		return false
	}
	_, err := os.Stat(s.pathFor(sum))
	return err == nil
}

// Path returns the absolute filesystem path where a blob with the
// given sum would (or does) live. Returns an error on a malformed sum.
// Does not check that the file actually exists — use Has for that.
func (s *Store) Path(sum string) (string, error) {
	if !sha256Hex.MatchString(sum) {
		return "", fmt.Errorf("cas: invalid sha256 %q", sum)
	}
	return s.pathFor(sum), nil
}

// pathFor assumes sum is already validated.
func (s *Store) pathFor(sum string) string {
	return filepath.Join(s.root, sum[:2], sum[2:])
}
