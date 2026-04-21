// Command pakman-server is the pakman package registry server.
//
// See https://github.com/schochastics/pakman for documentation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/schochastics/pakman/internal/api"
	"github.com/schochastics/pakman/internal/auth"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/db"
	"github.com/schochastics/pakman/internal/version"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		configPath  = flag.String("config", "", "path to server config file (YAML)")
		dataDir     = flag.String("data", "./data", "data directory (SQLite + CAS blobs); ignored when -config is set")
		initStorage = flag.Bool("init", false, "initialize data dir (bootstrap configs, create DB, migrate, sync channels) and exit")
		mintToken   = flag.Bool("mint-token", false, "issue a new API token and exit (prints plaintext once)")
		tokenScopes = flag.String("scopes", "", "comma-separated scopes for -mint-token (e.g. 'publish:*,read:*,admin')")
		tokenLabel  = flag.String("label", "", "human-readable label for -mint-token")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	cfg, err := resolveConfig(*configPath, *dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pakman-server: config: %v\n", err)
		os.Exit(1)
	}

	switch {
	case *initStorage:
		if err := runInit(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pakman-server: init failed: %v\n", err)
			os.Exit(1)
		}
		return
	case *mintToken:
		if err := runMintToken(cfg, *tokenScopes, *tokenLabel); err != nil {
			fmt.Fprintf(os.Stderr, "pakman-server: mint-token failed: %v\n", err)
			os.Exit(1)
		}
		return
	default:
		if err := runServe(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pakman-server: serve failed: %v\n", err)
			os.Exit(1)
		}
	}
}

// resolveConfig produces a ServerConfig from either the YAML file at
// configPath or the command-line fallbacks. If configPath is empty
// every field comes from defaults overridden by the -data flag.
func resolveConfig(configPath, dataDir string) (*config.ServerConfig, error) {
	if configPath != "" {
		return config.LoadServer(configPath)
	}
	cfg := config.DefaultServerConfig()
	cfg.DataDir = dataDir
	return &cfg, nil
}

// runInit bootstraps everything needed to make the data dir ready for
// the first request. Safe to run against an existing data dir: every
// step is idempotent.
func runInit(cfg *config.ServerConfig) error {
	dataDir := cfg.DataDir
	if dataDir == "" {
		return fmt.Errorf("data dir is required")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	bootstrap, err := config.BootstrapDefaults(dataDir)
	if err != nil {
		return fmt.Errorf("bootstrap default configs: %w", err)
	}
	for _, p := range bootstrap.Written {
		fmt.Printf("wrote default config: %s\n", p)
	}

	channels, err := config.LoadChannels(cfg.ChannelsPath())
	if err != nil {
		return fmt.Errorf("load channels: %w", err)
	}
	if _, err := config.LoadMatrix(cfg.MatrixPath()); err != nil {
		return fmt.Errorf("load matrix: %w", err)
	}

	database, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	if _, err := cas.New(filepath.Join(dataDir, "cas")); err != nil {
		return fmt.Errorf("prepare cas: %w", err)
	}

	rec, err := config.ReconcileChannels(context.Background(), database.DB, channels)
	if err != nil {
		return fmt.Errorf("sync channels: %w", err)
	}
	printReconcile(rec)

	fmt.Printf("storage ready: db=%s cas=%s\n", filepath.Join(dataDir, "db.sqlite"), filepath.Join(dataDir, "cas"))
	return nil
}

func openDB(cfg *config.ServerConfig) (*db.DB, error) {
	path := filepath.Join(cfg.DataDir, "db.sqlite")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.MigrateEmbedded(context.Background(), database); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return database, nil
}

// runMintToken issues a new token row and prints the plaintext to
// stdout. Since we never store plaintext, this is the only chance the
// operator has to capture it. The companion /api/v1/admin/tokens
// endpoint in A6 will do the same thing over HTTP.
func runMintToken(cfg *config.ServerConfig, scopes, label string) error {
	scopes = strings.TrimSpace(scopes)
	if scopes == "" {
		return fmt.Errorf("-scopes is required (e.g. 'publish:*,read:*,admin')")
	}
	if label == "" {
		label = "cli-" + time.Now().UTC().Format("20060102-150405")
	}

	database, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	plaintext, err := auth.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	_, err = database.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(plaintext), scopes, label)
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}

	// Deliberately print JUST the token, unadorned, so shell pipelines
	// like `TOKEN=$(pakman-server -mint-token ...)` work cleanly. Other
	// context goes to stderr.
	fmt.Fprintf(os.Stderr, "issued token label=%q scopes=%q\n", label, scopes)
	fmt.Println(plaintext)
	return nil
}

// runServe starts the HTTP server. Exits cleanly on SIGINT/SIGTERM,
// giving in-flight requests up to 30 seconds to finish. A failed
// migration or config load here is fatal — we'd rather refuse to start
// than serve a half-initialized request surface.
func runServe(cfg *config.ServerConfig) error {
	matrix, err := config.LoadMatrix(cfg.MatrixPath())
	if err != nil {
		return fmt.Errorf("load matrix: %w", err)
	}
	channels, err := config.LoadChannels(cfg.ChannelsPath())
	if err != nil {
		return fmt.Errorf("load channels: %w", err)
	}

	database, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	// Reconcile on every start so YAML edits take effect without a
	// separate -init run. ReconcileChannels is idempotent so the common
	// case (no changes) is a cheap no-op.
	if rec, err := config.ReconcileChannels(context.Background(), database.DB, channels); err != nil {
		return fmt.Errorf("sync channels: %w", err)
	} else {
		printReconcile(rec)
	}

	store, err := cas.New(filepath.Join(cfg.DataDir, "cas"))
	if err != nil {
		return fmt.Errorf("prepare cas: %w", err)
	}

	deps := api.Deps{DB: database, CAS: store, Matrix: matrix, Server: cfg}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.NewMux(deps),
		ReadHeaderTimeout: 10 * time.Second,
		// Read/Write timeouts are deliberately NOT set: publish uploads
		// can be slow over poor connections, and forcing a deadline mid-
		// multipart stream would corrupt big pushes. MaxBytesReader in
		// the publish handler bounds the payload.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("pakman-server listening",
			"addr", cfg.Listen,
			"tls", cfg.TLSEnabled(),
			"version", version.Version,
		)
		if cfg.TLSEnabled() {
			errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutdown signal received; draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	slog.Info("pakman-server stopped")
	return nil
}

func printReconcile(r config.ReconcileResult) {
	if len(r.Created) > 0 {
		fmt.Printf("created channels: %s\n", strings.Join(r.Created, ", "))
	}
	if len(r.Updated) > 0 {
		fmt.Printf("updated channels: %s\n", strings.Join(r.Updated, ", "))
	}
	if len(r.Obsolete) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: channels in DB but not in channels.yaml (left in place): %s\n",
			strings.Join(r.Obsolete, ", "))
	}
}
