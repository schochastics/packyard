package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

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
	case "channels":
		return adminChannels(cfg, rest[1:])
	case "cells":
		return adminCells(cfg, rest[1:])
	case "gc":
		return adminGC(cfg, rest[1:])
	default:
		return adminUsageError("admin: unknown verb %q", rest[0])
	}
}

// adminGC reclaims CAS blobs that no longer appear in any package or
// binary row. Safe to run while the server is stopped; running against
// a live server is a known-sharp-edge op — see the cas.GC doc comment.
//
// Output reports scanned / removed / freed bytes so an operator can
// tell at a glance whether the run did anything.
func adminGC(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin gc", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would be removed; do not actually delete")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return adminUsageError("admin gc: no positional arguments expected")
	}

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	live, err := liveBlobSet(deps)
	if err != nil {
		return fmt.Errorf("build live set: %w", err)
	}
	fmt.Printf("live blobs referenced by DB: %d\n", len(live))

	if *dryRun {
		// Dry run: walk what GC would scan and report, but don't delete.
		// Swap live with a dummy that claims every blob is live, so GC
		// walks without removing anything, then re-walk ourselves to
		// compute the count. Simpler: duplicate the small bit of walk
		// logic here with a Has check.
		return dryRunGC(deps, live)
	}

	report, err := deps.CAS.GC(live)
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	fmt.Printf("scanned=%d removed=%d freed=%s skipped_stray=%d\n",
		report.Scanned, report.Removed, humanBytes(report.FreedBytes), report.SkippedStray)
	return nil
}

