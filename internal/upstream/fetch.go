// Package upstream is the outbound-HTTP side of packyard's lazy proxy
// channels. It speaks CRAN-protocol against an upstream
// (cloud.r-project.org, Posit Package Manager, r-universe, ...) so the
// rest of packyard can treat an upstream as just another source of
// tarballs to drop into CAS.
//
// The package itself is stateless aside from a [singleflight.Group]
// that collapses concurrent identical requests; TTL caching of
// PACKAGES bodies happens one layer up, in the api Index.
//
// Layering: this package depends on [internal/store] (for CAS writes
// and BlobRef) but not on [internal/api]. It is called *from*
// [internal/api] on cache misses; the api package never imports
// upstream's read-path callers, so the cycle is broken.
//
// See design.md §15 (Lazy proxy channels) for the feature design and
// the role of each piece.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/version"
)

// ErrTooLarge is returned when an upstream response exceeds the
// configured per-tarball cap. Callers translate to a 502/400-style
// HTTP response and emit a metric.
var ErrTooLarge = errors.New("upstream response exceeds configured tarball_max_size")

// ErrUpstreamStatus wraps a non-2xx upstream response with the URL
// and status code. Callers usually map this to 502 or, for explicit
// upstream 404s, to a packyard 404.
type ErrUpstreamStatus struct {
	URL    string
	Status int
}

// Error reports the status. URL is already redacted (no userinfo).
func (e *ErrUpstreamStatus) Error() string {
	return fmt.Sprintf("upstream %s returned HTTP %d", e.URL, e.Status)
}

// NotFound reports whether the wrapped error is a 404 from upstream.
// Handy in handlers that want to translate "upstream doesn't have
// this package" into a packyard 404 rather than a generic 502.
func NotFound(err error) bool {
	var u *ErrUpstreamStatus
	return errors.As(err, &u) && u.Status == http.StatusNotFound
}

// Fetcher is the upstream-HTTP client. Construct with [New].
type Fetcher struct {
	client   *http.Client
	store    *store.Service
	single   *singleflight.Group
	maxIndex int64 // bytes; cap on PACKAGES bodies (tarballs use a per-call cap)
}

// DefaultUserAgent is sent when the caller has no R User-Agent to
// forward (source indexes and tarballs).
var DefaultUserAgent = "packyard/" + version.Version

// New builds a Fetcher. The provided http.Client is used as-is; pass
// one with a generous timeout (upstream PACKAGES files for full-CRAN
// snapshots are a few MB and downloads of a single tarball can take
// seconds on a cold cache). Defaulting to [http.DefaultClient] is not
// recommended in production because it has no timeout at all.
func New(client *http.Client, svc *store.Service) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Fetcher{
		client:   client,
		store:    svc,
		single:   &singleflight.Group{},
		maxIndex: 64 << 20, // 64 MiB cap on PACKAGES bodies — well above the largest CRAN snapshot
	}
}

