package dbkit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFitColumnValue pins the write-boundary fit contract: valid UTF-8
// within the bound passes through byte-identical; anything else comes back
// valid UTF-8 and never longer than the bound in RUNES, with cut reporting
// only a genuine shortening.
func TestFitColumnValue(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
		wantCut  bool
	}{
		{"ascii within bound", "abc", 4, "abc", false},
		{"ascii exactly at bound", "abc", 3, "abc", false},
		{"empty value", "", 4, "", false},
		{"empty value, zero bound", "", 0, "", false},
		{"multibyte within bound by runes though past it by bytes", "€€€", 3, "€€€", false},
		{"rune cut, never a byte cut", strings.Repeat("€", 5), 3, strings.Repeat("€", 3), true},
		{"zero bound empties a non-empty value", "a", 0, "", true},
		{"invalid utf8 within bound is sanitized, not cut", "ok\xff", 100, "ok\uFFFD", false},
		{"consecutive invalid bytes collapse to one replacement rune", "a\xff\xffb", 100, "a\uFFFDb", false},
		{"sanitized first, then cut when still over the bound", "aaaa\xff", 4, "aaaa", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := FitColumnValue(tc.in, tc.maxRunes)
			if got != tc.want || cut != tc.wantCut {
				t.Errorf("FitColumnValue(%q, %d) = (%q, %v), want (%q, %v)", tc.in, tc.maxRunes, got, cut, tc.want, tc.wantCut)
			}
			if !utf8.ValidString(got) {
				t.Errorf("FitColumnValue(%q, %d) returned invalid UTF-8 %q", tc.in, tc.maxRunes, got)
			}
			if n := utf8.RuneCountInString(got); n > tc.maxRunes {
				t.Errorf("FitColumnValue(%q, %d) = %q: %d runes, over the bound", tc.in, tc.maxRunes, got, n)
			}
		})
	}
}
