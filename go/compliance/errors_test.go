package compliance

import (
	"errors"
	"fmt"
	"testing"
)

// TestHasCode_MatchesDecoratedError proves hasCode still matches after
// WithParam/WithCause have derived a new *apperr.Error, since those never
// mutate the receiver -- the exact pitfall this helper exists to avoid a
// caller falling into with == or errors.Is.
func TestHasCode_MatchesDecoratedError(t *testing.T) {
	decorated := ErrQueueRequired.WithParam("module", "compliance")
	if !hasCode(decorated, ErrQueueRequired.Code) {
		t.Error("hasCode should match a decorated error by Code")
	}
}

// TestHasCode_DoesNotMatchAPlainError proves hasCode returns false for an
// error that is not an *apperr.Error at all.
func TestHasCode_DoesNotMatchAPlainError(t *testing.T) {
	if hasCode(errors.New("plain"), ErrQueueRequired.Code) {
		t.Error("hasCode should not match a plain error")
	}
}

// TestHasCode_DoesNotMatchADifferentCode proves hasCode distinguishes
// between two different apperr codes.
func TestHasCode_DoesNotMatchADifferentCode(t *testing.T) {
	if hasCode(ErrQueueRequired, ErrEmptySubjectRef.Code) {
		t.Error("hasCode should not match a different code")
	}
}

// TestHasCode_MatchesThroughFmtErrorfWrap proves hasCode still matches
// through a plain %w wrap, since apperr.As walks the Unwrap chain.
func TestHasCode_MatchesThroughFmtErrorfWrap(t *testing.T) {
	wrapped := fmt.Errorf("compliance: enqueue failed: %w", ErrQueueRequired)
	if !hasCode(wrapped, ErrQueueRequired.Code) {
		t.Error("hasCode should match through a %w wrap")
	}
}
