package core

import "testing"

// TestSentinelSetMatchesDesignTable pins the sentinel set to the design's
// error table: six errors, each distinct, each reachable by name.
func TestSentinelSetMatchesDesignTable(t *testing.T) {
	sentinels := map[string]error{
		"ErrMissingProvider":       ErrMissingProvider,
		"ErrAmbiguousProvider":     ErrAmbiguousProvider,
		"ErrExclusiveViolated":     ErrExclusiveViolated,
		"ErrMissingReason":         ErrMissingReason,
		"ErrDependencyCycle":       ErrDependencyCycle,
		"ErrUndeliveredCapability": ErrUndeliveredCapability,
	}
	if len(sentinels) != 6 {
		t.Fatalf("the design table lists 6 sentinels, the test pins %d", len(sentinels))
	}
	seen := make(map[string]string, len(sentinels))
	for name, err := range sentinels {
		if err == nil {
			t.Errorf("%s is nil", name)
			continue
		}
		if err.Error() == "" {
			t.Errorf("%s has an empty message", name)
		}
		if other, dup := seen[err.Error()]; dup {
			t.Errorf("%s and %s share the message %q", name, other, err.Error())
		}
		seen[err.Error()] = name
	}
}
