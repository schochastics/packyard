package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
)

// TestBackupRestoreServeRoundTrip backs up a data dir, restores it
// elsewhere, and serves the restored copy.
func TestBackupRestoreServeRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := config.DefaultServerConfig()
	cfg.DataDir = t.TempDir()
	captureStdout(t, func() error { return runInit(&cfg, config.BootstrapOptions{}) })

	database, err := openDB(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.New(filepath.Join(cfg.DataDir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	sum, size, err := store.Write(strings.NewReader("foo source bytes"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := auth.GenerateToken()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO packages(channel, name, version, source_sha256, source_size, index_fields)
		  VALUES ('prod', 'foo', '1.0.0', ?, ?, '{}')`, []any{sum, size}},
		{`INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, 'read:*', 'reader')`,
			[]any{auth.HashToken(token)}},
	} {
		if _, err := database.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	_ = database.Close()

	out := filepath.Join(t.TempDir(), "backup")
	if got := captureStdout(t, func() error { return adminBackup(&cfg, "", []string{"-out", out}) }); !strings.Contains(got, "blobs=1 (1 new)") {
		t.Errorf("backup output = %q", got)
	}
	if got := captureStdout(t, func() error { return adminBackup(&cfg, "", []string{"-verify", out}) }); !strings.Contains(got, "integrity_check: ok") {
		t.Errorf("verify output = %q", got)
	}

	restored := config.DefaultServerConfig()
	restored.DataDir = t.TempDir()
	got := captureStdout(t, func() error { return adminRestore(&restored, []string{"-from", out}) })
	if !strings.Contains(got, "blobs=1") || !strings.Contains(got, "channels.yaml") {
		t.Errorf("restore output = %q", got)
	}
	if err := adminRestore(&restored, []string{"-from", out}); err == nil {
		t.Error("second restore without -force succeeded")
	}

	rdb, err := openDB(&restored)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	rcas, err := cas.New(filepath.Join(restored.DataDir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	channels, err := config.LoadChannels(restored.ChannelsPath())
	if err != nil {
		t.Fatal(err)
	}
	matrix, err := config.LoadMatrix(restored.MatrixPath())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewMux(api.Deps{DB: rdb, CAS: rcas, Channels: channels, Matrix: matrix, Server: &restored}))
	defer srv.Close()

	for path, want := range map[string]string{
		"/prod/src/contrib/PACKAGES":         "Package: foo\nVersion: 1.0.0\n",
		"/prod/src/contrib/foo_1.0.0.tar.gz": "foo source bytes",
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), want) {
			t.Errorf("GET %s = %d %q, want %q", path, resp.StatusCode, b, want)
		}
	}
}

func TestAdminBackupFlags(t *testing.T) {
	cfg := config.DefaultServerConfig()
	for _, args := range [][]string{{}, {"-out", "a", "-verify", "b"}} {
		if err := adminBackup(&cfg, "", args); err == nil {
			t.Errorf("adminBackup(%v) succeeded", args)
		}
	}
	if err := adminRestore(&cfg, nil); err == nil {
		t.Error("adminRestore without -from succeeded")
	}
}

func TestAdminTokenGen(t *testing.T) {
	out := strings.Fields(captureStdout(t, func() error { return adminTokenGen(nil) }))
	if len(out) != 2 || !strings.HasPrefix(out[0], "pkm_") || auth.HashToken(out[0]) != out[1] {
		t.Errorf("token-gen output = %q", out)
	}
}
