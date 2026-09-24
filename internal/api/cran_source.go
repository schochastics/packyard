package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/upstream"
)

// errNoUpstreamFetcher fires when a proxy-channel handler runs without
// an upstream.Fetcher wired up — operator misconfiguration that
// shouldn't happen in production but is easy to hit in tests.
var errNoUpstreamFetcher = errors.New("no upstream fetcher configured on Deps")

// The source surface. Each handler serves both the channel-named
// route (/{channel}/src/contrib/…) and the default-channel alias
// (/src/contrib/…); withChannel resolves which.
//
//	GET /{channel}/src/contrib/PACKAGES[.gz]
//	GET /{channel}/src/contrib/{file}
//	GET /{channel}/src/contrib/Archive/{pkg}/{file}
//	GET /{channel}/src/contrib/Meta/archive.rds

func handleSourcePackages(deps Deps, gzipped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveSourcePackages(w, r, deps, channel, gzipped)
		})
	}
}

func handleSourceTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveSourceTarball(w, r, deps, channel, "", r.PathValue("file"))
		})
	}
}

func handleSourceArchiveTarball(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			serveSourceTarball(w, r, deps, channel, r.PathValue("pkg"), r.PathValue("file"))
		})
	}
}

// handleArchiveRDS serves src/contrib/Meta/archive.rds on both the
// source and the /__linux__/ paths; the listing is the same for both.
func handleArchiveRDS(deps Deps, linux bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withChannel(w, r, deps, func(channel string) {
			if linux {
				if _, ok := resolveLinuxCell(w, r, deps); !ok {
					return
				}
			}
			if !requireReadScope(w, r, deps, channel) {
				return
			}
			if herr := requireChannel(r.Context(), deps, channel); herr != nil {
				herr.write(w, r)
				return
			}
			if lookupChannelMeta(r.Context(), deps, channel).IsProxy() {
				writeError(w, r, http.StatusNotFound, CodeNotFound,
					"archive.rds is not served for proxy channels", "")
				return
			}
			body, err := deps.Index.GetArchive(r.Context(), channel)
			if err != nil {
				internalErr("build archive.rds", err).write(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		})
	}
}

// withChannel calls fn with the request's channel: the {channel} path
// segment when the route has one, else the default channel.
func withChannel(w http.ResponseWriter, r *http.Request, deps Deps, fn func(channel string)) {
	if ch := r.PathValue("channel"); ch != "" {
		fn(ch)
		return
	}
	ch, herr := resolveDefaultChannel(r.Context(), deps.DB.DB)
	if herr != nil {
		herr.write(w, r)
		return
	}
	fn(ch)
}

// serveSourcePackages serves the source PACKAGES index.
func serveSourcePackages(w http.ResponseWriter, r *http.Request, deps Deps, channel string, gzipped bool) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	body, herr := loadSourcePackages(r.Context(), deps, channel)
	if herr != nil {
		herr.write(w, r)
		return
	}
	writeIndexBody(w, r, body, gzipped)
}

