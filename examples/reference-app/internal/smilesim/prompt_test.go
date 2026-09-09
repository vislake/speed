package smilesim

import (
	"strings"
	"testing"
)

// This file pins the rendered provider prompt (prompt.go): that each option
// vocabulary value speaks exactly its own instruction, that the strength
// value renders verbatim (never bucketed, never rounded), that the
// rendering is deterministic, and -- the pins this product's core promise
// depends on -- that the preservation sentence appears VERBATIM as
// the final sentence of every render, never weakened or reworded.
//
// The phrase literals below are deliberately independent copies of the
// prompt sentences -- not references to prompt.go's constants -- so a
// reworded clause fails here, in the test, rather than passing because the
// test and the implementation changed together.

// distinctive clause fragments, keyed by the vocabulary value they belong
// to. Each is a substring that appears in exactly one option's sentence and
// in no other sentence of the full prompt (e.g. "natural" alone would match
// both the natural style and the natural shade sentences; these fragments
// do not).
var styleClauses = map[SmileStyle]string{
	SmileStyleSubtle:  "subtle, gentle smile with only a slight curve",
	SmileStyleNatural: "natural, relaxed smile with evenly aligned teeth",
	SmileStyleBright:  "bright, open smile with clearly visible, evenly aligned teeth",
}

var shadeClauses = map[ToothShade]string{
	ToothShadeNatural:    "natural tooth shade",
	ToothShadeWhite:      "clean white shade",
	ToothShadeUltraWhite: "bright ultra-white shade",
}

// preservationSentenceLegacy is the preservation sentence as it appears
// in every rendered prompt -- the "never weaken" pin's ground truth.
const preservationSentenceLegacy = "Keep the rest of the face, lighting and background unchanged."

// identitySentenceP2a is the strengthening sentence that precedes the
// preservation sentence -- it may be reworded only deliberately, since it
// names the product vision's preservation dimensions.
const identitySentenceP2a = "Preserve the patient's facial identity, proportions, skin tone, lip color and pose."

// TestPromptSentenceHelpers_UnknownValues_AnswerTheLoudEmptyString pins
// the two vocabulary-switch guards directly: a style or shade value no
// validated option set can produce answers the deliberately loud empty
// clause (never a silent substitution of another value's sentence), so a
// missing case in either switch shows up as a gap in the rendered prompt
// rather than as a wrong instruction shipped to the model.
func TestPromptSentenceHelpers_UnknownValues_AnswerTheLoudEmptyString(t *testing.T) {
	if got := smileStylePromptSentence(SmileStyle("no-such-style")); got != "" {
		t.Fatalf("smileStylePromptSentence(unknown) = %q, want the loud empty string", got)
	}
	if got := toothShadePromptSentence(ToothShade("no-such-shade")); got != "" {
		t.Fatalf("toothShadePromptSentence(unknown) = %q, want the loud empty string", got)
	}
}

func TestRenderSimulationPrompt_EveryStyleShadeCombination_SpeaksOnlyItsOwnVocabulary(t *testing.T) {
	for style, styleClause := range styleClauses {
		for shade, shadeClause := range shadeClauses {
			o := DefaultSimulationOptions()
			o.SmileStyle = style
			o.ToothShade = shade

			prompt := renderSimulationPrompt(o)

			if !strings.Contains(prompt, styleClause) {
				t.Errorf("style %q prompt missing its own clause %q:\n%s", style, styleClause, prompt)
			}
			if !strings.Contains(prompt, shadeClause) {
				t.Errorf("shade %q prompt missing its own clause %q:\n%s", shade, shadeClause, prompt)
			}
			for otherStyle, otherClause := range styleClauses {
				if otherStyle == style {
					continue
				}
				if strings.Contains(prompt, otherClause) {
					t.Errorf("style %q prompt contains another style's clause %q (from %q):\n%s", style, otherClause, otherStyle, prompt)
				}
			}
			for otherShade, otherClause := range shadeClauses {
				if otherShade == shade {
					continue
				}
				if strings.Contains(prompt, otherClause) {
					t.Errorf("shade %q prompt contains another shade's clause %q (from %q):\n%s", shade, otherClause, otherShade, prompt)
				}
			}
		}
	}
}

