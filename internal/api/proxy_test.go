package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/upstream"
)

// proxyFixture wires a packyard with a proxy channel pointing at a
// mock upstream. The upstream is a *httptest.Server registered on the
// shared mux; tests add handlers under /src/contrib/... before
// hitting packyard.
type proxyFixture struct {
	deps    Deps
	mux     http.Handler
	srv     *httptest.Server // packyard's own server
	upMux   *http.ServeMux   // mock upstream's mux
	upSrv   *httptest.Server // mock upstream
	upHits  *int64           // bumps on every request to mock upstream
	channel string           // proxy channel name
}

// newProxyFixture constructs everything needed for an end-to-end
// proxy test: mock upstream + packyard server with anonymous reads
// enabled (so tests don't have to manage tokens).
func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(dir, "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	casStore, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}

	// Spin up the mock upstream and a counter for hits.
	var hits int64
	upMux := http.NewServeMux()
	upMuxCounting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		upMux.ServeHTTP(w, r)
	})
	upSrv := httptest.NewServer(upMuxCounting)
	t.Cleanup(upSrv.Close)

	channels := &config.ChannelsConfig{Channels: []config.Channel{
		{Name: "prod", OverwritePolicy: config.PolicyImmutable, Default: true, Kind: config.KindLocal},
		{Name: "cran", OverwritePolicy: config.PolicyImmutable, Kind: config.KindProxy, Upstream: &config.UpstreamConfig{
			SourceURL: upSrv.URL,
			BinaryURLs: map[string]string{
				"ubuntu-22.04-amd64-r-4.4": upSrv.URL + "/__linux__/jammy",
			},
		}},
	}}
	if _, err := config.ReconcileChannels(ctx, database.DB, channels); err != nil {
		t.Fatalf("ReconcileChannels: %v", err)
	}

	matrix := &config.MatrixConfig{Cells: []config.Cell{
		{Name: "ubuntu-22.04-amd64-r-4.4", OS: "ubuntu", OSVersion: "22.04", Arch: "amd64", RMinor: "4.4"},
	}}

	svc := store.New(database.DB, casStore)
	deps := Deps{
		DB:       database,
		CAS:      casStore,
		Matrix:   matrix,
		Channels: channels,
		Server:   &config.ServerConfig{AllowAnonymousReads: false},
		Index:    NewIndex(database.DB),
		Store:    svc,
		Upstream: upstream.New(upSrv.Client(), svc),
	}
	mux := NewMux(deps)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &proxyFixture{
		deps:    deps,
		mux:     mux,
		srv:     srv,
		upMux:   upMux,
		upSrv:   upSrv,
		upHits:  &hits,
		channel: "cran",
	}
}

