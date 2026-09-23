package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/backup"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/version"
)

// adminBackup snapshots the data dir (-out) or checks a snapshot
// (-verify). Safe against a running server.
func adminBackup(cfg *config.ServerConfig, configPath string, args []string) error {
	fs := flag.NewFlagSet("admin backup", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write the backup into (created if missing; reusable for incremental backups)")
	verify := fs.String("verify", "", "verify the backup in this directory instead of writing one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*out == "") == (*verify == "") {
		return adminUsageError("admin backup: pass exactly one of -out or -verify")
	}
	ctx := context.Background()

	if *verify != "" {
		rep, err := backup.Verify(ctx, *verify)
		if err != nil {
			return err
		}
		fmt.Printf("backup from %s (packyard %s, schema %d)\n",
			rep.Manifest.CreatedAt, rep.Manifest.PackyardVersion, rep.Manifest.SchemaVersion)
		fmt.Printf("integrity_check: %s\nblobs re-hashed: %d\nmissing: %d\ncorrupt: %d\n",
			rep.Integrity, rep.Checked, len(rep.Missing), len(rep.Corrupt))
		for _, m := range rep.Missing {
			fmt.Printf("  missing %s\n", m)
		}
		for _, c := range rep.Corrupt {
			fmt.Printf("  corrupt %s\n", c)
		}
		if !rep.OK() {
			return errors.New("backup verification failed")
		}
		return nil
	}

	database, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	files := map[string]string{
		"channels.yaml": cfg.ChannelsPath(),
		"matrix.yaml":   cfg.MatrixPath(),
	}
	if configPath != "" {
		files["server.yaml"] = configPath
	}
	res, err := backup.Backup(ctx, backup.Source{
		DB:          database.DB,
		CASRoot:     filepath.Join(cfg.DataDir, "cas"),
		ConfigFiles: files,
		Version:     version.Version,
	}, *out)
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s: blobs=%d (%d new) size=%s config=%v\n",
		*out, res.Manifest.Blobs, res.BlobsAdded, humanBytes(res.Manifest.BlobBytes), res.Manifest.ConfigFiles)
	return nil
}

// adminRestore writes a verified backup into a data dir. The server
// must be stopped.
func adminRestore(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin restore", flag.ContinueOnError)
	from := fs.String("from", "", "backup directory to restore (required)")
	dataDir := fs.String("data", cfg.DataDir, "data directory to restore into")
	force := fs.Bool("force", false, "replace an existing database and blob store")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *from == "" {
		return adminUsageError("admin restore: -from is required")
	}
	target := *cfg
	target.DataDir = *dataDir
	res, err := backup.Restore(context.Background(), *from, backup.Target{
		DataDir: *dataDir,
		ConfigFiles: map[string]string{
			"channels.yaml": target.ChannelsPath(),
			"matrix.yaml":   target.MatrixPath(),
		},
		Force: *force,
	})
	if err != nil {
		return err
	}
	fmt.Printf("restored into %s: blobs=%d config=%v\n", *dataDir, res.Blobs, res.ConfigWritten)
	for _, name := range res.ConfigSkipped {
		fmt.Printf("not restored: %s (destination exists or is managed outside the data dir); the backup's copy is %s\n",
			name, filepath.Join(*from, "config", name))
	}
	fmt.Println("start the server to apply any pending migrations.")
	return nil
}

// adminTokenGen prints a new token and its sha256, for provisioning
// through server.yaml tokens: without the server ever seeing the
// plaintext (sha256_file) — the CI secret gets line 1, the server
// secret line 2. Touches no database.
func adminTokenGen(args []string) error {
	if len(args) != 0 {
		return adminUsageError("admin token-gen: no arguments expected")
	}
	tok, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	fmt.Println(tok)
	fmt.Println(auth.HashToken(tok))
	return nil
}
