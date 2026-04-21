package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// handleBinaryPackages serves GET /{channel}/bin/linux/{cell}/PACKAGES.
// Only rows that have a binary for the cell appear.
func handleBinaryPackages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveBinaryPackages(w, r, deps, r.PathValue("channel"), r.PathValue("cell"), false)
	}
}

// handleBinaryPackagesGz is the gzipped variant for clients that ask
// for .gz first.
func handleBinaryPackagesGz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveBinaryPackages(w, r, deps, r.PathValue("channel"), r.PathValue("cell"), true)
	}
}

// handleBinaryTarball serves GET /{channel}/bin/linux/{cell}/{file}.
// File shape matches source tarballs: <Package>_<Version>.tar.gz.
// Linux R binaries follow the PPM convention of using tar.gz files
// that unpack as already-built packages — filenames are the same as
// source to keep URL patterns predictable.
func handleBinaryTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveBinaryTarball(w, r, deps, r.PathValue("channel"), r.PathValue("cell"), r.PathValue("file"))
	}
}

// handleDefaultBinaryPackages / ...Gz / ...Tarball serve the alias
// routes under /bin/linux/{cell}/... — no channel in the URL.
func handleDefaultBinaryPackages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveBinaryPackages(w, r, deps, ch, r.PathValue("cell"), false)
	}
}

func handleDefaultBinaryPackagesGz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveBinaryPackages(w, r, deps, ch, r.PathValue("cell"), true)
	}
}

func handleDefaultBinaryTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
		if herr != nil {
			herr.write(w, r)
			return
		}
		serveBinaryTarball(w, r, deps, ch, r.PathValue("cell"), r.PathValue("file"))
	}
}

func serveBinaryPackages(w http.ResponseWriter, r *http.Request, deps Deps, channel, cell string, gzipped bool) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	body, herr := loadBinaryPackages(r.Context(), deps, channel, cell)
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

func serveBinaryTarball(w http.ResponseWriter, r *http.Request, deps Deps, channel, cell, file string) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	name, version, ok := parseSourceTarballFilename(file)
	if !ok {
		writeError(w, r, http.StatusNotFound,
			CodeNotFound, "unknown resource",
			"binary tarballs are named <Package>_<Version>.tar.gz")
		return
	}
	sum, size, herr := lookupBinaryBlob(r.Context(), deps.DB.DB, channel, name, version, cell)
	if herr != nil {
		herr.write(w, r)
		return
	}
	serveBlob(w, r, deps, sum, size, "application/x-gzip")
}

// lookupBinaryBlob fetches the binary sha256/size for a (channel, name,
// version, cell) tuple. The JOIN against packages scopes to channel
// and version; binaries.cell pins to the requested cell.
func lookupBinaryBlob(ctx context.Context, db *sql.DB, channel, name, version, cell string) (sum string, size int64, herr *httpError) {
	err := db.QueryRowContext(ctx, `
		SELECT b.binary_sha256, b.size
		FROM binaries b
		JOIN packages p ON p.id = b.package_id
		WHERE p.channel = ? AND p.name = ? AND p.version = ? AND b.cell = ?
	`, channel, name, version, cell).Scan(&sum, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("%s@%s has no binary for cell %s on channel %s", name, version, cell, channel),
		}
	}
	if err != nil {
		return "", 0, internalErr("binary lookup", err)
	}
	return sum, size, nil
}

// loadBinaryPackages wraps Index.GetBinary with 404s for unknown
// channel and unknown cell. Looking up the cell in matrix.yaml lets
// us surface a targeted error before doing a DB read.
func loadBinaryPackages(ctx context.Context, deps Deps, channel, cell string) ([]byte, *httpError) {
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
	if deps.Matrix == nil || deps.Matrix.Lookup(cell) == nil {
		return nil, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("cell %q is not configured", cell),
			hint:   "add the cell to matrix.yaml and restart the server",
		}
	}
	rMinor := deps.Matrix.Lookup(cell).RMinor
	body, err := deps.Index.GetBinary(ctx, channel, cell, rMinor)
	if err != nil {
		return nil, internalErr("build binary packages", err)
	}
	return body, nil
}
