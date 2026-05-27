package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty input", nil, []string{}},
		{"lowercase + trim", []string{"  Essay ", "BUGFIX"}, []string{"essay", "bugfix"}},
		{"strip leading hash", []string{"#thread", "##idea"}, []string{"thread", "#idea"}},
		{"dedup", []string{"a", "A", "  a  "}, []string{"a"}},
		{"drop empty", []string{"", "  ", "x"}, []string{"x"}},
		{"max 32 chars truncates rejected", []string{"abcdefghijabcdefghijabcdefghijabcXTRA"}, []string{"abcdefghijabcdefghijabcdefghijab"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTags(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeriveTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"\n\n  hello\nworld", "hello"},
		{"first line is long " + strings.Repeat("x", 100), ("first line is long " + strings.Repeat("x", 100))[:80]},
	}
	for _, tc := range cases {
		if got := DeriveTitle(tc.in); got != tc.want {
			t.Errorf("DeriveTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
