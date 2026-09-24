package upstream_test

import (
	"context"
	"errors"
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

	got, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, "", time.Second)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestFetcherFetchIndex404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})

	_, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, "", time.Second)
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

	blob, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"praise_1.0.0.tar.gz"}, 1024, time.Second)
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

	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"big_1.tar.gz"}, 100, time.Second)
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

	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"sneaky_1.tar.gz"}, 100, time.Second)
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
			_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"slow_1.tar.gz"}, 1024, 5*time.Second)
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
	_, err := f.fetcher.FetchIndex(context.Background(), "/relative/url", "", time.Second)
	if err == nil {
		t.Fatal("expected error for relative base URL")
	}
}

func TestFetcherRejectsFilenameWithSlash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"../etc/passwd"}, 1024, time.Second)
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
	_, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"hang_1.tar.gz"}, 1024, 50*time.Millisecond)
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
	got, err := f.fetcher.FetchIndex(context.Background(), base, "", time.Second)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestFetcherSendsUserAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var got []string
	var mu sync.Mutex
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.UserAgent())
		mu.Unlock()
		_, _ = io.WriteString(w, "Package: x\n")
	})
	if _, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, "", time.Second); err != nil {
		t.Fatal(err)
	}
	ua := "R (4.4.0 x86_64-pc-linux-gnu x86_64 linux-gnu)"
	if _, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, ua, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "packyard/") || got[1] != ua {
		t.Errorf("user agents = %q", got)
	}
}

// Requests for the same URL with different User-Agents are different
// upstream responses (PPM serves a binary per R version) and must not
// share one flight.
func TestFetcherSingleflightKeyIncludesUserAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	release := make(chan struct{})
	f.mux.HandleFunc("/src/contrib/x_1.0.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, "binary for "+r.UserAgent())
	})
	var wg sync.WaitGroup
	sums := make([]string, 2)
	for i, ua := range []string{"R (4.4.0 a b c)", "R (4.5.0 a b c)"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			blob, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, ua, []string{"x_1.0.tar.gz"}, 1024, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			sums[i] = blob.SHA256
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if sums[0] == "" || sums[0] == sums[1] {
		t.Errorf("both R versions got the same blob: %v", sums)
	}
}

// The first caller of a shared fetch going away must not fail the
// callers that joined it.
func TestFetcherFirstCallerCancelDoesNotFailOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	release := make(chan struct{})
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "Package: x\n")
	})
	firstCtx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := f.fetcher.FetchIndex(firstCtx, f.srv.URL, "", 5*time.Second)
		firstErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	secondErr := make(chan error, 1)
	go func() {
		_, err := f.fetcher.FetchIndex(context.Background(), f.srv.URL, "", 5*time.Second)
		secondErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Errorf("first caller: err = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-secondErr; err != nil {
		t.Errorf("second caller failed because the first went away: %v", err)
	}
}

func TestFetcherRedactsCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux.HandleFunc("/src/contrib/PACKAGES", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	base := strings.Replace(f.srv.URL, "http://", "http://user:s3cret@", 1)
	_, err := f.fetcher.FetchIndex(context.Background(), base, "", time.Second)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func TestFetcherFetchTarballArchivePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mux.HandleFunc("/src/contrib/Archive/x/x_0.9.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "old x")
	})
	if _, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"Archive", "x", "x_0.9.tar.gz"}, 1024, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.fetcher.FetchTarball(context.Background(), f.srv.URL, "", []string{"Archive", "..", "x"}, 1024, time.Second); err == nil {
		t.Error("dot-dot segment accepted")
	}
}