// writeIndexBody writes a PACKAGES body, gzipped for PACKAGES.gz.
func writeIndexBody(w http.ResponseWriter, r *http.Request, body []byte, gzipped bool) {
	if gzipped {
		gz, err := gzipIndexBody(body)
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

// serveSourceTarball serves a source tarball. archivePkg is the {pkg}
// segment of an Archive/ URL, "" for a plain src/contrib/{file}.
// Both paths serve any stored version, current, archived or yanked:
// clients differ in which one they try for a pinned version, so only
// the indexes distinguish current from archived.
func serveSourceTarball(w http.ResponseWriter, r *http.Request, deps Deps, channel, archivePkg, file string) {
	if !requireReadScope(w, r, deps, channel) {
		return
	}
	name, version, ok := parseTarballPath(archivePkg, file)
	if !ok {
		writeTarballNameError(w, r)
		return
	}
	serveSourceVersion(w, r, deps, channel, name, version)
}

// serveSourceVersion streams the source tarball of name@version,
// fetching it from upstream first on a proxy channel miss. Callers
// have already checked read scope.
func serveSourceVersion(w http.ResponseWriter, r *http.Request, deps Deps, channel, name, version string) {
	sum, size, herr := lookupSourceBlob(r.Context(), deps.DB.DB, channel, name, version)
	if herr != nil && herr.status == http.StatusNotFound {
		// Proxy channels translate the local miss into an upstream
		// fetch. On success we re-query and serve from CAS; on failure
		// the upstream error replaces the local 404.
		if meta := lookupChannelMeta(r.Context(), deps, channel); meta.IsProxy() {
			if herr2 := proxyFetchSourceTarball(r.Context(), deps, meta, name, version); herr2 != nil {
				herr2.write(w, r)
				return
			}
			sum, size, herr = lookupSourceBlob(r.Context(), deps.DB.DB, channel, name, version)
		}
	}
	if herr != nil {
		herr.write(w, r)
		return
	}
	serveBlob(w, r, deps, sum, size, "application/x-gzip")
}

// parseTarballPath parses {file} and, for Archive/{pkg}/{file} URLs,
// checks that {pkg} names the same package.
func parseTarballPath(archivePkg, file string) (name, version string, ok bool) {
	name, version, ok = parseSourceTarballFilename(file)
	if !ok || (archivePkg != "" && archivePkg != name) {
		return "", "", false
	}
	return name, version, true
}

func writeTarballNameError(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, CodeNotFound, "unknown resource",
		"tarballs are named <Package>_<Version>.tar.gz (under Archive/<Package>/ for archived versions)")
}

// requireChannel returns a 404 error when channel doesn't exist.
func requireChannel(ctx context.Context, deps Deps, channel string) *httpError {
	ok, err := channelExists(ctx, deps.DB.DB, channel)
	if err != nil {
		return internalErr("channel lookup", err)
	}
	if !ok {
		return &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("channel %q not found", channel),
		}
	}
	return nil
}

// noteProxyIndexRead records proxy index metrics and, when a stale
// index was served because upstream was unreachable, an audit event.
func noteProxyIndexRead(ctx context.Context, deps Deps, channel string, meta *channelMeta, stale bool) {
	if !meta.IsProxy() {
		return
	}
	if stale {
		// Best-effort audit annotation; ignoring errors here keeps the
		// happy path simple and the event row purely advisory.
		_, _ = deps.DB.ExecContext(ctx, `
			INSERT INTO events(type, channel, note)
			VALUES ('proxy_index_stale_served', ?, 'PACKAGES upstream unreachable')
		`, channel)
	}
	if deps.Metrics == nil {
		return
	}
	outcome := "ok"
	if stale {
		outcome = "stale"
	}
	deps.Metrics.ProxyFetchTotal.WithLabelValues(channel, "index", outcome).Inc()
}

// proxyFetchSourceTarball fetches <name>_<version>.tar.gz from the
// proxy channel's upstream, writes it into CAS, and materializes a
// source-only package row plus an audit event. Subsequent reads hit
// the local CAS and skip this path entirely.
func proxyFetchSourceTarball(ctx context.Context, deps Deps, meta *channelMeta, name, version string) *httpError {
	if deps.Upstream == nil {
		return internalErr("proxy fetch", errNoUpstreamFetcher)
	}
	filename := fmt.Sprintf("%s_%s.tar.gz", name, version)
	blob, err := deps.Upstream.FetchTarball(ctx, meta.Upstream.SourceURL, filename,
		meta.Upstream.TarballMaxSize, meta.Upstream.Timeout)
	if err != nil {
		if upstream.NotFound(err) {
			return &httpError{
				status: http.StatusNotFound,
				code:   CodeNotFound,
				msg:    fmt.Sprintf("%s@%s not available upstream of channel %s", name, version, meta.Name),
			}
		}
		if deps.Metrics != nil {
			deps.Metrics.ProxyFetchTotal.WithLabelValues(meta.Name, "source", "upstream_error").Inc()
		}
		return &httpError{
			status: http.StatusBadGateway,
			code:   CodeUnavailable,
			msg:    "upstream tarball fetch failed",
			hint:   err.Error(),
		}
	}
	if _, err := deps.Store.Materialize(ctx, store.Input{
		Channel: meta.Name,
		Name:    name,
		Version: version,
		Policy:  meta.Policy,
		Source:  blob,
		Actor:   "proxy:" + meta.Upstream.SourceURL,
	}); err != nil {
		return internalErr("materialize proxy tarball", err)
	}
	if deps.Metrics != nil {
		deps.Metrics.ProxyFetchTotal.WithLabelValues(meta.Name, "source", "ok").Inc()
	}
	// Audit event. Best-effort: a failure here doesn't undo the row.
	_, _ = deps.DB.ExecContext(ctx, `
		INSERT INTO events(type, channel, package, version, note)
		VALUES ('proxy_tarball_fetch', ?, ?, ?, ?)
	`, meta.Name, name, version, "upstream="+meta.Upstream.SourceURL)
	if deps.Index != nil {
		deps.Index.InvalidateChannel(meta.Name)
	}
	return nil
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
	if !packageNameRE.MatchString(name) || !validVersion(version) {
		return "", "", false
	}
	return name, version, true
}

