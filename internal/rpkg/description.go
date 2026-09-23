// Package rpkg reads metadata out of R package tarballs.
//
// packyard stores tarballs as opaque CAS blobs; the only thing it
// needs from inside them is the DESCRIPTION file, whose dependency
// fields have to appear in PACKAGES for install.packages() to pull in
// a package's dependencies, and whose Built field marks a binary.
package rpkg

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxDescriptionBytes bounds how much of a DESCRIPTION file is read.
// Real DESCRIPTION files are a few KiB; the cap keeps a hostile
// tarball from making the server buffer an arbitrarily large entry.
const maxDescriptionBytes = 1 << 20

// ErrNoDescription is returned when a tarball has no <pkg>/DESCRIPTION.
var ErrNoDescription = errors.New("tarball has no DESCRIPTION")

// IndexFields are the DESCRIPTION fields CRAN-like repositories carry
// in PACKAGES besides Package and Version — the same set as R's
// tools:::.get_standard_repository_db_fields(), minus MD5sum.
var IndexFields = []string{
	"Priority",
	"Depends",
	"Imports",
	"LinkingTo",
	"Suggests",
	"Enhances",
	"License",
	"License_is_FOSS",
	"License_restricts_use",
	"OS_type",
	"Archs",
	"NeedsCompilation",
}

// ReadDescription returns the parsed DESCRIPTION of package pkg from
// a gzipped tarball (source or binary build — both keep it at
// <pkg>/DESCRIPTION). Field values are whitespace-normalized the way
// R's read.dcf() does for non-Description fields: continuation lines
// are joined with single spaces.
func ReadDescription(r io.Reader, pkg string) (map[string]string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	want := pkg + "/DESCRIPTION"
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoDescription
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if strings.TrimPrefix(hdr.Name, "./") != want || hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxDescriptionBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read DESCRIPTION: %w", err)
		}
		if len(body) > maxDescriptionBytes {
			return nil, fmt.Errorf("DESCRIPTION exceeds %d bytes", maxDescriptionBytes)
		}
		return ParseDCF(string(body))
	}
}

// ParseDCF parses a single Debian-control-file record. Lines starting
// with whitespace continue the previous field; blank lines are
// ignored.
func ParseDCF(s string) (map[string]string, error) {
	out := map[string]string{}
	var key string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), maxDescriptionBytes)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key == "" {
				return nil, errors.New("DCF continuation line before any field")
			}
			out[key] = joinValue(out[key], strings.TrimSpace(line))
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("malformed DCF line %q", line)
		}
		key = k
		out[key] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func joinValue(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

// SelectIndexFields returns the subset of desc that belongs in a
// PACKAGES stanza, keyed by field name. Empty values are dropped.
func SelectIndexFields(desc map[string]string) map[string]string {
	out := map[string]string{}
	for _, f := range IndexFields {
		if v := desc[f]; v != "" {
			out[f] = v
		}
	}
	return out
}
