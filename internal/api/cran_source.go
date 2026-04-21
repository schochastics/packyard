package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// handleSourcePackages serves GET /{channel}/src/contrib/PACKAGES.
// Returns plain text; every access requires read:<channel> unless
// anonymous reads are enabled and {channel} is the default.
func handleSourcePackages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSourcePackages(w, r, deps, r.PathValue("channel"), false)
	}
}

// handleSourcePackagesGz serves the gzipped variant. Base R asks for
// .gz first on a CRAN-protocol install; we build gz from the same
// cached body so a mutation invalidates both views.
func handleSourcePackagesGz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSourcePackages(w, r, deps, r.PathValue("channel"), true)
	}
}

// handleSourceTarball serves GET /{channel}/src/contrib/{file}.
func handleSourceTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSourceTarball(w, r, deps, r.PathValue("channel"), r.PathValue("file"))
	}
}

// handleDefaultSourcePackages / ...Gz / ...Tarball serve the alias
// routes under /src/contrib/... — no channel in the URL. We resolve
// the default from the DB and delegate to the same core logic as the
// channel-named variants.
func handleDefaultSourcePackages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveSourcePackages(w, r, deps, ch, false)
	}
}

func handleDefaultSourcePackagesGz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveSourcePackages(w, r, deps, ch, true)
	}
}

func handleDefaultSourceTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveSourceTarball(w, r, deps, ch, r.PathValue("file"))
	}
}

// serveSourcePackages is the shared core of the channel-named and
// default-channel alias PACKAGES handlers. gzip=true switches response
// type and body to PACKAGES.gz.
func serveSourcePackages(w http.ResponseWriter, r *http.Request, deps Deps, channel string, gzipped bool) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	body, herr := loadSourcePackages(r.Context(), deps, channel)
	if herr != nil {
		herr.write(w, r)
		return
	}
	if gzipped {
		gz, err := gzipBytes(body)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError,
				CodeInternal, "gzip: "+err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
		_, _ = w.Write(gz)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// serveSourceTarball is the shared core of the channel-named and
// default-channel alias tarball handlers.
func serveSourceTarball(w http.ResponseWriter, r *http.Request, deps Deps, channel, file string) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	name, version, ok := parseSourceTarballFilename(file)
	if !ok {
		writeError(w, r, http.StatusNotFound,
			CodeNotFound, "unknown resource",
			"source tarballs are named <Package>_<Version>.tar.gz")
		return
	}
	sum, size, herr := lookupSourceBlob(r.Context(), deps.DB.DB, channel, name, version)
	if herr != nil {
		herr.write(w, r)
		return
	}
	serveBlob(w, r, deps, sum, size, "application/x-gzip")
}

// resolveDefaultChannel returns the name of the default channel, or a
// 500 httpError if the DB is in an impossible state (no row with
// is_default=1). Validation at config load time ensures exactly one
// default exists, so reaching the error path here means something
// tampered with the DB directly.
func resolveDefaultChannel(ctx context.Context, db *sql.DB) (string, *httpError) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM channels WHERE is_default = 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", &httpError{
			status: http.StatusInternalServerError,
			code:   CodeInternal,
			msg:    "no default channel configured",
			hint:   "set exactly one channel with default: true in channels.yaml and restart",
		}
	}
	if err != nil {
		return "", internalErr("default channel lookup", err)
	}
	return name, nil
}

// parseSourceTarballFilename extracts (name, version) from filenames
// of the form "pkg_1.2.3.tar.gz". Returns ok=false for anything else.
func parseSourceTarballFilename(file string) (name, version string, ok bool) {
	if !strings.HasSuffix(file, ".tar.gz") {
		return "", "", false
	}
	base := strings.TrimSuffix(file, ".tar.gz")
	i := strings.Index(base, "_")
	if i <= 0 || i == len(base)-1 {
		return "", "", false
	}
	name = base[:i]
	version = base[i+1:]
	if !packageNameRE.MatchString(name) || !versionRE.MatchString(version) {
		return "", "", false
	}
	return name, version, true
}

