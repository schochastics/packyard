package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/importers"
)

// adminImport routes `admin import <source> …`.
func adminImport(cfg *config.ServerConfig, args []string) error {
	if len(args) == 0 {
		return adminUsageError("admin import: missing source (drat|git|bundle)")
	}
	switch args[0] {
	case "drat":
		return adminImportDrat(cfg, args[1:])
	case "git":
		return adminImportGit(cfg, args[1:])
	case "bundle":
		return adminImportBundle(cfg, args[1:])
	default:
		return adminUsageError("admin import: unknown source %q (expected drat|git|bundle)", args[0])
	}
}

func adminImportDrat(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin import drat", flag.ContinueOnError)
	channel := fs.String("channel", "", "target packyard channel (required)")
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

// adminImportBundle imports a packyard-bundle/{1,2} bundle (directory
// or .tar.gz) into the named channel. The channel must already exist;
// we don't auto-create it because channels.yaml is the source of truth
// for overwrite policy. Each package goes through the store like a
// publish, so the channel's own policy applies: on an immutable
// channel a version already present with different bytes fails, on a
// mutable one it is overwritten. Air-gap snapshots belong in immutable
// channels; migrations may target any channel.
func adminImportBundle(cfg *config.ServerConfig, args []string) error {
	fs := flag.NewFlagSet("admin import bundle", flag.ContinueOnError)
	channel := fs.String("channel", "", "target packyard channel (required, must exist; its overwrite policy applies)")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return err
	}
	if *channel == "" {
		return adminUsageError("admin import bundle: -channel is required")
	}
	if fs.NArg() != 1 {
		return adminUsageError("admin import bundle: expected exactly one <path-or-targz> argument")
	}
	path := fs.Arg(0)

	deps, cleanup, err := openAdminDeps(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	imp := importers.NewBundleImporter(deps, *channel)
	res, err := imp.Run(context.Background(), path, func(line string) {
		fmt.Println(line)
	})
	if err != nil {
		return fmt.Errorf("bundle import: %w", err)
	}

	fmt.Printf("imported=%d skipped=%d failed=%d",
		len(res.Imported), len(res.Skipped), len(res.Failed))
	if res.Manifest != nil {
		fmt.Printf(" snapshot=%s kind=%s", res.Manifest.SnapshotID, res.Manifest.Kind)
		if res.Manifest.Cell != "" {
			fmt.Printf(" cell=%s", res.Manifest.Cell)
		}
	}
	fmt.Println()
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
	channel := fs.String("channel", "", "target packyard channel (required)")
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
