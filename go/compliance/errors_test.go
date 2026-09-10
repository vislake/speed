package compliance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestHasCode_MatchesDecoratedError proves apperr.HasCode still matches
// after WithParam/WithCause have derived a new *apperr.Error, since those
// never mutate the receiver.
func TestHasCode_MatchesDecoratedError(t *testing.T) {
	decorated := ErrQueueRequired.WithParam("module", "compliance")
	if !apperr.HasCode(decorated, ErrQueueRequired.Code) {
		t.Error("apperr.HasCode should match a decorated error by Code")
	}
}

// TestHasCode_DoesNotMatchAPlainError proves apperr.HasCode returns false
// for an error that is not an *apperr.Error at all.
func TestHasCode_DoesNotMatchAPlainError(t *testing.T) {
	if apperr.HasCode(errors.New("plain"), ErrQueueRequired.Code) {
		t.Error("apperr.HasCode should not match a plain error")
	}
}

// TestHasCode_DoesNotMatchADifferentCode proves apperr.HasCode
// distinguishes between two different apperr codes.
func TestHasCode_DoesNotMatchADifferentCode(t *testing.T) {
	if apperr.HasCode(ErrQueueRequired, ErrEmptySubjectRef.Code) {
		t.Error("apperr.HasCode should not match a different code")
	}
}

// TestHasCode_MatchesThroughFmtErrorfWrap proves apperr.HasCode still
// matches through a plain %w wrap, since apperr.As walks the Unwrap chain.
func TestHasCode_MatchesThroughFmtErrorfWrap(t *testing.T) {
	wrapped := fmt.Errorf("compliance: enqueue failed: %w", ErrQueueRequired)
	if !apperr.HasCode(wrapped, ErrQueueRequired.Code) {
		t.Error("apperr.HasCode should match through a %w wrap")
	}
}
