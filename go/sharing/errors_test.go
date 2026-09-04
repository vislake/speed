package sharing

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestHasCode(t *testing.T) {
	err := ErrShareNotFound.WithParam("id", "x").WithCause(errors.New("boom"))
	if !hasCode(err, ErrShareNotFound.Code) {
		t.Errorf("hasCode(decorated ErrShareNotFound) = false, want true")
	}
	if hasCode(err, ErrNotAccessible.Code) {
		t.Errorf("hasCode(decorated ErrShareNotFound, ErrNotAccessible.Code) = true, want false")
	}
	if hasCode(errors.New("plain error"), ErrShareNotFound.Code) {
		t.Errorf("hasCode(a plain error) = true, want false")
	}
}

// TestErrorIndex_EveryCodeIsWellFormed pins that every declared error is a
// real *apperr.Error with a non-empty, correctly namespaced code -- a
// cheap, mechanical guard against a copy-paste mistake in errors.go.
func TestErrorIndex_EveryCodeIsWellFormed(t *testing.T) {
	all := []*apperr.Error{
		ErrResourceRefRequired, ErrExpiryRequired, ErrInvalidMaxViews,
		ErrNotAccessible, ErrShareNotFound, ErrInternal,
	}
	seen := map[string]bool{}
	for _, e := range all {
		if e == nil {
			t.Fatalf("a declared error is nil")
		}
		if seen[e.Code] {
			t.Errorf("code %q is declared more than once", e.Code)
		}
		seen[e.Code] = true
		if len(e.Code) < len("sharing.") || e.Code[:len("sharing.")] != "sharing." {
			t.Errorf("code %q does not use the sharing.<reason> convention", e.Code)
		}
	}
}
