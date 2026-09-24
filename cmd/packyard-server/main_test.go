package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	// A specific listen host is probed directly; 0.0.0.0 means loopback.
	for _, listen := range []string{"127.0.0.1:" + port, "0.0.0.0:" + port} {
		cfg.Listen = listen
		if err := runHealthcheck(&cfg, ""); err != nil {
			t.Errorf("listen %s: %v", listen, err)
		}
	}

	status.Store(http.StatusServiceUnavailable)
	if err := runHealthcheck(&cfg, srv.URL+"/health"); err == nil {
		t.Error("503 reported healthy")
	}
	if err := runHealthcheck(&cfg, "http://127.0.0.1:1/health"); err == nil {
		t.Error("closed port reported healthy")
	}
}

func TestMintTokenValidatesScopes(t *testing.T) {
	cfg := config.DefaultServerConfig()
	cfg.DataDir = t.TempDir()
	for _, bad := range []string{"pubish:*", "publish: prod", "Publish:prod"} {
		if err := runMintToken(&cfg, bad, "x"); err == nil || !strings.Contains(err.Error(), "invalid scope") {
			t.Errorf("scopes %q: err = %v", bad, err)
		}
	}
	if err := runMintToken(&cfg, " ", "x"); err == nil {
		t.Error("empty scopes accepted")
	}
}

// A server.yaml that moves channels.yaml elsewhere still gets a
// default matrix.yaml written into the data dir, and nothing else.
func TestInitBootstrapsOnlyDataDirFiles(t *testing.T) {
	cfg := config.DefaultServerConfig()
	cfg.DataDir = t.TempDir()
	cfg.ChannelsFile = filepath.Join(t.TempDir(), "channels.yaml")
	if err := os.WriteFile(cfg.ChannelsFile, []byte("channels:\n  - name: prod\n    overwrite_policy: immutable\n    default: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() error { return runInit(&cfg, config.BootstrapOptions{}) })
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "matrix.yaml")); err != nil {
		t.Errorf("matrix.yaml not bootstrapped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "channels.yaml")); !os.IsNotExist(err) {
		t.Errorf("channels.yaml written into the data dir despite channels_file: %v", err)
	}
}
