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

	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/db"
	"github.com/schochastics/pakman/internal/version"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		configPath  = flag.String("config", "", "path to server config file")
		dataDir     = flag.String("data", "./data", "data directory (SQLite + CAS blobs)")
		initStorage = flag.Bool("init", false, "initialize data dir (create DB + run migrations) and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	if *initStorage {
		if err := runInit(*dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "pakman-server: init failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	_ = configPath
	fmt.Fprintln(os.Stderr, "pakman-server: not yet implemented (run with -version or -init)")
	os.Exit(2)
}

// runInit bootstraps <dataDir>/db.sqlite + <dataDir>/cas/ and applies
// embedded migrations. It is safe to run against an existing data dir:
// migrations skip already-applied versions, and cas.New leaves existing
// blobs untouched.
func runInit(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("data dir is required")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
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

	fmt.Printf("storage ready: db=%s cas=%s\n", dbPath, casRoot)
	return nil
}