func (f *proxyFixture) authedGet(t *testing.T, path string) *http.Response {
	t.Helper()
	// Seed a read:* token on demand.
	token := seedTokenRow(t, f.deps.DB.DB, "test", "read:*", false)
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProxySourcePackagesPassThroughUpstreamBody(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)

	const body = "Package: praise\nVersion: 1.0.0\n\n"
	f.upMux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	resp := f.authedGet(t, "/cran/src/contrib/PACKAGES")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestProxySourceTarballFetchAndCache(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)

	const body = "praise-source-bytes"
	f.upMux.HandleFunc("/src/contrib/praise_1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	// First fetch: hits upstream, materializes a row, serves bytes.
	resp1 := f.authedGet(t, "/cran/src/contrib/praise_1.0.0.tar.gz")
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", resp1.StatusCode)
	}
	got, _ := io.ReadAll(resp1.Body)
	if string(got) != body {
		t.Errorf("first body = %q, want %q", got, body)
	}
	hitsAfterFirst := atomic.LoadInt64(f.upHits)

	// Second fetch: should hit local CAS, not upstream.
	resp2 := f.authedGet(t, "/cran/src/contrib/praise_1.0.0.tar.gz")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp2.StatusCode)
	}
	got2, _ := io.ReadAll(resp2.Body)
	if string(got2) != body {
		t.Errorf("second body = %q, want %q", got2, body)
	}
	if hits := atomic.LoadInt64(f.upHits); hits != hitsAfterFirst {
		t.Errorf("upstream hit count increased from %d to %d on cached read", hitsAfterFirst, hits)
	}

	// And the audit row should be present.
	var n int
	if err := f.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE type='proxy_tarball_fetch' AND channel='cran'`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n == 0 {
		t.Error("no proxy_tarball_fetch event row")
	}
}

func TestProxySourceTarball404Upstream(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	f.upMux.HandleFunc("/src/contrib/nope_1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})

	resp := f.authedGet(t, "/cran/src/contrib/nope_1.0.0.tar.gz")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProxyBinaryTarballMaterializesSourceFirst(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)

	f.upMux.HandleFunc("/src/contrib/foo_1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "foo-source")
	})
	f.upMux.HandleFunc("/__linux__/jammy/src/contrib/foo_1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "foo-binary-jammy")
	})

	resp := f.authedGet(t, "/cran/bin/linux/ubuntu-22.04-amd64-r-4.4/foo_1.0.0.tar.gz")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "foo-binary-jammy" {
		t.Errorf("body = %q, want foo-binary-jammy", got)
	}

	// Both source and binary rows must exist now.
	var pkgID int64
	if err := f.deps.DB.QueryRowContext(context.Background(),
		`SELECT id FROM packages WHERE channel='cran' AND name='foo' AND version='1.0.0'`).Scan(&pkgID); err != nil {
		t.Fatalf("source row missing: %v", err)
	}
	var binCount int
	if err := f.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM binaries WHERE package_id=? AND cell='ubuntu-22.04-amd64-r-4.4'`, pkgID).Scan(&binCount); err != nil {
		t.Fatal(err)
	}
	if binCount != 1 {
		t.Errorf("binary row count = %d, want 1", binCount)
	}
}

func TestProxyBinaryCellWithoutUpstream404(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	// We only configured upstream binary for ubuntu-22.04-amd64-r-4.4.
	// Add a cell to matrix that we DON'T proxy.
	f.deps.Matrix.Cells = append(f.deps.Matrix.Cells, config.Cell{
		Name: "rhel9-amd64-r-4.4", OS: "rhel", OSVersion: "9", Arch: "amd64", RMinor: "4.4",
	})

	resp := f.authedGet(t, "/cran/bin/linux/rhel9-amd64-r-4.4/foo_1.0.0.tar.gz")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProxyChannelRejectsPublish(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	token := seedTokenRow(t, f.deps.DB.DB, "ci", "publish:*", false)

	// Minimal multipart body — handler should refuse before parsing it.
	req, err := http.NewRequest(http.MethodPost,
		f.srv.URL+"/api/v1/packages/cran/foo/1.0.0",
		strings.NewReader("ignored"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestProxyChannelRejectsYank(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	token := seedTokenRow(t, f.deps.DB.DB, "ci", "yank:*", false)

	req, _ := http.NewRequest(http.MethodPost,
		f.srv.URL+"/api/v1/packages/cran/foo/1.0.0/yank",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestProxyChannelStaleWhileError(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)

	const fresh = "Package: praise\nVersion: 1.0.0\n\n"
	// First serve the fresh body.
	var serveErr atomic.Bool
	f.upMux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		if serveErr.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, fresh)
	})

	// Warm the cache.
	resp1 := f.authedGet(t, "/cran/src/contrib/PACKAGES")
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if string(body1) != fresh {
		t.Fatalf("warm-up body = %q", body1)
	}

	// Force a re-fetch by zeroing the TTL on the cached entry.
	idx := f.deps.Index
	idx.mu.Lock()
	for k, e := range idx.entries {
		e.expires = e.expires.Add(-time.Hour)
		idx.entries[k] = e
	}
	idx.mu.Unlock()

	// Make the next upstream call fail.
	serveErr.Store(true)

	resp2 := f.authedGet(t, "/cran/src/contrib/PACKAGES")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("stale-while-error status = %d, want 200", resp2.StatusCode)
	}
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != fresh {
		t.Errorf("stale body = %q, want %q", body2, fresh)
	}

	// And the stale-served event row should have been emitted.
	var n int
	_ = f.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE type='proxy_index_stale_served'`).Scan(&n)
	if n == 0 {
		t.Error("no proxy_index_stale_served event row")
	}
}

// fmt import is used by some helpers above; ensure it stays in.
var _ = fmt.Sprintf
