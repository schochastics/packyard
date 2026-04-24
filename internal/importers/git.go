package importers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/schochastics/packyard/internal/api"
)

// GitImporter clones an R-package git repo, runs `R CMD build`, and
// imports the resulting source tarball. Unlike drat imports this needs
// local git + R binaries on PATH.
//
// The Clone and Build hooks are fields (not methods) so tests can
// substitute lightweight stand-ins — the default implementations shell
// out to real binaries, which CI for packyard itself does not carry.
type GitImporter struct {
	Deps    api.Deps
	Channel string
	Actor   string // event.actor; defaults to "import-git"

	// Clone fetches repoURL at branch into dest. Override to e.g. fake
	// a working tree for tests.
	Clone func(ctx context.Context, repoURL, branch, dest string) error

	// Build runs R CMD build in sourceDir and returns the path of the
	// produced tarball. Override for tests that don't have R.
	Build func(ctx context.Context, sourceDir string) (string, error)
}

// NewGitImporter wires the default shell-out implementations.
func NewGitImporter(deps api.Deps, channel string) *GitImporter {
	return &GitImporter{
		Deps:    deps,
		Channel: channel,
		Actor:   "import-git",
		Clone:   gitClone,
		Build:   rCmdBuild,
	}
}

// GitResult reports the one package published from a git clone.
type GitResult struct {
	Package  string
	Version  string
	Response *api.PublishResponse
}

// Run clones repoURL@branch into a scratch dir, runs R CMD build,
// parses the resulting tarball name to recover (name, version), and
// imports it. The scratch dir is removed after the call completes.
func (g *GitImporter) Run(ctx context.Context, repoURL, branch string, progress func(string)) (*GitResult, error) {
	workdir, err := os.MkdirTemp("", "packyard-import-git-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	checkout := filepath.Join(workdir, "src")
	if progress != nil {
		progress("cloning " + repoURL + "@" + branch)
	}
	if err := g.Clone(ctx, repoURL, branch, checkout); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}

	// DESCRIPTION at the repo root is the canonical source for
	// (name, version). We read it before R CMD build so we can report
	// the package we're about to import even if the build fails.
	name, version, err := readDescription(filepath.Join(checkout, "DESCRIPTION"))
	if err != nil {
		return nil, fmt.Errorf("parse DESCRIPTION: %w", err)
	}

	if progress != nil {
		progress("R CMD build " + name + " " + version)
	}
	tarPath, err := g.Build(ctx, checkout)
	if err != nil {
		return nil, fmt.Errorf("R CMD build: %w", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("open tarball: %w", err)
	}
	defer func() { _ = f.Close() }()

	resp, err := api.ImportSource(ctx, g.Deps, api.ImportInput{
		Channel: g.Channel,
		Name:    name,
		Version: version,
		Source:  f,
		Actor:   g.Actor,
		Note:    repoURL + "@" + branch,
	})
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}

	return &GitResult{Package: name, Version: version, Response: resp}, nil
}

// gitClone is the default Clone implementation. Shallow clone to keep
// the scratch dir small — we never need git history to import.
func gitClone(ctx context.Context, repoURL, branch, dest string) error {
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repoURL, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stderr // clone noise on stderr so stdout stays for progress
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// rCmdBuild shells out to R CMD build and returns the produced tarball
// path. R writes the tarball into the CWD — we run in a temp dir and
// then look for the single *.tar.gz.
func rCmdBuild(ctx context.Context, sourceDir string) (string, error) {
	outDir, err := os.MkdirTemp("", "packyard-R-build-*")
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "R", "CMD", "build", sourceDir)
	cmd.Dir = outDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("R CMD build: %w", err)
	}

	matches, err := filepath.Glob(filepath.Join(outDir, "*.tar.gz"))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("R CMD build produced %d tarballs; expected exactly 1", len(matches))
	}
	return matches[0], nil
}

// readDescription parses an R package's DESCRIPTION file and returns
// (Package, Version). Anything else in the file is ignored.
func readDescription(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	return parseDescriptionStream(f)
}

func parseDescriptionStream(r io.Reader) (string, string, error) {
	var name, version string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // continuation
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "Package":
			name = val
		case "Version":
			version = val
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	if name == "" {
		return "", "", errors.New("DESCRIPTION has no Package field")
	}
	if version == "" {
		return "", "", errors.New("DESCRIPTION has no Version field")
	}
	return name, version, nil
}
