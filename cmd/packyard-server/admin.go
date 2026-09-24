package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
)

// adminMain is the entry point for `packyard-server admin …`. Kept out
// of main.go so the imperative shell there stays small.
//
// Top-level grammar:
//
//	packyard-server admin [-data DIR] [-config PATH] <verb> [args…]
//
// where <verb> is one of:
//
//	import drat   <repo-url>      -channel <name>
//	import git    <repo-url>      [-branch <b>] -channel <name>
//	import bundle <path-or-targz> -channel <name>
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
	warnIgnoredData(*configPath, flagSet(fs, "data"))

	cfg, err := resolveConfig(*configPath, *dataDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	switch rest[0] {
	case "import":
		return adminImport(cfg, rest[1:])
	case "channels":
		return adminChannels(cfg, rest[1:])
	case "cells":
		return adminCells(cfg, rest[1:])
	case "gc":
		return adminGC(cfg, rest[1:])
	case "reindex":
		return adminReindex(cfg, rest[1:])
	case "missing-binaries":
		return adminMissingBinaries(cfg, rest[1:])
	case "backup":
		return adminBackup(cfg, *configPath, rest[1:])
	case "restore":
		return adminRestore(cfg, rest[1:])
	case "token-gen":
		return adminTokenGen(rest[1:])
	default:
		return adminUsageError("admin: unknown verb %q", rest[0])
	}
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

	// Loaded so writes refuse channels removed from channels.yaml, as
	// the server does. Kind (proxy or local) comes from the DB either way.
	channels, err := config.LoadChannels(cfg.ChannelsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: channels: %v\n", err)
		channels = nil
	}

	database, err := openExistingDB(cfg)
	if err != nil {
		return api.Deps{}, nil, err
	}
	store, err := cas.New(filepath.Join(cfg.DataDir, "cas"))
	if err != nil {
		_ = database.Close()
		return api.Deps{}, nil, fmt.Errorf("cas: %w", err)
	}

	cleanup := func() { _ = database.Close() }
	return api.Deps{DB: database, CAS: store, Matrix: matrix, Channels: channels, Server: cfg}, cleanup, nil
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
  packyard-server admin [-data DIR] [-config PATH] <verb> [args…]

verbs:
  import drat   <repo-url>      -channel <name>
  import git    <repo-url>      [-branch <b>] -channel <name>
  import bundle <path-or-targz> -channel <name>
  channels list
  cells list
  cells show <cell-name>
  missing-binaries -channel <name> [-cell <cell>]
  gc [-dry-run]
  reindex
  backup -out <dir> | -verify <dir>
  restore -from <dir> [-data <dir>] [-force]
  token-gen`

// newTabWriter produces a stdout-backed writer with consistent column
// padding for every admin subcommand.
func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

// humanBytes renders n in the closest IEC unit for CLI output. The UI
// has its own fmtBytes; duplicated here rather than exported because
// the call sites are tiny and the dependency direction (cmd -> ui)
// would be backwards.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	v := float64(n) / float64(div)
	if v == float64(int(v)) {
		return strconv.FormatInt(int64(v), 10) + " " + units[exp]
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return s + " " + units[exp]
}