func TestRenderSimulationPrompt_Strength_RendersTheExactRequestedValue(t *testing.T) {
	for _, strength := range []float64{0.25, 0.4, 0.5, 1} {
		o := DefaultSimulationOptions()
		o.Strength = strength

		prompt := renderSimulationPrompt(o)

		// strengthText renders 1 as "1" (never "1.0") and 0.4 as "0.4"
		// (never an artifact) -- the literal here is the same full
		// precision spelling the value was stored under, which is the
		// point: the prompt agrees with the record.
		want := "at a strength of " + strengthText(strength) + ", where 1.0 means the complete simulated smile"
		if !strings.Contains(prompt, want) {
			t.Errorf("strength %v prompt missing the exact strength clause %q:\n%s", strength, want, prompt)
		}
	}
}

// TestRenderSimulationPrompt_PreservationBlock_NeverWeakened is the
// core-promise pin: for every style, every shade and every one of a spread
// of strengths, the rendered prompt ends with the preservation
// sentence byte-for-byte as its final sentence, and carries the identity
// sentence in front of it -- the preservation instruction is never omitted,
// reworded or displaced.
func TestRenderSimulationPrompt_PreservationBlock_NeverWeakened(t *testing.T) {
	strengths := []float64{0.25, 0.5, 0.75, 1}
	for style := range styleClauses {
		for shade := range shadeClauses {
			for _, strength := range strengths {
				o := DefaultSimulationOptions()
				o.SmileStyle = style
				o.ToothShade = shade
				o.Strength = strength

				prompt := renderSimulationPrompt(o)
				if !strings.HasSuffix(prompt, preservationSentenceLegacy) {
					t.Errorf("prompt does not end with the legacy preservation sentence (style %q, shade %q, strength %v):\n%s", style, shade, strength, prompt)
				}
				identityAt := strings.Index(prompt, identitySentenceP2a)
				preservationAt := strings.Index(prompt, preservationSentenceLegacy)
				if identityAt < 0 {
					t.Errorf("prompt missing the identity sentence (style %q, shade %q, strength %v):\n%s", style, shade, strength, prompt)
				}
				if identityAt > preservationAt {
					t.Errorf("identity sentence appears after the preservation sentence (style %q, shade %q, strength %v):\n%s", style, shade, strength, prompt)
				}
			}
		}
	}
}

// TestRenderSimulationPrompt_SentenceOrder_PreservationLast pins the fixed
// template order clause-by-clause for the default option set: style, then
// shade, then strength, then identity, then the legacy preservation
// sentence last.
func TestRenderSimulationPrompt_SentenceOrder_PreservationLast(t *testing.T) {
	prompt := renderSimulationPrompt(DefaultSimulationOptions())

	positions := []int{
		strings.Index(prompt, styleClauses[SmileStyleNatural]),
		strings.Index(prompt, shadeClauses[ToothShadeNatural]),
		strings.Index(prompt, "at a strength of "+strengthText(DefaultSimulationOptions().Strength)),
		strings.Index(prompt, identitySentenceP2a),
		strings.Index(prompt, preservationSentenceLegacy),
	}
	for i := 0; i+1 < len(positions); i++ {
		if positions[i] < 0 {
			t.Fatalf("clause %d missing from the default prompt:\n%s", i, prompt)
		}
		if positions[i] >= positions[i+1] {
			t.Fatalf("clause %d (at %d) is not before clause %d (at %d) in the default prompt:\n%s", i, positions[i], i+1, positions[i+1], prompt)
		}
	}
}

func TestRenderSimulationPrompt_IsDeterministic(t *testing.T) {
	o := SimulationOptions{SmileStyle: SmileStyleBright, ToothShade: ToothShadeUltraWhite, Strength: 0.3}
	first := renderSimulationPrompt(o)
	for i := 0; i < 5; i++ {
		if again := renderSimulationPrompt(o); again != first {
			t.Fatalf("renderSimulationPrompt is not deterministic:\nfirst: %s\nagain: %s", first, again)
		}
	}
}
