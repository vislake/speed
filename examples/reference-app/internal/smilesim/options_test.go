package smilesim

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file pins the option vocabulary (options.go): the documented
// defaults, the acceptance of every legal value on every dimension, the
// coded rejection of every out-of-vocabulary/out-of-range value, and the
// JSON round-trip that the durable per-photo record (simulation_store.go)
// depends on. The rejection pins use the wire code strings as literals --
// not the package's own error construction -- so an accidental code change
// fails here, exactly where a client whitelist would notice it.

func TestDefaultSimulationOptions_PinsTheDocumentedDefaults(t *testing.T) {
	got := DefaultSimulationOptions()
	want := SimulationOptions{SmileStyle: SmileStyleNatural, ToothShade: ToothShadeNatural, Strength: 1}
	if got != want {
		t.Fatalf("DefaultSimulationOptions() = %+v, want %+v -- the defaults are part of this package's documented contract", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("DefaultSimulationOptions() fails its own Validate: %v", err)
	}
}

// TestSimulationOptions_Validate_AcceptsEveryLegalValue walks the full
// cross product of the two vocabularies at a legal strength, plus the
// strength range's own edges (every value in (0, 1] is legal), so an option
// accidentally removed from the vocabulary fails here with the dimension
// named.
func TestSimulationOptions_Validate_AcceptsEveryLegalValue(t *testing.T) {
	for _, style := range []SmileStyle{SmileStyleSubtle, SmileStyleNatural, SmileStyleBright} {
		for _, shade := range []ToothShade{ToothShadeNatural, ToothShadeWhite, ToothShadeUltraWhite} {
			for _, strength := range []float64{0.000001, 0.25, 0.5, 1} {
				o := SimulationOptions{SmileStyle: style, ToothShade: shade, Strength: strength}
				if err := o.Validate(); err != nil {
					t.Errorf("Validate(%+v) = %v, want nil", o, err)
				}
			}
		}
	}
}

func TestSimulationOptions_Validate_RejectsEveryIllegalValue(t *testing.T) {
	legal := DefaultSimulationOptions()

	tests := []struct {
		name    string
		mutate  func(*SimulationOptions)
		wantErr string
	}{
		{name: "empty smile style", mutate: func(o *SimulationOptions) { o.SmileStyle = "" }, wantErr: "smilesim.unsupported_smile_style"},
		{name: "unknown smile style", mutate: func(o *SimulationOptions) { o.SmileStyle = "dazzling" }, wantErr: "smilesim.unsupported_smile_style"},
		{name: "wrong-case smile style", mutate: func(o *SimulationOptions) { o.SmileStyle = "Natural" }, wantErr: "smilesim.unsupported_smile_style"},
		{name: "empty tooth shade", mutate: func(o *SimulationOptions) { o.ToothShade = "" }, wantErr: "smilesim.unsupported_tooth_shade"},
		{name: "unknown tooth shade", mutate: func(o *SimulationOptions) { o.ToothShade = "glittering" }, wantErr: "smilesim.unsupported_tooth_shade"},
		{name: "wrong-case tooth shade", mutate: func(o *SimulationOptions) { o.ToothShade = "Ultra-White" }, wantErr: "smilesim.unsupported_tooth_shade"},
		{name: "strength at the excluded lower bound", mutate: func(o *SimulationOptions) { o.Strength = 0 }, wantErr: "smilesim.strength_out_of_range"},
		{name: "negative strength", mutate: func(o *SimulationOptions) { o.Strength = -0.5 }, wantErr: "smilesim.strength_out_of_range"},
		{name: "strength above the upper bound", mutate: func(o *SimulationOptions) { o.Strength = 1.5 }, wantErr: "smilesim.strength_out_of_range"},
		{name: "NaN strength", mutate: func(o *SimulationOptions) { o.Strength = math.NaN() }, wantErr: "smilesim.strength_out_of_range"},
		{name: "infinite strength", mutate: func(o *SimulationOptions) { o.Strength = math.Inf(1) }, wantErr: "smilesim.strength_out_of_range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := legal
			tt.mutate(&o)
			err := o.Validate()
			if err == nil {
				t.Fatal("Validate succeeded, want a coded error")
			}
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("Validate error = %v, want an *apperr.Error", err)
			}
			if !apperr.HasCode(err, tt.wantErr) {
				t.Errorf("Validate error code = %q, want %q", appErr.Code, tt.wantErr)
			}
		})
	}
}

