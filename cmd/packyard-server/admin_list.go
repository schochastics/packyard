package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/config"
)

// adminMissingBinaries lists, for the current version of every package
// on a channel, the matrix cells that still have no binary — the
// work list for a backfill after a new R version is added to
// matrix.yaml. Same query as GET /api/v1/channels/{c}/missing-binaries.
func adminMissingBinaries(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin missing-binaries", flag.ContinueOnError)
	channel := fs.String("channel", "", "channel to inspect (required)")
	cell := fs.String("cell", "", "restrict to one cell")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel == "" {
		return adminUsageError("admin missing-binaries: -channel is required")
	}
	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	missing, err := api.MissingBinaries(context.Background(), deps, *channel, *cell)
	if err != nil {
		return err
	}
	tw := newTabWriter()
	fmt.Fprintln(tw, "PACKAGE\tVERSION\tCELL")
	for _, m := range missing {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, m.Version, m.Cell)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d missing\n", len(missing))
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

	fmt.Printf("distro %s (%s), default R %s\n\n", deps.Matrix.Distro, deps.Matrix.Arch, deps.Matrix.DefaultRMinor)
	tw := newTabWriter()
	fmt.Fprintln(tw, "CELL\tR\tBINARIES\tCOVERAGE\tSIZE")
	for _, c := range deps.Matrix.Cells {
		a := agg[c.Name]
		coverage := "—"
		if total > 0 {
			coverage = fmt.Sprintf("%d/%d", a.PkgCount, total)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			c.Name, c.RMinor, a.BinCount, coverage, humanBytes(a.Bytes))
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

	fmt.Printf("cell %s\n  distro %s\n  arch   %s\n  r      %s\n\n",
		cell.Name, deps.Matrix.Distro, deps.Matrix.Arch, cell.RMinor)

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
