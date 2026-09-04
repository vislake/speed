package integration

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// apperrIs reports whether err is (a decorated instance of) want, matching
// on Code -- the convention this module's own errors.go doc comment
// documents (every builder-derived error must be compared by Code, never by
// identity), mirroring go/pki/repository_test.go's identical helper.
func apperrIs(err error, want *apperr.Error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == want.Code
}

// TestErrors_EveryVarIsAnAppErrorWithTheModulePrefix proves every exported
// error var in this module's index (errors.go) is a genuine *apperr.Error
// (or, for ErrRateLimited, a struct literal of that same type) whose Code
// starts with "integration.", the convention every code in this codebase
// follows.
func TestErrors_EveryVarIsAnAppErrorWithTheModulePrefix(t *testing.T) {
	for _, err := range []*apperr.Error{
		ErrKeyNotFound,
		ErrKeyAlreadyRevoked,
		ErrCreatedByRequired,
		ErrExpiryExceedsMaximum,
		ErrExpiryInPast,
		ErrScopeNotHeldByCreator,
		ErrPermissionListerUnavailable,
		ErrRateLimited,
		ErrInternal,
	} {
		if err.Code == "" {
			t.Errorf("error has an empty Code: %+v", err)
			continue
		}
		if len(err.Code) < len("integration.") || err.Code[:len("integration.")] != "integration." {
			t.Errorf("Code = %q, want it to start with %q", err.Code, "integration.")
		}
	}
}

func TestErrRateLimited_Status429(t *testing.T) {
	if ErrRateLimited.Status != 429 {
		t.Errorf("ErrRateLimited.Status = %d, want 429", ErrRateLimited.Status)
	}
}
