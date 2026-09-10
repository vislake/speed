package i18n

import (
	"reflect"
	"slices"
	"testing"
)

// TestNegotiate_Table drives the negotiation rules Negotiate's doc comment
// promises over the supported pairs the catalog actually ships: exact and
// language-prefix matches, case-insensitive tags, zero weights in every
// spelling the qvalue grammar allows, and header-order preference among
// non-zero weights. A miss answers ("", false) -- Negotiate has no default
// of its own.
func TestNegotiate_Table(t *testing.T) {
	supported := []string{LocaleENUS, LocaleZHCN}
	for _, tc := range []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"exact tag", "en-US", "en-US", true},
		{"uppercase exact tag", "EN-US", "en-US", true},
		{"lowercase exact tag", "zh-cn", "zh-CN", true},
		{"uppercase prefix", "EN", "en-US", true},
		{"lowercase tag with weight", "en-us;q=0.9", "en-US", true},
		{"prefix match answers the supported spelling", "zh", "zh-CN", true},
		{"second part matches", "fr-FR, zh-CN", "zh-CN", true},
		{"non-zero weights keep header order", "en-US;q=0.7, zh-CN;q=1", "en-US", true},
		{"zero weight then a weighted match", "fr-FR;q=0.9, en-US;q=0.0, zh-CN;q=0.5", "zh-CN", true},
		{"decimal zero weight skipped", "en;q=0.0, fr-FR", "", false},
		{"uppercase zero weight skipped", "en;Q=0, fr-FR", "", false},
		{"trailing decimal zero on the only match", "en-US;q=0.0", "", false},
		{"spaces around the weight", "en-US; q = 0.0", "", false},
		{"default when nothing is acceptable", "en;q=0, fr-FR;q=0.0", "", false},
		{"empty header", "", "", false},
		{"blank header", "   ", "", false},
		{"unsupported language only", "fr-FR", "", false},
		{"unsupported language with a lookalike prefix", "enx-XX", "", false},
		{"extension subtag does not match its parent", "en-US-x-private", "", false},
		{"wildcard is not a language", "*", "", false},
		{"empty parts skipped", " , en-GB ,, en-US", "en-US", true},
		{"weight on the first accepted part", "zh-CN;q=0.1,fr;q=1", "zh-CN", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Negotiate(tc.header, supported)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Negotiate(%q, %v) = (%q, %v), want (%q, %v)", tc.header, supported, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestNegotiate_SupportedSetShapes pins the boundary conditions of the
// supported list itself: no list at all and a single-language list both
// answer what they can, and the returned spelling is always the supported
// entry's own -- never the header's letter case.
func TestNegotiate_SupportedSetShapes(t *testing.T) {
	if got, ok := Negotiate("en-US", nil); got != "" || ok {
		t.Errorf("Negotiate with no supported set = (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := Negotiate("en-US", []string{}); got != "" || ok {
		t.Errorf("Negotiate with an empty supported set = (%q, %v), want (\"\", false)", got, ok)
	}
	got, ok := Negotiate("en-us", []string{"en-US"})
	if got != "en-US" || !ok {
		t.Errorf("Negotiate = (%q, %v), want the supported spelling (\"en-US\", true)", got, ok)
	}
	got, ok = Negotiate("zh-CN", []string{"en-US"})
	if got != "" || ok {
		t.Errorf("Negotiate of an unsupported language = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestNegotiate_PrefersHeaderOrderOverSupportedOrder pins that a header
// naming several supported languages answers the one it lists first, no
// matter how the supported set is ordered.
func TestNegotiate_PrefersHeaderOrderOverSupportedOrder(t *testing.T) {
	for _, supported := range [][]string{
		{LocaleENUS, LocaleZHCN},
		{LocaleZHCN, LocaleENUS},
	} {
		got, ok := Negotiate("zh-CN, en-US", supported)
		if !ok || got != LocaleZHCN {
			t.Errorf("Negotiate(\"zh-CN, en-US\", %v) = (%q, %v), want (%q, true)", supported, got, ok, LocaleZHCN)
		}
	}
	got, ok := Negotiate("de-DE, en-US, zh-CN", []string{LocaleZHCN, LocaleENUS})
	if !ok || got != LocaleENUS {
		t.Errorf("Negotiate = (%q, %v), want the first supported header part", got, ok)
	}
}

// TestNegotiate_DoesNotMutateTheSupportedSet guards the helper contract
// callers rely on: the supported slice is read-only input, and the caller's
// catalog order survives any negotiation.
func TestNegotiate_DoesNotMutateTheSupportedSet(t *testing.T) {
	supported := []string{LocaleZHCN, LocaleENUS}
	before := slices.Clone(supported)
	Negotiate("en-US, zh-CN", supported)
	if !reflect.DeepEqual(supported, before) {
		t.Errorf("Negotiate mutated its supported slice: got %v, want %v", supported, before)
	}
}
