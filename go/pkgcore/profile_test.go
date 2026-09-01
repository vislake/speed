package pkgcore

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParseProfile_ValidValues_Normalised(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Profile
	}{
		{name: "demo exact", input: "demo", want: ProfileDemo},
		{name: "production exact", input: "production", want: ProfileProduction},
		{name: "demo uppercase", input: "DEMO", want: ProfileDemo},
		{name: "production mixed case", input: "Production", want: ProfileProduction},
		{name: "demo with surrounding spaces", input: "  demo  ", want: ProfileDemo},
		{name: "production with tab and newline", input: "\tproduction\n", want: ProfileProduction},
		{name: "demo with case and whitespace", input: " DeMo\n", want: ProfileDemo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProfile(tt.input)
			if err != nil {
				t.Fatalf("ParseProfile(%q) returned unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseProfile(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if !got.Valid() {
				t.Errorf("ParseProfile(%q) returned %q, which reports itself invalid", tt.input, got)
			}
		})
	}
}

func TestParseProfile_InvalidValues_ReturnDescriptiveError(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty string", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "unknown word", input: "staging"},
		{name: "abbreviation", input: "prod"},
		{name: "prefix of valid value", input: "dem"},
		{name: "valid value with suffix", input: "demo1"},
		{name: "both values at once", input: "demo production"},
		{name: "inner whitespace", input: "produ ction"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProfile(tt.input)
			if err == nil {
				t.Fatalf("ParseProfile(%q) returned no error, want one", tt.input)
			}
			if got != "" {
				t.Errorf("ParseProfile(%q) = %q on failure, want the zero Profile", tt.input, got)
			}
			if !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("ParseProfile(%q) error %v does not wrap ErrInvalidProfile", tt.input, err)
			}

			msg := err.Error()
			if !strings.Contains(msg, strconv.Quote(tt.input)) {
				t.Errorf("error %q does not echo the rejected input %q", msg, tt.input)
			}
			for _, valid := range []Profile{ProfileDemo, ProfileProduction} {
				if !strings.Contains(msg, string(valid)) {
					t.Errorf("error %q does not list the valid value %q", msg, valid)
				}
			}
		})
	}
}

func TestProfileValid(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
		want    bool
	}{
		{name: "demo constant", profile: ProfileDemo, want: true},
		{name: "production constant", profile: ProfileProduction, want: true},
		{name: "zero value", profile: Profile(""), want: false},
		{name: "unnormalised case is not valid", profile: Profile("DEMO"), want: false},
		{name: "untrimmed value is not valid", profile: Profile(" demo"), want: false},
		{name: "unknown value", profile: Profile("staging"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.profile.Valid(); got != tt.want {
				t.Errorf("Profile(%q).Valid() = %t, want %t", tt.profile, got, tt.want)
			}
		})
	}
}
