package pkgcore

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParseDeploymentMode_ValidValues_Normalised(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  DeploymentMode
	}{
		{name: "standalone exact", input: "standalone", want: DeploymentModeStandalone},
		{name: "distributed exact", input: "distributed", want: DeploymentModeDistributed},
		{name: "standalone uppercase", input: "STANDALONE", want: DeploymentModeStandalone},
		{name: "distributed mixed case", input: "Distributed", want: DeploymentModeDistributed},
		{name: "standalone with surrounding spaces", input: "  standalone  ", want: DeploymentModeStandalone},
		{name: "distributed with tab and newline", input: "\tdistributed\n", want: DeploymentModeDistributed},
		{name: "standalone with case and whitespace", input: " StAndAlone\n", want: DeploymentModeStandalone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDeploymentMode(tt.input)
			if err != nil {
				t.Fatalf("ParseDeploymentMode(%q) returned unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseDeploymentMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if !got.Valid() {
				t.Errorf("ParseDeploymentMode(%q) returned %q, which reports itself invalid", tt.input, got)
			}
		})
	}
}

func TestParseDeploymentMode_InvalidValues_ReturnDescriptiveError(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty string", input: ""},
		{name: "whitespace only", input: "   "},
		{name: "unknown word", input: "staging"},
		{name: "abbreviation", input: "dist"},
		{name: "prefix of valid value", input: "stand"},
		{name: "valid value with suffix", input: "standalone1"},
		{name: "both values at once", input: "standalone distributed"},
		{name: "inner whitespace", input: "distri buted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDeploymentMode(tt.input)
			if err == nil {
				t.Fatalf("ParseDeploymentMode(%q) returned no error, want one", tt.input)
			}
			if got != "" {
				t.Errorf("ParseDeploymentMode(%q) = %q on failure, want the zero DeploymentMode", tt.input, got)
			}
			if !errors.Is(err, ErrInvalidDeploymentMode) {
				t.Errorf("ParseDeploymentMode(%q) error %v does not wrap ErrInvalidDeploymentMode", tt.input, err)
			}

			msg := err.Error()
			if !strings.Contains(msg, strconv.Quote(tt.input)) {
				t.Errorf("error %q does not echo the rejected input %q", msg, tt.input)
			}
			for _, valid := range []DeploymentMode{DeploymentModeStandalone, DeploymentModeDistributed} {
				if !strings.Contains(msg, string(valid)) {
					t.Errorf("error %q does not list the valid value %q", msg, valid)
				}
			}
		})
	}
}

func TestDeploymentMode_RequiredCapabilities(t *testing.T) {
	tests := []struct {
		name string
		mode DeploymentMode
		want Capability
	}{
		{name: "distributed requires MultiReplicaSafe", mode: DeploymentModeDistributed, want: MultiReplicaSafe},
		{name: "standalone requires nothing extra", mode: DeploymentModeStandalone, want: 0},
		{
			// RequiredCapabilities is not the place an invalid mode is
			// rejected -- Kernel.Bootstrap checks Valid() itself before ever
			// asking for the requirement -- so an invalid value falls back to
			// the standalone requirement rather than panicking or erroring.
			name: "an invalid mode falls back to the standalone requirement",
			mode: DeploymentMode("staging"),
			want: 0,
		},
		{name: "the zero value falls back to the standalone requirement", mode: DeploymentMode(""), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.RequiredCapabilities(); got != tt.want {
				t.Errorf("DeploymentMode(%q).RequiredCapabilities() = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestDeploymentModeValid(t *testing.T) {
	tests := []struct {
		name string
		mode DeploymentMode
		want bool
	}{
		{name: "standalone constant", mode: DeploymentModeStandalone, want: true},
		{name: "distributed constant", mode: DeploymentModeDistributed, want: true},
		{name: "zero value", mode: DeploymentMode(""), want: false},
		{name: "unnormalised case is not valid", mode: DeploymentMode("STANDALONE"), want: false},
		{name: "untrimmed value is not valid", mode: DeploymentMode(" standalone"), want: false},
		{name: "unknown value", mode: DeploymentMode("staging"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.Valid(); got != tt.want {
				t.Errorf("DeploymentMode(%q).Valid() = %t, want %t", tt.mode, got, tt.want)
			}
		})
	}
}
