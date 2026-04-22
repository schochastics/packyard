// Package importers provides tools for backfilling a pakman channel
// from existing R package sources. v1 supports drat (HTTP) and git;
// both importers publish source tarballs only — binaries are a CI
// concern, not an import one.
package importers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/schochastics/pakman/internal/api"
)

// DratImporter walks a drat (or CRAN-shaped) repo over HTTP, fetching
// each source tarball listed in /src/contrib/PACKAGES and handing it
// to api.ImportSource. Zero value is not useful — construct with
// NewDratImporter so the HTTP client, target channel, and deps are
// explicit.
type DratImporter struct {
	Deps    api.Deps
	Client  *http.Client // nil -> stdlib default with a 5m timeout
	Channel string       // pakman channel to land imports in
	Actor   string       // event.actor tag; defaults to "import-drat"
}

// DratResult is the outcome of one DratImporter.Run.
type DratResult struct {
	Imported []string // "<pkg>@<ver>" of packages newly created
	Skipped  []string // already-existing entries (idempotent replays)
	Failed   []DratFailure
}

// DratFailure is one package that couldn't be imported.
type DratFailure struct {
	Package string
	Version string
	Err     error
}

// NewDratImporter fills in sane defaults around an api.Deps + target
// channel. The returned importer can be reused for multiple Run calls
// (eg. to re-scan a repo on a schedule).
func NewDratImporter(deps api.Deps, channel string) *DratImporter {
	return &DratImporter{
		Deps:    deps,
		Client:  &http.Client{Timeout: 5 * time.Minute},
		Channel: channel,
		Actor:   "import-drat",
	}
}

// Run fetches repoURL/src/contrib/PACKAGES, then downloads each listed
// tarball and imports it. Non-fatal per-package failures go into
// result.Failed so one broken tarball doesn't abort a 500-package
// backfill. A fatal error (unreachable PACKAGES, auth failure) is
// returned directly.
//
// progress is optional; when non-nil it's called before each package
// import with a short status line suitable for CLI output.
func (d *DratImporter) Run(ctx context.Context, repoURL string, progress func(string)) (*DratResult, error) {
	base, err := url.Parse(strings.TrimRight(repoURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse repo URL: %w", err)
	}
	pkgsURL := base.JoinPath("src", "contrib", "PACKAGES").String()
	entries, err := d.fetchPackagesIndex(ctx, pkgsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pkgsURL, err)
	}

	res := &DratResult{}
	for _, e := range entries {
		tag := e.Package + "@" + e.Version
		if progress != nil {
			progress("importing " + tag)
		}

		tarURL := base.JoinPath("src", "contrib", e.tarballFilename()).String()
		resp, err := d.importOne(ctx, tarURL, e)
		if err != nil {
			res.Failed = append(res.Failed, DratFailure{Package: e.Package, Version: e.Version, Err: err})
			if progress != nil {
				progress("  failed: " + err.Error())
			}
			continue
		}
		switch {
		case resp.AlreadyExisted:
			res.Skipped = append(res.Skipped, tag)
			if progress != nil {
				progress("  skipped (already present)")
			}
		default:
			res.Imported = append(res.Imported, tag)
		}
	}
	return res, nil
}

// importOne GETs the tarball URL and streams it straight into
// api.ImportSource so nothing is buffered in memory beyond CAS.
func (d *DratImporter) importOne(ctx context.Context, tarURL string, e packagesEntry) (*api.PublishResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tarURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", tarURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: HTTP %d", tarURL, resp.StatusCode)
	}

	return api.ImportSource(ctx, d.Deps, api.ImportInput{
		Channel: d.Channel,
		Name:    e.Package,
		Version: e.Version,
		Source:  resp.Body,
		Actor:   d.Actor,
		Note:    tarURL,
	})
}

// fetchPackagesIndex GETs <base>/src/contrib/PACKAGES and parses the
// DCF. Returns one packagesEntry per stanza. Fields beyond Package /
// Version are ignored in v1 — we don't use dependency metadata during
// import.
func (d *DratImporter) fetchPackagesIndex(ctx context.Context, pkgsURL string) ([]packagesEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pkgsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parsePackagesDCF(resp.Body)
}

// packagesEntry is the minimum pakman cares about per PACKAGES stanza.
type packagesEntry struct {
	Package string
	Version string
}

func (e packagesEntry) tarballFilename() string {
	return e.Package + "_" + e.Version + ".tar.gz"
}

// parsePackagesDCF reads a PACKAGES-format body and returns one entry
// per stanza. Handles blank-line stanza separators; continuation lines
// (leading whitespace) are ignored since we only need Package+Version
// today — if/when we care about Depends etc. this grows a fold step.
func parsePackagesDCF(r io.Reader) ([]packagesEntry, error) {
	out := []packagesEntry{}
	cur := packagesEntry{}
	flush := func() {
		if cur.Package != "" && cur.Version != "" {
			out = append(out, cur)
		}
		cur = packagesEntry{}
	}

	sc := bufio.NewScanner(r)
	// PACKAGES can be large — a single entry with a long list of
	// dependencies may exceed the default 64 KiB scanner line buffer.
	// Bump to 1 MiB; anything over that is malformed.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			// Continuation — we don't decode these for v1.
			continue
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "Package":
			cur.Package = val
		case "Version":
			cur.Version = val
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("PACKAGES contained no entries")
	}
	return out, nil
}
