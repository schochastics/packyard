package rversion

import (
	"errors"
	"testing"
)

// Expected results in this file were checked against R 4.5.3's
// package_version.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1},
		{"2.1.16", "2.1.9", 1},
		{"1.0-1", "1.0.2", -1},
		{"1.0-10", "1.0-9", 1},
		{"1.0-1", "1.0.1", 0},
		{"1.2.3-4", "1.2.3.4", 0},
		{"1.0", "1.0.0", 0},
		{"1.0", "1.0.0.0", 0},
		{"1.0.0", "1.0.0.1", -1},
		{"1.01", "1.1", 0},
		{"1.0", "1.0.1", -1},
		{"0.9.9", "1.0", -1},
		{"10.0", "9.99", 1},
	}
	for _, c := range cases {
		va, err := Parse(c.a)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.a, err)
		}
		vb, err := Parse(c.b)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.b, err)
		}
		if got := va.Compare(vb); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := vb.Compare(va); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	// Every one of these is rejected by R's package_version.
	for _, s := range []string{"", "1", "1.", ".1", "1..0", "1.-1", " 1.0", "1.0 ", "1.0a", "v1.0", "1.0_1"} {
		if _, err := Parse(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
}

func TestParseKeepsRaw(t *testing.T) {
	v, err := Parse("1.0-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "1.0-1" {
		t.Errorf("String() = %q", v.String())
	}
}

func TestCompareStringsWithInvalid(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"bogus", "1.0", -1},
		{"1.0", "bogus", 1},
		{"a", "b", -1},
		{"b", "b", 0},
		{"1.10", "1.9", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
