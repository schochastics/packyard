package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/schochastics/pakman/internal/api"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/importers"
)

// adminMain is the entry point for `pakman-server admin …`. Kept out
// of main.go so the imperative shell there stays small.
//
// Top-level grammar:
//
//	pakman-server admin [-data DIR] [-config PATH] <verb> [args…]
//
// where <verb> is one of:
//
//	import drat <repo-url> -channel <name>
//	import git  <repo-url> [-branch <b>] -channel <name>
func adminMain(args []string) error {
	if len(args) == 0 {
		return adminUsageError("admin: missing verb")
	}
	// Parse shared -data / -config before the verb.
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to server config YAML")
	dataDir := fs.String("data", "./data", "data directory; ignored when -config is set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return adminUsageError("admin: missing verb after flags")
	}

	cfg, err := resolveConfig(*configPath, *dataDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	switch rest[0] {
	case "import":
		return adminImport(cfg, rest[1:])
	default:
		return adminUsageError("admin: unknown verb %q", rest[0])
	}
}

// adminImport routes `admin import <source> …`.
func adminImport(cfg *config.ServerConfig, args []string) error {
	if len(args) == 0 {
		return adminUsageError("admin import: missing source (drat|git)")
	}
	switch args[0] {
	case "drat":
		return adminImportDrat(cfg, args[1:])
	case "git":
		return adminImportGit(cfg, args[1:])
	default:
		return adminUsageError("admin import: unknown source %q (expected drat|git)", args[0])
	}
}

func adminImportDrat(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin import drat", flag.ContinueOnError)
	channel := fs.String("channel", "", "target pakman channel (required)")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return err
	}
	if *channel == "" {
		return adminUsageError("admin import drat: -channel is required")
	}
	if fs.NArg() != 1 {
		return adminUsageError("admin import drat: expected exactly one <repo-url> argument")
	}
	repoURL := fs.Arg(0)

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	imp := importers.NewDratImporter(deps, *channel)
	res, err := imp.Run(context.Background(), repoURL, func(line string) {
		fmt.Println(line)
	})
	if err != nil {
		return fmt.Errorf("drat import: %w", err)
	}

	fmt.Printf("imported=%d skipped=%d failed=%d\n",
		len(res.Imported), len(res.Skipped), len(res.Failed))
	for _, f := range res.Failed {
		fmt.Fprintf(os.Stderr, "  fail %s@%s: %v\n", f.Package, f.Version, f.Err)
	}
	if len(res.Failed) > 0 {
		return fmt.Errorf("%d packages failed to import", len(res.Failed))
	}
	return nil
}

func adminImportGit(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin import git", flag.ContinueOnError)
	channel := fs.String("channel", "", "target pakman channel (required)")
	branch := fs.String("branch", "", "branch or tag to clone (default: repo's default)")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return err
	}
	if *channel == "" {
		return adminUsageError("admin import git: -channel is required")
	}
	if fs.NArg() != 1 {
		return adminUsageError("admin import git: expected exactly one <repo-url> argument")
	}
	repoURL := fs.Arg(0)

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	imp := importers.NewGitImporter(deps, *channel)
	res, err := imp.Run(context.Background(), repoURL, *branch, func(line string) {
		fmt.Println(line)
	})
	if err != nil {
		return fmt.Errorf("git import: %w", err)
	}

	status := "created"
	if res.Response.AlreadyExisted {
		status = "already_existed"
	} else if res.Response.Overwritten {
		status = "overwrote"
	}
	fmt.Printf("imported %s@%s (%s)\n", res.Package, res.Version, status)
	return nil
}

// openAdminDeps opens the DB and CAS for an admin command and returns
// a cleanup function. Unlike runServe, there's no channel reconcile —
// admin commands shouldn't surprise the operator by rewriting rows.
func openAdminDeps(cfg *config.ServerConfig) (api.Deps, func(), error) {
	matrix, err := config.LoadMatrix(cfg.MatrixPath())
	if err != nil {
		// Non-fatal: some admin commands don't care about the matrix.
		// Log and carry on with a nil Matrix so publish-path sanity
		// checks that do matter still fire on a nil check.
		fmt.Fprintf(os.Stderr, "warning: matrix: %v\n", err)
	}

	database, err := openDB(cfg)
	if err != nil {
		return api.Deps{}, nil, err
	}
	store, err := cas.New(filepath.Join(cfg.DataDir, "cas"))
	if err != nil {
		_ = database.Close()
		return api.Deps{}, nil, fmt.Errorf("cas: %w", err)
	}

	cleanup := func() { _ = database.Close() }
	return api.Deps{DB: database, CAS: store, Matrix: matrix, Server: cfg}, cleanup, nil
}

// reorderFlagsFirst moves every -flag and -flag=value / -flag value
// pair to the front of the returned slice, leaving positionals at the
// tail in their original order. Go's stdlib flag.Parse stops at the
// first positional; this shim lets `admin import drat <url> -channel x`
// and `admin import drat -channel x <url>` both work.
//
// The heuristic is intentionally small: a token is a flag if it starts
// with "-". If that flag lacks "=" and takes a value, the next token
// is consumed as its value — we can't know which flags take values
// without consulting the FlagSet, so we peek at whether the next token
// starts with "-" and only consume non-flag tokens as values. The net
// effect: `-channel dev -branch main URL` round-trips safely; the
// pathological `-channel -branch` (missing value) fails later in
// fs.Parse, same as native stdlib behavior.
func reorderFlagsFirst(args []string) []string {
	flags := []string{}
	positional := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// If this is "-flag" (no "=") and the next token isn't itself
		// a flag, treat the next token as the value.
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

// adminUsageError wraps a formatted message so callers can distinguish
// user-input problems from operational ones. Today it's plain error
// but future refactors can branch on a custom type if we want a
// different exit code.
func adminUsageError(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s\n\n%s", msg, strings.TrimSpace(adminUsageText))
}

const adminUsageText = `usage:
  pakman-server admin [-data DIR] [-config PATH] <verb> [args…]

verbs:
  import drat <repo-url> -channel <name>
  import git  <repo-url> [-branch <b>] -channel <name>`
