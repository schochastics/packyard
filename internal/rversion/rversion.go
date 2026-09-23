// Package rversion implements R's package_version ordering.
//
// R version strings are two or more non-negative integers separated
// by "." or "-" ("1.0", "2.1.16", "1.0-1"). Components compare
// numerically, so "1.10.0" > "1.9.0"; missing trailing components
// count as zero, so "1.0" == "1.0.0". Both rules were checked against
// R 4.5's package_version; packyard needs them to pick the latest
// version for PACKAGES and to order Meta/archive.rds, where a
// lexical sort gets "1.10.0" vs "1.9.0" wrong.
package rversion

import (
	"errors"
	"fmt"
	"strconv"
)

// Version is a parsed R package version.
type Version struct {
	raw   string
	parts []uint64
}

// ErrInvalid is returned by Parse for strings R would reject.
var ErrInvalid = errors.New("invalid R version")

// Parse parses s the way R's package_version does. It rejects
// anything R rejects: fewer than two components, empty components,
// signs, whitespace or non-digit characters.
func Parse(s string) (Version, error) {
	var parts []uint64
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '.' && s[i] != '-' {
			if s[i] < '0' || s[i] > '9' {
				return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
			}
			continue
		}
		if i == start {
			return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
		}
		n, err := strconv.ParseUint(s[start:i], 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
		}
		parts = append(parts, n)
		start = i + 1
	}
	if len(parts) < 2 {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	return Version{raw: s, parts: parts}, nil
}

// String returns the version exactly as it was parsed.
func (v Version) String() string { return v.raw }

// Compare returns -1, 0 or +1 as v is less than, equal to or greater
// than w.
func (v Version) Compare(w Version) int {
	n := max(len(v.parts), len(w.parts))
	for i := range n {
		a, b := part(v.parts, i), part(w.parts, i)
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
	}
	return 0
}

func part(p []uint64, i int) uint64 {
	if i < len(p) {
		return p[i]
	}
	return 0
}

// Compare parses a and b and compares them. Strings that fail to
// parse sort before every valid version and compare lexically among
// themselves, so callers ordering DB rows never lose a row to a
// malformed legacy version.
func Compare(a, b string) int {
	va, errA := Parse(a)
	vb, errB := Parse(b)
	switch {
	case errA == nil && errB == nil:
		return va.Compare(vb)
	case errA != nil && errB != nil:
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	case errA != nil:
		return -1
	default:
		return 1
	}
}
