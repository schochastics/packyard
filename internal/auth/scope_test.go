package auth_test

import (
	"testing"

	"github.com/schochastics/pakman/internal/auth"
)

func TestParseScopes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"admin", []string{"admin"}},
		{"publish:dev,publish:test,read:*", []string{"publish:dev", "publish:test", "read:*"}},
		{"  publish:dev , , read:*  ", []string{"publish:dev", "read:*"}},
		{"publish:dev,publish:dev", []string{"publish:dev"}}, // dedup
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := auth.ParseScopes(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v vs %v)", len(got), len(tc.want), got, tc.want)
			}
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("missing scope %q in %v", w, got)
				}
			}
		})
	}
}

func TestScopeSetHas(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		grants   string
		required string
		want     bool
	}{
		{"exact match", "publish:dev", "publish:dev", true},
		{"wrong channel", "publish:dev", "publish:prod", false},
		{"wildcard grants concrete", "publish:*", "publish:prod", true},
		{"wildcard doesn't cross verbs", "read:*", "publish:prod", false},
		{"admin doesn't imply publish", "admin", "publish:prod", false},
		{"admin matches itself", "admin", "admin", true},
		{"wildcard matches wildcard", "publish:*", "publish:*", true},
		{"empty grants", "", "publish:prod", false},
		{"multiple grants include match", "read:*,publish:dev,admin", "publish:dev", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set := auth.ParseScopes(tc.grants)
			if got := set.Has(tc.required); got != tc.want {
				t.Errorf("Has(%q, %q) = %v, want %v", tc.grants, tc.required, got, tc.want)
			}
		})
	}
}

func TestScopeSetCSVIsStable(t *testing.T) {
	t.Parallel()

	a := auth.ParseScopes("publish:dev,read:*,admin").CSV()
	b := auth.ParseScopes("read:*,admin,publish:dev").CSV()
	if a != b {
		t.Errorf("CSV order unstable: %q vs %q", a, b)
	}
	// Sorted lexicographically.
	want := "admin,publish:dev,read:*"
	if a != want {
		t.Errorf("CSV = %q, want %q", a, want)
	}
}