// lookupSourceBlob returns the source_sha256 and source_size for a
// published (channel, name, version). Yanked rows are still served —
// a lockfile pinned to a yanked version must still resolve, and the
// Yanked: yes field in PACKAGES is the signal tools use. Missing rows
// return 404.
func lookupSourceBlob(ctx context.Context, db *sql.DB, channel, name, version string) (sum string, size int64, herr *httpError) {
	err := db.QueryRowContext(ctx, `
		SELECT source_sha256, source_size
		FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, channel, name, version).Scan(&sum, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("%s@%s not found on channel %s", name, version, channel),
		}
	}
	if err != nil {
		return "", 0, internalErr("source lookup", err)
	}
	return sum, size, nil
}

// serveBlob streams a CAS blob into the response. The size comes from
// the DB (authoritative) rather than stat on the file, so a truncated
// blob on disk surfaces as a short response rather than a silent size
// mismatch.
func serveBlob(w http.ResponseWriter, r *http.Request, deps Deps, sum string, size int64, contentType string) {
	rc, err := deps.CAS.Read(sum)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// DB says the blob should exist but it doesn't — that's an
			// operator problem, not a client error.
			writeError(w, r, http.StatusInternalServerError,
				CodeInternal, "blob missing from CAS",
				"DB references a sha256 with no matching file; run admin gc to diagnose")
			return
		}
		writeError(w, r, http.StatusInternalServerError,
			CodeInternal, "cas read: "+err.Error(), "")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+sum+`"`)
	if _, err := io.Copy(w, rc); err != nil {
		// Client probably closed the connection mid-download. Not a
		// server error — just note it; the status was already written.
		_ = err
	}
}

// loadSourcePackages is a thin wrapper over Index.GetSource that
// converts "channel not found" into a 404.
func loadSourcePackages(ctx context.Context, deps Deps, channel string) ([]byte, *httpError) {
	ok, err := channelExists(ctx, deps.DB.DB, channel)
	if err != nil {
		return nil, internalErr("channel lookup", err)
	}
	if !ok {
		return nil, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("channel %q not found", channel),
		}
	}
	body, err := deps.Index.GetSource(ctx, channel)
	if err != nil {
		return nil, internalErr("build packages", err)
	}
	return body, nil
}

func channelExists(ctx context.Context, db *sql.DB, channel string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM channels WHERE name = ?`, channel).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// requireReadScope is requireScope plus the anonymous-default-channel
// exception. Flow:
//
//  1. Authenticated caller with read:<channel> wins immediately.
//  2. If cfg.AllowAnonymousReads AND the channel is the DB-marked
//     default, an unauthenticated request passes.
//  3. Otherwise fall back to requireScope which writes the standard
//     401/403 envelope.
func requireReadScope(w http.ResponseWriter, r *http.Request, deps Deps, channel string) bool {
	id, authenticated := IdentityFromContext(r.Context())
	if authenticated && id.Scopes.Has("read:"+channel) {
		return true
	}
	if deps.Server != nil && deps.Server.AllowAnonymousReads && isDefaultChannel(r.Context(), deps.DB.DB, channel) {
		return true
	}
	return requireScope(w, r, "read:"+channel)
}

// isDefaultChannel is a tiny DB lookup. Call sites are rare enough
// (one per read, and only on the anonymous path) that caching isn't
// worth the bookkeeping yet.
func isDefaultChannel(ctx context.Context, db *sql.DB, channel string) bool {
	var isDefault int
	err := db.QueryRowContext(ctx,
		`SELECT is_default FROM channels WHERE name = ?`, channel).Scan(&isDefault)
	return err == nil && isDefault == 1
}

// gzipBytes is a one-shot compressor. The inputs are small (a few KB
// to a few MB of PACKAGES text), so the whole-in-memory approach is
// fine and simpler than streaming.
func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
