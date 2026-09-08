package smilesim

import (
	"math"
	"strconv"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file carries the parameterization of a smile simulation: the option
// vocabulary one Simulate call may ask for (SimulationOptions
// and its three dimensions below), the validation that rejects an option
// outside the vocabulary or range with a coded error, the defaults a caller
// that names no options gets, and the functional-option helpers
// (SimulateOption/WithSmileStyle/WithToothShade/WithStrength) Simulate's own
// signature accepts so a bare two-argument call keeps meaning "the
// defaults". The provider prompt itself renders from a resolved option set
// in prompt.go.

// SmileStyle is the character of the simulated smile -- the shape the mouth
// is asked to take. The three legal values form a deliberate ladder from the
// most conservative change to the fullest one, so a caller can pick how much
// of a transformation the outcome should show without ever leaving the
// product's core facial-preservation promise (every style's prompt sentence
// still ends in the unchanged preservation block; see prompt.go).
type SmileStyle string

const (
	// SmileStyleSubtle shapes the mouth into a subtle, gentle smile with
	// only a slight curve -- the option closest to the patient's own
	// expression, for a caller who wants the least visible change.
	SmileStyleSubtle SmileStyle = "subtle"

	// SmileStyleNatural shapes the mouth into a natural, relaxed smile
	// with evenly aligned teeth -- the middle of the ladder and the
	// default, standing in for the kind of even, unforced smile an
	// orthodontic outcome would produce.
	SmileStyleNatural SmileStyle = "natural"

	// SmileStyleBright shapes the mouth into a bright, open smile with
	// clearly visible, evenly aligned teeth -- the fullest tooth display
	// of the three.
	SmileStyleBright SmileStyle = "bright"
)

// ToothShade is the tooth color the simulated smile is asked to show. The
// three legal values are a brightness ladder from the patient's own teeth to
// the most whitened one.
type ToothShade string

const (
	// ToothShadeNatural asks for a natural tooth shade -- teeth that look
	// like the patient's own, the default.
	ToothShadeNatural ToothShade = "natural"

	// ToothShadeWhite asks for a clean white shade -- a visibly whiter
	// tooth color than natural.
	ToothShadeWhite ToothShade = "white"

	// ToothShadeUltraWhite asks for a bright ultra-white shade, the
	// whitest option in the vocabulary.
	ToothShadeUltraWhite ToothShade = "ultra-white"
)

// Strength bounds, documented together because the range's shape is a
// deliberate decision: an out-of-range Strength is REJECTED with a
// coded error, never silently clamped, because the durable per-photo record
// (simulation_store.go) stores the exact option set that produced an image
// and the prompt (prompt.go) renders the exact strength value -- a clamped
// value would make both the record and the prompt lie about what the caller
// asked for, on a billed operation whose request should be answered as
// asked.
const (
	// minSimulationStrength is the exclusive lower bound of Strength: a
	// zero-strength simulation would be a billed no-op (an image
	// deliberately unchanged from the photo) and is refused as
	// meaningless rather than accepted.
	minSimulationStrength = 0.0

	// maxSimulationStrength is the inclusive upper bound of Strength: 1.0
	// is the complete simulated smile, and the default.
	maxSimulationStrength = 1.0
)

// SimulationOptions is the effective, fully-specified option set for one
// smile-simulation generation -- every field always carries a legal value
// once the set has been defaulted and validated (see
// DefaultSimulationOptions and Validate). It is the shape durably recorded
// per simulation (simulation_store.go), returned by
// Service.OptionsForJob, and carried per entry by
// Service.ListSimulationsByPhoto, so an outcome always answers "which
// options produced it".
type SimulationOptions struct {
	// SmileStyle is the smile character requested; see SmileStyle's own
	// doc comment. Empty (the zero value) is invalid and must be replaced
	// by a default or an explicit choice before use.
	SmileStyle SmileStyle `json:"smile_style"`

	// ToothShade is the tooth color requested; see ToothShade's own doc
	// comment. Empty (the zero value) is invalid and must be replaced by
	// a default or an explicit choice before use.
	ToothShade ToothShade `json:"tooth_shade"`

	// Strength is how strongly the transformation moves the photo toward
	// the requested smile, on a scale whose lower bound is exclusive
	// (Strength == 0 would be a billed no-op and is invalid) and whose
	// upper bound 1.0 is the complete simulated smile. The zero value is
	// invalid (it equals the excluded lower bound), so "unset" and
	// "explicitly zero" are indistinguishable in Go and both are refused
	// rather than one of them silently meaning the default.
	Strength float64 `json:"strength"`
}

// DefaultSimulationOptions returns the option set a Simulate call that names
// no options gets: SmileStyleNatural, ToothShadeNatural, full Strength. The
// natural/natural pair is this package's documented default because it is
// the most conservative middle of both ladders for a patient-facing feature;
// full strength is the setting the package's prompt was written for (the
// complete-smile outcome).
func DefaultSimulationOptions() SimulationOptions {
	return SimulationOptions{
		SmileStyle: SmileStyleNatural,
		ToothShade: ToothShadeNatural,
		Strength:   maxSimulationStrength,
	}
}

// validSmileStyles and validToothShades are the one validation table both
// Validate and the with-option helpers' own doc comments reference -- a
// whitelist, never a blacklist, so a typo'd or future-invented value is
// refused rather than guessed at.
var validSmileStyles = map[SmileStyle]bool{
	SmileStyleSubtle:  true,
	SmileStyleNatural: true,
	SmileStyleBright:  true,
}

var validToothShades = map[ToothShade]bool{
	ToothShadeNatural:    true,
	ToothShadeWhite:      true,
	ToothShadeUltraWhite: true,
}

// The option-validation sentinels, named as package-level declarations so
// the app's error-mapping audits (the codes-alignment suite of the web
// host, which cites every reachable code to the declaration that defines
// it) can cite stable sites. Each carries the offending value in params;
// the three are HTTP 400 once they cross a route.
var (
	// ErrUnsupportedSmileStyle refuses a SmileStyle outside the
	// vocabulary (smilesim.unsupported_smile_style).
	ErrUnsupportedSmileStyle = apperr.Invalid("smilesim.unsupported_smile_style")

	// ErrUnsupportedToothShade refuses a ToothShade outside the
	// vocabulary (smilesim.unsupported_tooth_shade).
	ErrUnsupportedToothShade = apperr.Invalid("smilesim.unsupported_tooth_shade")

	// ErrStrengthOutOfRange refuses a Strength outside (0, 1], including
	// NaN and the excluded zero (smilesim.strength_out_of_range).
	ErrStrengthOutOfRange = apperr.Invalid("smilesim.strength_out_of_range")
)

// Validate reports whether o is a legal, fully-specified option set. Every
// failure is a coded *apperr.Error (HTTP 400 once it crosses a route): a
// smile style or tooth shade outside the vocabulary is refused with
// smilesim.unsupported_smile_style / smilesim.unsupported_tooth_shade
// carrying the offending value, and a Strength outside (0, 1] -- including
// NaN and the excluded zero -- is refused with
// smilesim.strength_out_of_range carrying the value and the bounds. Nothing
// is clamped, for the reason Strength's own bound constants document.
func (o SimulationOptions) Validate() error {
	if !validSmileStyles[o.SmileStyle] {
		return ErrUnsupportedSmileStyle.WithParam("value", string(o.SmileStyle))
	}
	if !validToothShades[o.ToothShade] {
		return ErrUnsupportedToothShade.WithParam("value", string(o.ToothShade))
	}
	if math.IsNaN(o.Strength) || o.Strength <= minSimulationStrength || o.Strength > maxSimulationStrength {
		return ErrStrengthOutOfRange.
			WithParam("value", strengthText(o.Strength)).
			WithParam("min_exclusive", strengthText(minSimulationStrength)).
			WithParam("max_inclusive", strengthText(maxSimulationStrength))
	}
	return nil
}

// SimulateOption is a functional option adjusting the option set of one
// Simulate call -- the same variadic-options shape go/jobs' own
// Queue.Enqueue accepts. Every helper stores the caller's value verbatim;
// the option set is validated as a whole, inside Simulate and before any
// credit is reserved, so an invalid value surfaces as the same coded
// smilesim.* error whether it arrived here or through a route.
type SimulateOption func(*SimulationOptions)

// WithSmileStyle asks the simulation to use style as its SmileStyle. An
// unknown style is refused by Simulate's own validation
// (smilesim.unsupported_smile_style), never silently replaced.
func WithSmileStyle(style SmileStyle) SimulateOption {
	return func(o *SimulationOptions) { o.SmileStyle = style }
}

// WithToothShade asks the simulation to use shade as its ToothShade. An
// unknown shade is refused by Simulate's own validation
// (smilesim.unsupported_tooth_shade), never silently replaced.
func WithToothShade(shade ToothShade) SimulateOption {
	return func(o *SimulationOptions) { o.ToothShade = shade }
}

// WithStrength asks the simulation to transform at strength -- see
// SimulationOptions.Strength's own doc comment for the meaning and the
// bounds, and Validate for why an out-of-range value is refused rather than
// clamped.
func WithStrength(strength float64) SimulateOption {
	return func(o *SimulationOptions) { o.Strength = strength }
}

// strengthText renders a Strength value as a plain decimal string -- the
// shared text form both Validate's smilesim.strength_out_of_range params and
// prompt.go's strength sentence use, so a rendered prompt and an error
// message always show the value the same way. Full precision, never
// scientific notation, so the JSON round-trip of, say, 0.3 prints as "0.3"
// and not "0.30000000000000004"; NaN renders as "NaN".
func strengthText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
