package metering

import (
	"errors"
	"net/http"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestErrors_AreAllInvalid pins that every error this file declares is an
// apperr.Invalid (a caller-fixable validation problem) rather than some
// other apperr kind -- every one of them reports a malformed UsageEvent or
// configuration value, never a not-found or an internal failure.
func TestErrors_AreAllInvalid(t *testing.T) {
	errs := []*apperr.Error{
		ErrMissingTenantID,
		ErrMissingFeature,
		ErrMissingIdempotencyKey,
		ErrInvalidQuantity,
		ErrMetadataTooLarge,
		ErrInvalidPeriodBucket,
	}
	seen := make(map[string]bool, len(errs))
	for _, e := range errs {
		if e.Status != http.StatusBadRequest {
			t.Errorf("error %q has Status %d, want %d (apperr.Invalid)", e.Code, e.Status, http.StatusBadRequest)
		}
		if seen[e.Code] {
			t.Errorf("duplicate error code %q", e.Code)
		}
		seen[e.Code] = true
	}
}

// TestErrMetadataEncodeFailed_IsInternal pins ErrMetadataEncodeFailed's
// kind: unlike every var TestErrors_AreAllInvalid checks, it reports an
// encode failure of already-validated data (outbox.go's encodeMetadata),
// never a malformed caller input, so it is deliberately apperr.Internal
// (500), not apperr.Invalid (400) -- see the var's own doc comment.
func TestErrMetadataEncodeFailed_IsInternal(t *testing.T) {
	if ErrMetadataEncodeFailed.Status != http.StatusInternalServerError {
		t.Errorf("ErrMetadataEncodeFailed.Status = %d, want %d (apperr.Internal)", ErrMetadataEncodeFailed.Status, http.StatusInternalServerError)
	}
	if ErrMetadataEncodeFailed.Code != "metering.metadata_encode_failed" {
		t.Errorf("ErrMetadataEncodeFailed.Code = %q, want %q", ErrMetadataEncodeFailed.Code, "metering.metadata_encode_failed")
	}
}

func TestHasCode(t *testing.T) {
	base := apperr.NotFound("metering.some_not_found")
	decorated := base.WithParam("id", "abc").WithCause(errors.New("boom"))

	tests := []struct {
		name string
		err  error
		code string
		want bool
	}{
		{name: "matching code on the undecorated sentinel", err: base, code: "metering.some_not_found", want: true},
		{name: "matching code survives WithParam/WithCause decoration", err: decorated, code: "metering.some_not_found", want: true},
		{name: "mismatched code", err: base, code: "metering.other", want: false},
		{name: "a plain non-apperr error", err: errors.New("plain"), code: "metering.some_not_found", want: false},
		{name: "nil error", err: nil, code: "metering.some_not_found", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCode(tc.err, tc.code); got != tc.want {
				t.Errorf("hasCode(%v, %q) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}
