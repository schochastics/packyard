package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/store"
)

// adminReindex verifies that every sha256 referenced by the DB has a
// matching blob in CAS. It's the v1 answer to "rebuild the indices"
// from implementation.md §B7 — packyard never persists a PACKAGES file,
// so the meaningful recovery op after a DB/CAS restore is this
// consistency check, not a literal rebuild.
//
// Missing blobs are reported to stdout with their (channel, pkg,
// version, which-column) so an operator can decide whether to restore
// from backup, delete the row, or republish via CI.
//
// In-memory PACKAGES cache (internal/api.Index) regenerates lazily on
// the next request; there is no persistent cache to evict.
func adminReindex(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin reindex", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return adminUsageError("admin reindex: no positional arguments expected")
	}

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	missing, err := verifyBlobs(deps)
	if err != nil {
		return err
	}

	bf, err := store.New(deps.DB.DB, deps.CAS).BackfillMetadata(context.Background())
	if err != nil {
		return err
	}

	fmt.Println("packyard computes PACKAGES on demand; there is no on-disk index to rebuild.")
	fmt.Printf("backfilled DESCRIPTION metadata: packages=%d binaries=%d\n", bf.Packages, bf.Binaries)
	fmt.Printf("verified DB -> CAS references. missing blobs: %d\n", len(missing))
	if len(missing) > 0 {
		tw := newTabWriter()
		fmt.Fprintln(tw, "CHANNEL\tPACKAGE\tVERSION\tCOLUMN\tSHA256")
		for _, m := range missing {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Channel, m.Package, m.Version, m.Column, m.SHA256)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		return fmt.Errorf("%d blob references missing from CAS", len(missing))
	}
	return nil
}

// missingBlob is one DB row whose blob is not present in CAS.
type missingBlob struct {
	Channel, Package, Version, Column, SHA256 string
}

// verifyBlobs walks every package and binary row and checks each sha256
// against cas.Has. Non-trivial repos will have O(10k) rows; a single
// cas.Has is a cheap os.Stat so we don't bother batching.
func verifyBlobs(deps api.Deps) ([]missingBlob, error) {
	var missing []missingBlob

	// Source blobs.
	srcRows, err := deps.DB.QueryContext(context.Background(), `
		SELECT channel, name, version, source_sha256 FROM packages
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = srcRows.Close() }()
	for srcRows.Next() {
		var ch, name, ver, sum string
		if err := srcRows.Scan(&ch, &name, &ver, &sum); err != nil {
			return nil, err
		}
		if !deps.CAS.Has(sum) {
			missing = append(missing, missingBlob{ch, name, ver, "source", sum})
		}
	}
	if err := srcRows.Err(); err != nil {
		return nil, err
	}

	// Binary blobs. Join to packages for the human-readable columns;
	// querying binaries.cell would be more correct than "column" here,
	// but the CSV is easier to grep when the label matches "source".
	binRows, err := deps.DB.QueryContext(context.Background(), `
		SELECT p.channel, p.name, p.version, b.cell, b.binary_sha256
		FROM binaries b JOIN packages p ON p.id = b.package_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = binRows.Close() }()
	for binRows.Next() {
		var ch, name, ver, cell, sum string
		if err := binRows.Scan(&ch, &name, &ver, &cell, &sum); err != nil {
			return nil, err
		}
		if !deps.CAS.Has(sum) {
			missing = append(missing, missingBlob{ch, name, ver, "binary/" + cell, sum})
		}
	}
	return missing, binRows.Err()
}

// adminGC reclaims CAS blobs that no longer appear in any package or
// binary row. Safe to run against a live server: blobs younger than
// -min-age are kept, which covers publishes still uploading.
//
// Output reports scanned / removed / freed bytes so an operator can
// tell at a glance whether the run did anything.
func adminGC(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin gc", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would be removed; do not actually delete")
	minAge := fs.Duration("min-age", time.Hour, "keep unreferenced blobs and temp files newer than this (protects in-flight publishes)")
	force := fs.Bool("force", false, "collect even when the DB references no blobs at all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return adminUsageError("admin gc: no positional arguments expected")
	}
	if *minAge < 0 {
		return adminUsageError("admin gc: -min-age must not be negative")
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

	// An empty live set over a non-empty CAS almost always means the
	// wrong DB (a fresh volume, a restored-over file), not a repository
	// whose every package was deleted. Collecting would wipe the store.
	if len(live) == 0 && !*force && !*dryRun {
		probe, err := deps.CAS.GC(live, cas.GCOptions{DryRun: true})
		if err != nil {
			return fmt.Errorf("gc: %w", err)
		}
		if probe.Scanned > 0 {
			return fmt.Errorf("gc: the DB references no blobs but the CAS holds %d; refusing to delete them all (check -data, or pass -force)", probe.Scanned)
		}
	}

	opts := cas.GCOptions{MinAge: *minAge, DryRun: *dryRun}
	if *dryRun {
		opts.OnRemove = func(sum string, size int64) {
			fmt.Printf("  would remove: %s (%s)\n", sum, humanBytes(size))
		}
	}
	report, err := deps.CAS.GC(live, opts)
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	prefix := ""
	if *dryRun {
		prefix = "DRY RUN — "
	}
	fmt.Printf("%sscanned=%d removed=%d freed=%s skipped_young=%d skipped_stray=%d tmp_removed=%d\n",
		prefix, report.Scanned, report.Removed, humanBytes(report.FreedBytes),
		report.SkippedYoung, report.SkippedStray, report.TmpRemoved)
	return nil
}

// liveBlobSet returns every sha256 referenced by a package or binary
// row (see db.ReferencedBlobs; yanked packages count as live).
func liveBlobSet(deps api.Deps) (map[string]struct{}, error) {
	sums, err := db.ReferencedBlobs(context.Background(), deps.DB.DB)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(sums))
	for _, s := range sums {
		out[s] = struct{}{}
	}
	return out, nil
}
