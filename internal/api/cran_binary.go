package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/upstream"
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
		if herr.status == http.StatusNotFound {
			if meta := lookupChannelMeta(r.Context(), deps, channel); meta.IsProxy() {
				if herr2 := proxyFetchBinaryTarball(r.Context(), deps, meta, name, version, cell); herr2 != nil {
					herr2.write(w, r)
					return
				}
				sum, size, herr = lookupBinaryBlob(r.Context(), deps.DB.DB, channel, name, version, cell)
			}
		}
		if herr != nil {
			herr.write(w, r)
			return
		}
	}
	serveBlob(w, r, deps, sum, size, "application/x-gzip")
}

// proxyFetchBinaryTarball materializes one (channel, name, version,
// cell) binary on a proxy channel. If the upstream binary URL for the
// cell isn't configured, returns 404 directly. Otherwise:
//
//  1. Ensure the source row exists locally (proxy-fetch the source
//     tarball if not — every binary attaches to a source row).
//  2. Fetch the binary tarball from the cell's upstream base URL.
//  3. AttachBinary to the existing source row + emit a
//     "proxy_tarball_fetch" event.
func proxyFetchBinaryTarball(ctx context.Context, deps Deps, meta *channelMeta, name, version, cell string) *httpError {
	if deps.Upstream == nil {
		return internalErr("proxy fetch", errNoUpstreamFetcher)
	}
	binBase, ok := meta.Upstream.BinaryURLs[cell]
	if !ok || binBase == "" {
		return &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("proxy channel %q has no upstream configured for cell %s", meta.Name, cell),
			hint:   "add the cell to channels.yaml under upstream.binary_urls, or fall back to source compile",
		}
	}

	// Source-row precondition: AttachBinary refuses without a source
	// row. Fetch source on demand if missing.
	var srcExists bool
	if err := deps.DB.QueryRowContext(ctx,
		`SELECT 1 FROM packages WHERE channel = ? AND name = ? AND version = ?`,
		meta.Name, name, version).Scan(new(int)); err == nil {
		srcExists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return internalErr("source row lookup", err)
	}
	if !srcExists {
		if herr := proxyFetchSourceTarball(ctx, deps, meta, name, version); herr != nil {
			return herr
		}
	}

	filename := fmt.Sprintf("%s_%s.tar.gz", name, version)
	blob, err := deps.Upstream.FetchTarball(ctx, binBase, filename,
		meta.Upstream.TarballMaxSize, meta.Upstream.Timeout)
	if err != nil {
		if upstream.NotFound(err) {
			return &httpError{
				status: http.StatusNotFound,
				code:   CodeNotFound,
				msg:    fmt.Sprintf("%s@%s binary not available upstream for cell %s on channel %s", name, version, cell, meta.Name),
			}
		}
		if deps.Metrics != nil {
			deps.Metrics.ProxyFetchTotal.WithLabelValues(meta.Name, "binary", "upstream_error").Inc()
		}
		return &httpError{
			status: http.StatusBadGateway,
			code:   CodeUnavailable,
			msg:    "upstream binary tarball fetch failed",
			hint:   err.Error(),
		}
	}
	if _, err := deps.Store.AttachBinary(ctx, store.AttachInput{
		Channel: meta.Name,
		Name:    name,
		Version: version,
		Policy:  meta.Policy,
		Cell:    cell,
		Binary:  blob,
		Actor:   "proxy:" + binBase,
	}); err != nil {
		return internalErr("attach proxy binary", err)
	}
	if deps.Metrics != nil {
		deps.Metrics.ProxyFetchTotal.WithLabelValues(meta.Name, "binary", "ok").Inc()
	}
	_, _ = deps.DB.ExecContext(ctx, `
		INSERT INTO events(type, channel, package, version, note)
		VALUES ('proxy_tarball_fetch', ?, ?, ?, ?)
	`, meta.Name, name, version, fmt.Sprintf("cell=%s upstream=%s", cell, binBase))
	if deps.Index != nil {
		deps.Index.InvalidateChannel(meta.Name)
	}
	return nil
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
	meta := lookupChannelMeta(ctx, deps, channel)
	body, stale, err := deps.Index.GetBinary(ctx, channel, cell, rMinor, meta, deps.Upstream)
	if err != nil {
		if meta.IsProxy() {
			return nil, &httpError{
				status: http.StatusServiceUnavailable,
				code:   CodeUnavailable,
				msg:    "upstream binary PACKAGES fetch failed",
				hint:   err.Error(),
			}
		}
		return nil, internalErr("build binary packages", err)
	}
	if stale {
		_, _ = deps.DB.ExecContext(ctx, `
			INSERT INTO events(type, channel, note)
			VALUES ('proxy_index_stale_served', ?, 'binary PACKAGES upstream unreachable')
		`, channel)
		if deps.Metrics != nil {
			deps.Metrics.ProxyFetchTotal.WithLabelValues(channel, "index", "stale").Inc()
		}
	} else if meta.IsProxy() && deps.Metrics != nil {
		deps.Metrics.ProxyFetchTotal.WithLabelValues(channel, "index", "ok").Inc()
	}
	return body, nil
}
