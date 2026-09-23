package upstream_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/upstream"
)

// fixture is a Fetcher backed by a real CAS rooted in a temp dir, plus
// an httptest mock upstream. Tests register handlers on mux; the
// fixture supplies the base URL.
type fixture struct {
	srv     *httptest.Server
	mux     *http.ServeMux
	fetcher *upstream.Fetcher
	cas     *cas.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	casStore, err := cas.New(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	svc := store.New(nil, casStore) // DB nil — we only exercise WriteBlob
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &fixture{
		srv:     srv,
		mux:     mux,
		fetcher: upstream.New(srv.Client(), svc),
		cas:     casStore,
	}
}

func TestFetcherFetchIndexReturnsBody(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const body = "Package: praise\nVersion: 1.0.0\n\n"
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	got, when, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, time.Second)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if time.Since(when) > time.Second {
		t.Errorf("fetched_at = %v, looks stale", when)
	}
}

func TestFetcherFetchIndex404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})

	_, _, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, time.Second)
	if err == nil {
		t.Fatal("FetchIndex returned nil error for 404")
	}
	if !upstream.NotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestFetcherFetchTarballWritesToCAS(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const body = "tarball-bytes"
	f.mux.HandleFunc("/src/contrib/praise_1.0.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	blob, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "praise_1.0.0.tar.gz", 1024, time.Second)
	if err != nil {
		t.Fatalf("FetchTarball: %v", err)
	}
	if blob.Size != int64(len(body)) {
		t.Errorf("BlobRef.Size = %d, want %d", blob.Size, len(body))
	}
	if blob.SHA256 == "" {
		t.Fatal("BlobRef.SHA256 is empty")
	}
	// Read it back from CAS.
	rc, err := f.cas.Read(blob.SHA256)
	if err != nil {
		t.Fatalf("CAS.Read: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("CAS contents = %q, want %q", got, body)
	}
}

func TestFetcherFetchTarballRejectsOversizeByContentLength(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Send a Content-Length that exceeds maxSize. The fetcher should
	// refuse before reading the body, so the test handler doesn't
	// need to stream anything.
	f.mux.HandleFunc("/src/contrib/big_1.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2048")
		w.WriteHeader(http.StatusOK)
		// Write padding so the connection drains cleanly.
		_, _ = w.Write(make([]byte, 2048))
	})

	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "big_1.tar.gz", 100, time.Second)
	if err == nil {
		t.Fatal("expected ErrTooLarge")
	}
	if !strings.Contains(err.Error(), "exceeds configured tarball_max_size") {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestFetcherFetchTarballRejectsOversizeMidStream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Force chunked transfer (no Content-Length header) by flushing
	// between writes — that way the fetcher can't reject upfront and
	// has to trip the boundedReader mid-stream instead.
	f.mux.HandleFunc("/src/contrib/sneaky_1.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter has no Flusher; httptest changed shape?")
			return
		}
		_, _ = w.Write(make([]byte, 50))
		flusher.Flush()
		_, _ = w.Write(make([]byte, 150)) // total 200, > 100-byte limit
	})

	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "sneaky_1.tar.gz", 100, time.Second)
	if err == nil {
		t.Fatal("expected ErrTooLarge")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want mid-stream limit hit", err)
	}
}

func TestFetcherSingleflightCollapsesConcurrentTarballFetches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var hits int64
	gate := make(chan struct{})
	f.mux.HandleFunc("/src/contrib/slow_1.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		<-gate // hold the response open until the test releases
		_, _ = io.WriteString(w, "slow-body")
	})

	// Kick off N concurrent fetches against the same URL. Singleflight
	// should funnel them through one upstream request.
	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "slow_1.tar.gz", 1024, 5*time.Second)
			errs <- err
		}()
	}

	// Give the goroutines time to enter Do and queue behind the inflight call.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("FetchTarball: %v", err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (singleflight collapsed)", got)
	}
}

func TestFetcherRejectsRelativeBase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, _, err := f.fetcher.FetchIndex(context.Background(), "/relative/url", time.Second)
	if err == nil {
		t.Fatal("expected error for relative base URL")
	}
}

func TestFetcherRejectsFilenameWithSlash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "../etc/passwd", 1024, time.Second)
	if err == nil {
		t.Fatal("expected error for filename containing slash")
	}
}

func TestFetcherFetchTarballHonoursTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux.HandleFunc("/src/contrib/hang_1.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		// Block until the request context cancels.
		<-r.Context().Done()
	})

	start := time.Now()
	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "hang_1.tar.gz", 1024, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; timeout did not fire", elapsed)
	}
}

func TestFetcherJoinURLPreservesBasePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Base with an existing path (PPM-style /cran/<date>).
	body := "Package: x\n"
	f.mux.HandleFunc("/cran/2026-01-15/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	base := fmt.Sprintf("%s/cran/2026-01-15", f.srv.URL)
	got, _, err := f.fetcher.FetchIndex(context.Background(), base, time.Second)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}
