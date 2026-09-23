package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"gitea.cynkra.com/david.schoch/packyard/internal/config"
	"gitea.cynkra.com/david.schoch/packyard/internal/store"
	"gitea.cynkra.com/david.schoch/packyard/internal/upstream"
)

// Linux binaries are served the way Posit Package Manager serves
// them: at a src/contrib path under /__linux__/{distro}/{snapshot}/,
// because R on Linux only ever requests <repo>/src/contrib/. The
// client's R minor version comes from its User-Agent and picks the
// cell; packages without a binary for that cell are served as source.
//
//	GET /{channel}/__linux__/{distro}/latest/src/contrib/PACKAGES[.gz]
//	GET /{channel}/__linux__/{distro}/latest/src/contrib/{file}
//	GET /{channel}/__linux__/{distro}/latest/src/contrib/Archive/{pkg}/{file}
//	(+ the same without /{channel} for the default channel)

// rUserAgentRE matches the R version R puts in its User-Agent,
// "R (4.4.3 x86_64-pc-linux-gnu x86_64 linux-gnu)". renv, pak and
// Posit Connect's restores all download through R and send the same
// prefix.
var rUserAgentRE = regexp.MustCompile(`(?:^|[\s;])R \((\d+)\.(\d+)(?:\.\d+)?[\s)]`)

// rMinorFromUserAgent extracts "4.4" from an R User-Agent.
func rMinorFromUserAgent(ua string) (string, bool) {
	m := rUserAgentRE.FindStringSubmatch(ua)
	if m == nil {
		return "", false
	}
	return m[1] + "." + m[2], true
}

// resolveLinuxCell validates the {distro} and {snapshot} segments and
// returns the cell for the requesting client, or nil when its R
// version has no cell (serve source). It writes the error and returns
// ok=false when the URL doesn't match this deployment.
func resolveLinuxCell(w http.ResponseWriter, r *http.Request, deps Deps) (cell *config.Cell, ok bool) {
	// The response depends on the client's R version; shared caches
	// must not hand an R 4.4 binary to R 4.6.
	w.Header().Add("Vary", "User-Agent")

	m := deps.Matrix
	if m == nil {
		writeError(w, r, http.StatusNotFound, CodeNotFound,
			"no binary matrix configured", "add matrix.yaml and restart the server")
		return nil, false
	}
	if d := r.PathValue("distro"); d != m.Distro {
		writeError(w, r, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("distro %q not served; this repository serves %q", d, m.Distro),
			fmt.Sprintf("use /__linux__/%s/latest in the repository URL", m.Distro))
		return nil, false
	}
	if snap := r.PathValue("snapshot"); snap != "latest" {
		writeError(w, r, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("snapshot %q not available; snapshots are not supported", snap),
			"use latest")
		return nil, false
	}
	rMinor, found := rMinorFromUserAgent(r.UserAgent())
	if !found {
		rMinor = m.DefaultRMinor
	}
	return m.CellForRMinor(rMinor), true
}

func handleLinuxPackages(deps Deps, gzipped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveLinuxPackages(w, r, deps, channel, gzipped)
		})
	}
}

func handleLinuxTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveLinuxTarball(w, r, deps, channel, "", r.PathValue("file"))
		})
	}
}

func handleLinuxArchiveTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveLinuxTarball(w, r, deps, channel, r.PathValue("pkg"), r.PathValue("file"))
		})
	}
}

func serveLinuxPackages(w http.ResponseWriter, r *http.Request, deps Deps, channel string, gzipped bool) {
	cell, ok := resolveLinuxCell(w, r, deps)
	if !ok || !requireReadScope(w, r, deps, channel) {
		return
	}
	if herr := requireChannel(r.Context(), deps, channel); herr != nil {
		herr.write(w, r)
		return
	}
	meta := lookupChannelMeta(r.Context(), deps, channel)
	body, stale, err := deps.Index.GetLinux(r.Context(), channel, cell, deps.Matrix.Arch, meta, deps.Upstream)
	if err != nil {
		if meta.IsProxy() {
			writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable,
				"upstream PACKAGES fetch failed", err.Error())
			return
		}
		internalErr("build packages", err).write(w, r)
		return
	}
	noteProxyIndexRead(r.Context(), deps, channel, meta, stale)
	writeIndexBody(w, r, body, gzipped)
}

// serveLinuxTarball serves {file} for the client's cell: the binary
// when one exists for that version, otherwise the source tarball.
// archivePkg is the {pkg} segment of an Archive/ URL ("" otherwise).
func serveLinuxTarball(w http.ResponseWriter, r *http.Request, deps Deps, channel, archivePkg, file string) {
	cell, ok := resolveLinuxCell(w, r, deps)
	if !ok || !requireReadScope(w, r, deps, channel) {
		return
	}
	name, version, ok := parseTarballPath(archivePkg, file)
	if !ok {
		writeTarballNameError(w, r)
		return
	}
	if cell != nil {
		sum, size, herr := lookupBinaryBlob(r.Context(), deps.DB.DB, channel, name, version, cell.Name)
		if herr == nil {
			serveBlob(w, r, deps, sum, size, "application/x-gzip")
			return
		}
		if herr.status != http.StatusNotFound {
			herr.write(w, r)
			return
		}
		meta := lookupChannelMeta(r.Context(), deps, channel)
		if meta.IsProxy() && meta.Upstream.BinaryURLs[cell.Name] != "" {
			herr := proxyFetchBinaryTarball(r.Context(), deps, meta, name, version, cell.Name)
			if herr != nil && herr.status != http.StatusNotFound {
				herr.write(w, r)
				return
			}
			if herr == nil {
				if sum, size, herr := lookupBinaryBlob(r.Context(), deps.DB.DB, channel, name, version, cell.Name); herr == nil {
					serveBlob(w, r, deps, sum, size, "application/x-gzip")
					return
				}
			}
		}
	}
	serveSourceVersion(w, r, deps, channel, name, version)
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
