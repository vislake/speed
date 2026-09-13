package log

import (
	"errors"
	"fmt"
	"testing"
)

// sentinels is the design's error table, by name. A caller tells the classes
// apart with errors.Is, so two sentinels that matched each other would make a
// startup failure unclassifiable.
var sentinels = map[string]error{
	"ErrInvalidLevel":       ErrInvalidLevel,
	"ErrUnknownDestination": ErrUnknownDestination,
	"ErrUnknownFormat":      ErrUnknownFormat,
	"ErrMissingPath":        ErrMissingPath,
	"ErrInvalidFileParam":   ErrInvalidFileParam,
	"ErrConflictingOutput":  ErrConflictingOutput,
	"ErrOutputUnavailable":  ErrOutputUnavailable,
}

// TestSentinelsAreDistinct pins the set to the design's table and checks that
// no two of them are interchangeable, wrapped or bare.
func TestSentinelsAreDistinct(t *testing.T) {
	if len(sentinels) != 7 {
		t.Fatalf("the design table lists 7 sentinels, the test pins %d", len(sentinels))
	}
	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("%s is nil", name)
		}
		if err.Error() == "" {
			t.Errorf("%s has an empty message", name)
		}
		wrapped := fmt.Errorf("assembling output 0: %w", err)
		for otherName, other := range sentinels {
			match := errors.Is(wrapped, other)
			if otherName == name && !match {
				t.Errorf("a wrapped %s does not match %s", name, name)
			}
			if otherName != name && match {
				t.Errorf("a wrapped %s also matches %s", name, otherName)
			}
		}
	}
}