// lookupSourceBlob returns the source_sha256 and source_size for a
// published (channel, name, version). Yanked rows are still served —
// a lockfile pinned to a yanked version must still resolve; yanking
// only removes a version from PACKAGES. Missing rows return 404.
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
	if herr := requireChannel(ctx, deps, channel); herr != nil {
		return nil, herr
	}
	meta := lookupChannelMeta(ctx, deps, channel)
	body, stale, err := deps.Index.GetSource(ctx, channel, meta, deps.Upstream)
	if err != nil {
		// Proxy channel where upstream failed and no stale cache was
		// available: 503 so clients distinguish "we tried and
		// upstream is broken" from "channel doesn't exist".
		if meta.IsProxy() {
			return nil, &httpError{
				status: http.StatusServiceUnavailable,
				code:   CodeUnavailable,
				msg:    "upstream PACKAGES fetch failed",
				hint:   err.Error(),
			}
		}
		return nil, internalErr("build packages", err)
	}
	noteProxyIndexRead(ctx, deps, channel, meta, stale)
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

// requireReadScope is requireScope plus the per-channel anonymous
// read exception: a request passes when it holds read:<channel>, or
// when channels.yaml sets anonymous_reads on the channel. Otherwise
// requireScope writes the standard 401/403 envelope.
func requireReadScope(w http.ResponseWriter, r *http.Request, deps Deps, channel string) bool {
	id, authenticated := IdentityFromContext(r.Context())
	if authenticated && id.Scopes.Has("read:"+channel) {
		return true
	}
	if deps.Channels != nil {
		if ch := deps.Channels.Lookup(channel); ch != nil && ch.AnonymousReads {
			return true
		}
	}
	return requireScope(w, r, "read:"+channel)
}

// gzipBytes is a one-shot compressor. The inputs are small (a few KB
// to a few MB of PACKAGES text), so the whole-in-memory approach is
// fine and simpler than streaming.
// gzCache memoizes gzipped index bodies by content hash. R asks for
// PACKAGES.gz on every install, and a proxy channel's CRAN index is
// several MB: hashing it is far cheaper than recompressing it. The
// cache holds only the few bodies currently being served.
var gzCache = struct {
	sync.Mutex
	m map[[sha256.Size]byte][]byte
}{m: map[[sha256.Size]byte][]byte{}}

const gzCacheMax = 64

func gzipIndexBody(body []byte) ([]byte, error) {
	key := sha256.Sum256(body)
	gzCache.Lock()
	gz, ok := gzCache.m[key]
	gzCache.Unlock()
	if ok {
		return gz, nil
	}
	gz, err := gzipBytes(body)
	if err != nil {
		return nil, err
	}
	gzCache.Lock()
	if len(gzCache.m) >= gzCacheMax {
		clear(gzCache.m) // crude, but bodies change rarely
	}
	gzCache.m[key] = gz
	gzCache.Unlock()
	return gz, nil
}

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