// liveBlobSet returns the union of every sha256 referenced by a live
// package or binary row. Yanked packages are INCLUDED — yanking is a
// visibility operation; the blob is still reachable via the audit log
// and admins can unyank.
func liveBlobSet(deps api.Deps) (map[string]struct{}, error) {
	out := map[string]struct{}{}

	collect := func(query string) error {
		rows, err := deps.DB.QueryContext(context.Background(), query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var sum string
			if err := rows.Scan(&sum); err != nil {
				return err
			}
			out[sum] = struct{}{}
		}
		return rows.Err()
	}

	if err := collect(`SELECT DISTINCT source_sha256 FROM packages`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT DISTINCT binary_sha256 FROM binaries`); err != nil {
		return nil, err
	}
	return out, nil
}

// dryRunGC reports what a real GC would remove, without deleting. No
// new CAS API needed: we use cas.Has to check each filesystem blob
// that isn't in live. Walks via filepath.Walk for simplicity since a
// dry-run doesn't need the subtree-skip logic GC has for tmp/.
func dryRunGC(deps api.Deps, live map[string]struct{}) error {
	root := deps.CAS.Root()
	var scanned, wouldRemove int
	var wouldFree int64

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if filepath.Base(path) == "tmp" && filepath.Dir(path) == root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// <aa>/<rest> shape — reuse cas' own check indirectly by
		// reassembling sha.
		shard, file := filepath.Split(rel)
		shard = strings.TrimSuffix(shard, string(filepath.Separator))
		if len(shard) != 2 || len(file) != 62 {
			return nil
		}
		sum := shard + file
		scanned++
		if _, ok := live[sum]; !ok {
			wouldRemove++
			wouldFree += info.Size()
			fmt.Printf("  would remove: %s (%s)\n", sum, humanBytes(info.Size()))
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("DRY RUN — scanned=%d would_remove=%d would_free=%s\n",
		scanned, wouldRemove, humanBytes(wouldFree))
	return nil
}

func adminChannels(cfg *config.ServerConfig, args []string) error {
	if len(args) == 0 {
		return adminUsageError("admin channels: missing subverb (list)")
	}
	switch args[0] {
	case "list":
		return adminChannelsList(cfg)
	default:
		return adminUsageError("admin channels: unknown subverb %q", args[0])
	}
}

func adminCells(cfg *config.ServerConfig, args []string) error {
	if len(args) == 0 {
		return adminUsageError("admin cells: missing subverb (list|show)")
	}
	switch args[0] {
	case "list":
		return adminCellsList(cfg)
	case "show":
		return adminCellsShow(cfg, args[1:])
	default:
		return adminUsageError("admin cells: unknown subverb %q", args[0])
	}
}

// adminChannelsList prints every channel's name, overwrite policy,
// default-flag, package count, and most-recent publish timestamp.
// Mirrors the JSON shape of GET /api/v1/channels but as an aligned
// text table.
func adminChannelsList(cfg *config.ServerConfig) error {
	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := deps.DB.QueryContext(context.Background(), `
		SELECT c.name, c.overwrite_policy, c.is_default,
		       COUNT(p.id) AS pkg_count,
		       COALESCE(MAX(p.published_at), '') AS latest
		FROM channels c
		LEFT JOIN packages p ON p.channel = c.name
		GROUP BY c.name, c.overwrite_policy, c.is_default
		ORDER BY c.is_default DESC, c.name
	`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	tw := newTabWriter()
	fmt.Fprintln(tw, "NAME\tPOLICY\tDEFAULT\tPACKAGES\tLATEST PUBLISH")
	for rows.Next() {
		var (
			name, policy, latest string
			isDefault            int
			count                int64
		)
		if err := rows.Scan(&name, &policy, &isDefault, &count, &latest); err != nil {
			return err
		}
		def := ""
		if isDefault == 1 {
			def = "yes"
		}
		if latest == "" {
			latest = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", name, policy, def, count, latest)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

// adminCellsList prints every cell declared in matrix.yaml with its
// coverage (how many of the total packages have a binary for the cell)
// and total bytes uploaded.
func adminCellsList(cfg *config.ServerConfig) error {
	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if deps.Matrix == nil {
		return fmt.Errorf("matrix.yaml not loaded; see earlier warning")
	}

	var total int64
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages`).Scan(&total); err != nil {
		return err
	}

	// Aggregate per cell.
	agg := map[string]struct {
		BinCount, PkgCount, Bytes int64
	}{}
	rows, err := deps.DB.QueryContext(context.Background(), `
		SELECT cell, COUNT(*), COUNT(DISTINCT package_id), COALESCE(SUM(size), 0)
		FROM binaries GROUP BY cell
	`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cell string
		var binCount, pkgCount, bytes int64
		if err := rows.Scan(&cell, &binCount, &pkgCount, &bytes); err != nil {
			return err
		}
		agg[cell] = struct {
			BinCount, PkgCount, Bytes int64
		}{binCount, pkgCount, bytes}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tw := newTabWriter()
	fmt.Fprintln(tw, "CELL\tOS\tARCH\tR\tBINARIES\tCOVERAGE\tSIZE")
	for _, c := range deps.Matrix.Cells {
		a := agg[c.Name]
		coverage := "—"
		if total > 0 {
			coverage = fmt.Sprintf("%d/%d", a.PkgCount, total)
		}
		fmt.Fprintf(tw, "%s\t%s %s\t%s\t%s\t%d\t%s\t%s\n",
			c.Name, c.OS, c.OSVersion, c.Arch, c.RMinor,
			a.BinCount, coverage, humanBytes(a.Bytes))
	}
	return tw.Flush()
}

// adminCellsShow prints the matrix entry for a single cell and lists
// packages that have NO binary for that cell (the coverage gap). Useful
// during a cell rollout — tells the operator exactly which packages
// still need a build targeting the new cell.
func adminCellsShow(cfg *config.ServerConfig, args []string) error {
	if len(args) != 1 {
		return adminUsageError("admin cells show: expected exactly one <cell-name> argument")
	}
	cellName := args[0]

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if deps.Matrix == nil {
		return fmt.Errorf("matrix.yaml not loaded")
	}
	cell := deps.Matrix.Lookup(cellName)
	if cell == nil {
		return fmt.Errorf("cell %q not declared in matrix.yaml", cellName)
	}

	fmt.Printf("cell %s\n  os     %s %s\n  arch   %s\n  r      %s\n\n",
		cell.Name, cell.OS, cell.OSVersion, cell.Arch, cell.RMinor)

	// Packages missing a binary for this cell. A LEFT JOIN + NULL filter
	// keeps this to one query.
	rows, err := deps.DB.QueryContext(context.Background(), `
		SELECT p.channel, p.name, p.version, p.published_at
		FROM packages p
		LEFT JOIN binaries b ON b.package_id = p.id AND b.cell = ?
		WHERE b.id IS NULL AND p.yanked = 0
		ORDER BY p.channel, p.name, p.version
	`, cellName)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	tw := newTabWriter()
	fmt.Fprintln(tw, "CHANNEL\tPACKAGE\tVERSION\tPUBLISHED")
	any := false
	for rows.Next() {
		var ch, name, ver, pub string
		if err := rows.Scan(&ch, &name, &ver, &pub); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ch, name, ver, pub)
		any = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !any {
		fmt.Println("all live packages have a binary for this cell.")
	}
	return nil
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
  import git  <repo-url> [-branch <b>] -channel <name>
  channels list
  cells list
  cells show <cell-name>
  gc [-dry-run]`

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
