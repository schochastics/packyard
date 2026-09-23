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
	client  *http.Client
	store   *store.Service
	single  *singleflight.Group
	maxIdle int64 // bytes; only consulted for PACKAGES (tarballs use per-call cap)
}

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
		client:  client,
		store:   svc,
		single:  &singleflight.Group{},
		maxIdle: 64 << 20, // 64 MiB cap on PACKAGES bodies — well above the largest CRAN snapshot
	}
}

// FetchIndex retrieves baseURL/src/contrib/PACKAGES, returning the
// body bytes plus the time the fetch completed. Concurrent calls with
// the same baseURL collapse via singleflight, so a thundering herd of
// cache-miss reads only generates one upstream request.
//
// Use the returned timestamp to populate a TTL cache one layer up
// (see api.Index for the local cache shape).
func (f *Fetcher) FetchIndex(ctx context.Context, baseURL string, timeout time.Duration) ([]byte, time.Time, error) {
	target, err := joinURL(baseURL, "src", "contrib", "PACKAGES")
	if err != nil {
		return nil, time.Time{}, err
	}

	type result struct {
		body []byte
		when time.Time
	}
	v, err, _ := f.single.Do("index:"+target, func() (any, error) {
		body, when, err := f.getBounded(ctx, target, f.maxIdle, timeout)
		if err != nil {
			return nil, err
		}
		return result{body: body, when: when}, nil
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	r := v.(result)
	return r.body, r.when, nil
}

// FetchTarball streams baseURL/src/contrib/<filename> into CAS and
// returns the resulting BlobRef. The HTTP response is wrapped in an
// [io.LimitReader] capped at maxSize before the CAS write so a hostile
// upstream can't bomb the tempfile area.
//
// Concurrent calls with the same (baseURL, filename) collapse via
// singleflight. Idempotency falls out naturally: the CAS dedups on
// hash, and if a duplicate fetch races past singleflight (e.g. the
// first finished, key cleared, second arrived) the CAS still writes
// once.
func (f *Fetcher) FetchTarball(ctx context.Context, baseURL, filename string, maxSize int64, timeout time.Duration) (store.BlobRef, error) {
	if filename == "" {
		return store.BlobRef{}, errors.New("upstream: empty tarball filename")
	}
	if strings.ContainsAny(filename, "/\\") {
		return store.BlobRef{}, fmt.Errorf("upstream: tarball filename %q must not contain slashes", filename)
	}
	target, err := joinURL(baseURL, "src", "contrib", filename)
	if err != nil {
		return store.BlobRef{}, err
	}

	v, err, _ := f.single.Do("tarball:"+target, func() (any, error) {
		return f.streamToCAS(ctx, target, maxSize, timeout)
	})
	if err != nil {
		return store.BlobRef{}, err
	}
	return v.(store.BlobRef), nil
}

// getBounded GETs target into a fully-buffered byte slice, refusing
// responses larger than maxBytes. The full-buffer pattern is intended
// for PACKAGES (always small); tarballs go through streamToCAS.
func (f *Fetcher) getBounded(ctx context.Context, target string, maxBytes int64, timeout time.Duration) ([]byte, time.Time, error) {
	reqCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, &ErrUpstreamStatus{URL: target, Status: resp.StatusCode}
	}

	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, time.Time{}, fmt.Errorf("%w: %s exceeded %d bytes", ErrTooLarge, target, maxBytes)
	}
	return body, time.Now().UTC(), nil
}

// streamToCAS GETs target and streams the response body through the
// CAS writer, refusing responses larger than maxBytes via
// [io.LimitReader]. The CAS-side stat-and-rename takes care of
// deduplication if the same bytes were already cached.
func (f *Fetcher) streamToCAS(ctx context.Context, target string, maxBytes int64, timeout time.Duration) (store.BlobRef, error) {
	reqCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return store.BlobRef{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return store.BlobRef{}, fmt.Errorf("get %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return store.BlobRef{}, &ErrUpstreamStatus{URL: target, Status: resp.StatusCode}
	}

	// Reject obvious-oversize responses by Content-Length before
	// spending a CAS tempfile on them. The LimitReader below catches
	// the case where Content-Length lied or wasn't provided.
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return store.BlobRef{}, fmt.Errorf("%w: %s Content-Length %d > %d",
			ErrTooLarge, target, resp.ContentLength, maxBytes)
	}

	limited := &boundedReader{r: io.LimitReader(resp.Body, maxBytes+1), max: maxBytes}
	blob, err := f.store.WriteBlob(limited)
	if err != nil {
		return store.BlobRef{}, fmt.Errorf("write blob from %s: %w", target, err)
	}
	if limited.tripped {
		// The CAS already wrote a tempfile-and-rename for the
		// partial body. That blob is now orphaned in CAS; periodic GC
		// reclaims it. We refuse the row rather than ingest unsafe bytes.
		return store.BlobRef{}, fmt.Errorf("%w: %s exceeded %d bytes mid-stream", ErrTooLarge, target, maxBytes)
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
		return "", fmt.Errorf("parse base URL %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base URL %q is not absolute", base)
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