// shared runs fn once for every concurrent caller with the same key.
// fn gets a context detached from the first caller's cancellation (the
// per-request timeout still bounds it), so one client disconnecting
// doesn't fail every request that joined the flight; each caller stops
// waiting when its own context ends.
func (f *Fetcher) shared(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	detached := context.WithoutCancel(ctx)
	ch := f.single.DoChan(key, func() (any, error) { return fn(detached) })
	select {
	case res := <-ch:
		return res.Val, res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// flightKey includes the User-Agent: upstreams such as Posit Package
// Manager answer the same URL with a different binary per R version.
func flightKey(kind, target, userAgent string) string {
	return kind + "\x00" + target + "\x00" + userAgent
}

// FetchIndex retrieves baseURL/src/contrib/PACKAGES. userAgent is
// sent as-is ("" means [DefaultUserAgent]); for a binary index it
// should be an R User-Agent naming the cell's R version. Concurrent
// calls with the same URL and User-Agent collapse via singleflight, so
// a thundering herd of cache-miss reads only generates one upstream
// request.
func (f *Fetcher) FetchIndex(ctx context.Context, baseURL, userAgent string, timeout time.Duration) ([]byte, error) {
	target, err := joinURL(baseURL, "src", "contrib", "PACKAGES")
	if err != nil {
		return nil, err
	}
	v, err := f.shared(ctx, flightKey("index", target, userAgent), func(ctx context.Context) (any, error) {
		return f.getBounded(ctx, target, userAgent, f.maxIndex, timeout)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// FetchTarball streams baseURL/src/contrib/<path...> into CAS and
// returns the resulting BlobRef. path is one or more segments, e.g.
// {"foo_1.0.tar.gz"} or {"Archive", "foo", "foo_1.0.tar.gz"}; no
// segment may contain a slash. The HTTP response is capped at maxSize
// before the CAS write so a hostile upstream can't bomb the tempfile
// area.
//
// Concurrent calls with the same URL and User-Agent collapse via
// singleflight. Idempotency falls out naturally: the CAS dedups on
// hash, and if a duplicate fetch races past singleflight the CAS
// still writes once.
func (f *Fetcher) FetchTarball(ctx context.Context, baseURL, userAgent string, path []string, maxSize int64, timeout time.Duration) (store.BlobRef, error) {
	if len(path) == 0 {
		return store.BlobRef{}, errors.New("upstream: empty tarball path")
	}
	for _, seg := range path {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/\\") {
			return store.BlobRef{}, fmt.Errorf("upstream: tarball path segment %q is not a plain name", seg)
		}
	}
	target, err := joinURL(baseURL, append([]string{"src", "contrib"}, path...)...)
	if err != nil {
		return store.BlobRef{}, err
	}

	v, err := f.shared(ctx, flightKey("tarball", target, userAgent), func(ctx context.Context) (any, error) {
		return f.streamToCAS(ctx, target, userAgent, maxSize, timeout)
	})
	if err != nil {
		return store.BlobRef{}, err
	}
	return v.(store.BlobRef), nil
}

// get issues a GET for target with the User-Agent set. Error strings
// carry the redacted URL, so upstream credentials in userinfo never
// reach logs or client-facing hints.
func (f *Fetcher) get(ctx context.Context, target, userAgent string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", redact(target), err)
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", redact(target), err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &ErrUpstreamStatus{URL: redact(target), Status: resp.StatusCode}
	}
	return resp, nil
}

// redact strips userinfo from a URL for display.
func redact(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "<unparseable upstream URL>"
	}
	return u.Redacted()
}

// getBounded GETs target into a fully-buffered byte slice, refusing
// responses larger than maxBytes. The full-buffer pattern is intended
// for PACKAGES (always small); tarballs go through streamToCAS.
func (f *Fetcher) getBounded(ctx context.Context, target, userAgent string, maxBytes int64, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	resp, err := f.get(reqCtx, target, userAgent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body of %s: %w", redact(target), err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w: %s exceeded %d bytes", ErrTooLarge, redact(target), maxBytes)
	}
	return body, nil
}

// streamToCAS GETs target and streams the response body through the
// CAS writer, refusing responses larger than maxBytes via
// [io.LimitReader]. The CAS-side stat-and-rename takes care of
// deduplication if the same bytes were already cached.
func (f *Fetcher) streamToCAS(ctx context.Context, target, userAgent string, maxBytes int64, timeout time.Duration) (store.BlobRef, error) {
	reqCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	resp, err := f.get(reqCtx, target, userAgent)
	if err != nil {
		return store.BlobRef{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Reject obvious-oversize responses by Content-Length before
	// spending a CAS tempfile on them. The LimitReader below catches
	// the case where Content-Length lied or wasn't provided.
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return store.BlobRef{}, fmt.Errorf("%w: %s Content-Length %d > %d",
			ErrTooLarge, redact(target), resp.ContentLength, maxBytes)
	}

	limited := &boundedReader{r: io.LimitReader(resp.Body, maxBytes+1), max: maxBytes}
	blob, err := f.store.WriteBlob(limited)
	if err != nil {
		return store.BlobRef{}, fmt.Errorf("write blob from %s: %w", redact(target), err)
	}
	if limited.tripped {
		// The CAS already wrote a tempfile-and-rename for the
		// partial body. That blob is now orphaned in CAS; periodic GC
		// reclaims it. We refuse the row rather than ingest unsafe bytes.
		return store.BlobRef{}, fmt.Errorf("%w: %s exceeded %d bytes mid-stream", ErrTooLarge, redact(target), maxBytes)
	}
	return blob, nil
}

// boundedReader wraps a LimitReader and notes when the boundary was
// hit, so the caller can distinguish "upstream is at the limit" from
// "upstream finished right at the limit." We read max+1 from the
// upstream; tripping the +1 means the body was strictly larger.
type boundedReader struct {
	r       io.Reader
	max     int64
	read    int64
	tripped bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.max {
		b.tripped = true
	}
	return n, err
}

// joinURL combines a base URL with one or more path segments. Each
// segment is URL-escaped so a malformed package name from upstream
// can't sneak path traversal back into the request URL.
func joinURL(base string, segments ...string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse upstream base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base URL %q is not absolute", u.Redacted())
	}
	parts := []string{strings.TrimRight(u.Path, "/")}
	for _, seg := range segments {
		parts = append(parts, url.PathEscape(seg))
	}
	u.Path = strings.Join(parts, "/")
	return u.String(), nil
}

// withTimeout wraps ctx with timeout if timeout > 0. When timeout is
// zero or negative, the caller's deadline is the only bound.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
