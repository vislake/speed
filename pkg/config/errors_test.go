package config

import (
	"errors"
	"testing"
)

// TestSentinelSetMatchesDesignTable holds the sentinel set to the one the
// design states, by name. A sentinel that were renamed, dropped or folded into
// another breaks this case at compile time or on the distinctness check.
func TestSentinelSetMatchesDesignTable(t *testing.T) {
	sentinels := map[string]error{
		"ErrHelpRequested":      ErrHelpRequested,
		"ErrMalformedLocator":   ErrMalformedLocator,
		"ErrUnknownScheme":      ErrUnknownScheme,
		"ErrUndeterminedFormat": ErrUndeterminedFormat,
		"ErrUnknownFormat":      ErrUnknownFormat,
		"ErrSourceUnavailable":  ErrSourceUnavailable,
		"ErrMalformedConfig":    ErrMalformedConfig,
		"ErrConfigConflict":     ErrConfigConflict,
		"ErrInvalidSchema":      ErrInvalidSchema,
		"ErrUnknownKey":         ErrUnknownKey,
		"ErrTypeMismatch":       ErrTypeMismatch,
		"ErrMissingRequired":    ErrMissingRequired,
	}
	if len(sentinels) != 12 {
		t.Fatalf("the case lists %d sentinels, want the 12 the design states", len(sentinels))
	}
	texts := make(map[string]string, len(sentinels))
	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("sentinel %s is nil", name)
		}
		if other, clash := texts[err.Error()]; clash {
			t.Errorf("sentinels %s and %s read the same: %q", name, other, err)
		}
		texts[err.Error()] = name
	}
	for name, err := range sentinels {
		for otherName, other := range sentinels {
			if name != otherName && errors.Is(err, other) {
				t.Errorf("errors.Is reports %s as %s, and the classes have to stay apart", name, otherName)
			}
		}
	}
}
