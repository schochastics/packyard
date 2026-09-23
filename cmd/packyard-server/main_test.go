package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/schochastics/packyard/internal/config"
)

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)

	// Derived from the listen address: 127.0.0.1:<port>/health.
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	cfg := config.DefaultServerConfig()
	cfg.Listen = ":" + port
	if err := runHealthcheck(&cfg, ""); err != nil {
		t.Errorf("healthy server: %v", err)
	}

	status.Store(http.StatusServiceUnavailable)
	if err := runHealthcheck(&cfg, srv.URL+"/health"); err == nil {
		t.Error("503 reported healthy")
	}
	if err := runHealthcheck(&cfg, "http://127.0.0.1:1/health"); err == nil {
		t.Error("closed port reported healthy")
	}
}