// TestSimulationOptions_StrengthOutOfRangeError_CarriesActionableParams
// pins the params a 400 response will carry: the offending value and the
// bounds, so a client can render "strength must be in (0, 1], got 1.5"
// without hardcoding the range.
func TestSimulationOptions_StrengthOutOfRangeError_CarriesActionableParams(t *testing.T) {
	o := DefaultSimulationOptions()
	o.Strength = 1.5
	appErr, ok := apperr.As(o.Validate())
	if !ok {
		t.Fatal("Validate error is not an *apperr.Error")
	}
	if got := appErr.Params["value"]; got != "1.5" {
		t.Errorf("params value = %v, want \"1.5\"", got)
	}
	if got := appErr.Params["min_exclusive"]; got != "0" {
		t.Errorf("params min_exclusive = %v, want \"0\"", got)
	}
	if got := appErr.Params["max_inclusive"]; got != "1" {
		t.Errorf("params max_inclusive = %v, want \"1\"", got)
	}
}

// TestSimulationOptions_JSONRoundTrip_PreservesExactValues pins the shape
// the durable per-photo record stores (simulation_store.go marshals the
// effective option set to JSON) and the shape the enumeration route reads
// back: the wire names are smile_style/tooth_shade/strength, and a decimal
// strength survives the round trip with its exact value -- 0.3 comes back
// as 0.3, never as a float artifact.
func TestSimulationOptions_JSONRoundTrip_PreservesExactValues(t *testing.T) {
	original := SimulationOptions{SmileStyle: SmileStyleBright, ToothShade: ToothShadeUltraWhite, Strength: 0.3}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wire := string(data)
	for _, want := range []string{`"smile_style":"bright"`, `"tooth_shade":"ultra-white"`, `"strength":0.3`} {
		if !strings.Contains(wire, want) {
			t.Errorf("marshaled options %s does not contain %s", wire, want)
		}
	}

	var back SimulationOptions
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != original {
		t.Fatalf("round trip changed the option set: got %+v, want %+v", back, original)
	}
}

// TestSimulationOptions_ZeroValue_IsRefused pins a deliberate semantic: the
// zero value (all empty) is an INVALID option set -- it is indistinguishable
// from "explicitly the excluded values" in Go, and Validate refuses it
// rather than one of the empty fields silently meaning the default. The
// route layer applies defaults for omitted fields BEFORE calling the
// service; the service itself never guesses.
func TestSimulationOptions_ZeroValue_IsRefused(t *testing.T) {
	var o SimulationOptions
	if err := o.Validate(); err == nil {
		t.Fatal("Validate(zero value) succeeded, want a coded error")
	}
}

// TestSimulateOptionHelpers_ApplyOnlyTheirOwnDimension pins the functional
// options' contract: each helper changes exactly the field it names,
// leaving the rest of a defaulted set untouched, so composing the helpers
// in any order yields the same set.
func TestSimulateOptionHelpers_ApplyOnlyTheirOwnDimension(t *testing.T) {
	o := DefaultSimulationOptions()
	WithSmileStyle(SmileStyleSubtle)(&o)
	if o.SmileStyle != SmileStyleSubtle || o.ToothShade != ToothShadeNatural || o.Strength != 1 {
		t.Fatalf("after WithSmileStyle: %+v", o)
	}
	WithToothShade(ToothShadeWhite)(&o)
	if o.SmileStyle != SmileStyleSubtle || o.ToothShade != ToothShadeWhite || o.Strength != 1 {
		t.Fatalf("after WithToothShade: %+v", o)
	}
	WithStrength(0.4)(&o)
	if o.SmileStyle != SmileStyleSubtle || o.ToothShade != ToothShadeWhite || o.Strength != 0.4 {
		t.Fatalf("after WithStrength: %+v", o)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
