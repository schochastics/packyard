// Command pakman-server is the pakman package registry server.
//
// See https://github.com/schochastics/pakman for documentation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	cfg, err := resolveConfig(*configPath, *dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pakman-server: config: %v\n", err)
		os.Exit(1)
	}

	if *initStorage {
		if err := runInit(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pakman-server: init failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Fprintln(os.Stderr, "pakman-server: HTTP server not yet implemented (run with -version or -init)")
	os.Exit(2)
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
// the first request: the data directory itself, default config files,
// the SQLite database with migrations applied, the CAS store, and a
// channels table synced against channels.yaml.
//
// Safe to run against an existing data dir: every step is idempotent.
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

	dbPath := filepath.Join(dataDir, "db.sqlite")
	casRoot := filepath.Join(dataDir, "cas")

	if _, err := cas.New(casRoot); err != nil {
		return fmt.Errorf("prepare cas: %w", err)
	}

	ctx := context.Background()
	database, err := db.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = database.Close() }()

	if err := db.MigrateEmbedded(ctx, database); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	rec, err := config.ReconcileChannels(ctx, database.DB, channels)
	if err != nil {
		return fmt.Errorf("sync channels: %w", err)
	}
	printReconcile(rec)

	fmt.Printf("storage ready: db=%s cas=%s\n", dbPath, casRoot)
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
		// Not an error — operator may have intentionally removed a
		// channel from YAML. But it's worth surfacing because these
		// rows still live in the DB and may still have packages.
		fmt.Fprintf(os.Stderr,
			"warning: channels in DB but not in channels.yaml (left in place): %s\n",
			strings.Join(r.Obsolete, ", "))
	}
}
