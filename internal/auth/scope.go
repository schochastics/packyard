// Package auth holds pakman's token and scope primitives.
//
// Scopes are strings of the form "verb:target", e.g.
//
//	publish:prod    — publish to the prod channel
//	read:dev        — read from the dev channel
//	yank:*          — yank on any channel
//	admin           — management endpoints (no colon)
//
// The Wildcard constant is "*" and only matches within a single verb,
// never across verbs. admin is its own scope — not "admin:*" — to keep
// the most common production grants short.
package auth

import (
	"strings"
)

// Wildcard matches any target within a given verb, e.g. publish:*.
const Wildcard = "*"

// ScopeAdmin is the single all-powerful scope guarding admin endpoints.
// It does not imply any other scope; an "admin" token cannot publish
// unless it also has a publish:* (or publish:<channel>) grant. This
// keeps least-privilege tooling simple.
const ScopeAdmin = "admin"

// ScopeSet is the parsed form of a scopes_csv string from the tokens
// table. Duplicates are deduped on parse; order is not preserved.
type ScopeSet map[string]struct{}

// ParseScopes parses a CSV string from the DB's scopes_csv column into
// a ScopeSet. Empty entries (double commas, leading/trailing whitespace)
// are silently dropped.
func ParseScopes(csv string) ScopeSet {
	out := ScopeSet{}
	for _, raw := range strings.Split(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// CSV renders the set back to a scopes_csv value. Sorted for stable
// output — useful for logging and for round-tripping tests.
func (s ScopeSet) CSV() string {
	if len(s) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	// Sort in place via a small bubble (len is always tiny).
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return strings.Join(keys, ",")
}

// Has reports whether the set grants required. required must be a
// concrete scope — "publish:prod", never "publish:*". The wildcard
// form is valid only inside a held grant.
func (s ScopeSet) Has(required string) bool {
	if _, ok := s[required]; ok {
		return true
	}
	verb, target, ok := strings.Cut(required, ":")
	if !ok {
		// A scope with no colon (e.g. ScopeAdmin) either matches
		// exactly (handled above) or doesn't match at all.
		return false
	}
	_ = target
	if _, ok := s[verb+":"+Wildcard]; ok {
		return true
	}
	return false
}
