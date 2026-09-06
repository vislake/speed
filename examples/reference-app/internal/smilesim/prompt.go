package smilesim

import (
	"fmt"
	"strings"
)

// This file is the provider prompt template this package gained in the P2a
// round: renderSimulationPrompt turns one validated SimulationOptions into
// the single English instruction sent to the vendor as the image request's
// prompt field. Like the pre-parameterization simulationPrompt constant it
// replaces, the rendered prompt carries no user-facing text of its own to
// localize -- it is sent to the vendor, never rendered to a person.
//
// The template is a fixed sequence of five sentences, each a package
// constant or a pure function of one option dimension, joined by single
// spaces:
//
//  1. the SmileStyle sentence -- the shape the mouth is asked to take;
//  2. the ToothShade sentence -- the tooth color asked for;
//  3. the Strength sentence -- the transformation magnitude, rendered with
//     the exact requested value (strengthText), never a rounded tier;
//  4. identityPreservationPrompt -- the strengthening sentence this round
//     added, spelling out what "the rest of the face" means for this
//     product (identity, proportions, skin tone, lip color, pose);
//  5. simulationPreservationPrompt -- the pre-existing preservation
//     sentence, byte-for-byte the second sentence of the old
//     simulationPrompt constant. It is deliberately the LAST sentence of
//     every render and is never weakened, reworded or omitted: keeping
//     "the rest of the face, lighting and background unchanged" semantics
//     is the product's core promise, and prompt_test.go pins its verbatim
//     presence as the suffix of every combination it renders.

const (
	// smileStylePromptSubtle is the SmileStyleSubtle sentence -- the most
	// conservative change of the three.
	smileStylePromptSubtle = "Shape the mouth into a subtle, gentle smile with only a slight curve."

	// smileStylePromptNatural is the SmileStyleNatural sentence -- the
	// even, relaxed smile that is this package's default.
	smileStylePromptNatural = "Shape the mouth into a natural, relaxed smile with evenly aligned teeth."

	// smileStylePromptBright is the SmileStyleBright sentence -- the
	// fullest tooth display of the three.
	smileStylePromptBright = "Shape the mouth into a bright, open smile with clearly visible, evenly aligned teeth."

	// toothShadePromptNatural is the ToothShadeNatural sentence.
	toothShadePromptNatural = "Give the teeth a natural tooth shade."

	// toothShadePromptWhite is the ToothShadeWhite sentence.
	toothShadePromptWhite = "Give the teeth a clean white shade."

	// toothShadePromptUltraWhite is the ToothShadeUltraWhite sentence.
	toothShadePromptUltraWhite = "Give the teeth a bright ultra-white shade."

	// strengthPromptFrame is the Strength sentence template: %s is
	// strengthText of the requested value. The sentence anchors the scale
	// for the vendor (1.0 is the complete simulated smile) so a fractional
	// value reads as a deliberate partial transformation rather than an
	// arbitrary number, and the exact value is named -- never bucketed,
	// never rounded -- so the prompt agrees with the option set durably
	// recorded for this simulation (simulation_store.go).
	strengthPromptFrame = "Apply the smile transformation at a strength of %s, where 1.0 means the complete simulated smile and lower values keep the original mouth progressively closer to unchanged."

	// identityPreservationPrompt names what this product's preservation
	// promise covers beyond the mouth itself -- the dimensions the P2a
	// round's product vision calls out (identity, proportions, skin tone,
	// lip color, pose) -- before the legacy preservation sentence below
	// states the unchanged-rest rule. Adding it strengthens the
	// instruction; it never replaces or rewrites the sentence after it.
	identityPreservationPrompt = "Preserve the patient's facial identity, proportions, skin tone, lip color and pose."

	// simulationPreservationPrompt is the preservation instruction this
	// service has sent since before parameterization, byte-for-byte the
	// second sentence of the old simulationPrompt constant ("Simulate a
	// bright, straight, natural-looking smile for this dental patient
	// photo. Keep the rest of the face, lighting and background
	// unchanged."). Every rendered prompt ends with exactly this sentence
	// (see the file doc comment and prompt_test.go's suffix pins).
	simulationPreservationPrompt = "Keep the rest of the face, lighting and background unchanged."
)

// renderSimulationPrompt renders the vendor prompt for o -- see this file's
// doc comment for the template's fixed sentence sequence. It must only ever
// be called with a validated option set (Simulate validates before it
// renders); an invalid style or shade falls through its switch to the empty
// string, which would render a visibly broken prompt rather than a silent
// substitution, and the validation gate upstream means that never happens
// on a real path.
func renderSimulationPrompt(o SimulationOptions) string {
	return strings.Join([]string{
		smileStylePromptSentence(o.SmileStyle),
		toothShadePromptSentence(o.ToothShade),
		fmt.Sprintf(strengthPromptFrame, strengthText(o.Strength)),
		identityPreservationPrompt,
		simulationPreservationPrompt,
	}, " ")
}

// smileStylePromptSentence returns the SmileStyle sentence for style.
func smileStylePromptSentence(style SmileStyle) string {
	switch style {
	case SmileStyleSubtle:
		return smileStylePromptSubtle
	case SmileStyleNatural:
		return smileStylePromptNatural
	case SmileStyleBright:
		return smileStylePromptBright
	default:
		// Unreachable through any validated option set -- see
		// renderSimulationPrompt's own doc comment. The empty result is
		// deliberately loud (a missing clause in a prompt) rather than a
		// silent default substitution.
		return ""
	}
}

// toothShadePromptSentence returns the ToothShade sentence for shade.
func toothShadePromptSentence(shade ToothShade) string {
	switch shade {
	case ToothShadeNatural:
		return toothShadePromptNatural
	case ToothShadeWhite:
		return toothShadePromptWhite
	case ToothShadeUltraWhite:
		return toothShadePromptUltraWhite
	default:
		// Unreachable through any validated option set -- see
		// renderSimulationPrompt's own doc comment.
		return ""
	}
}
