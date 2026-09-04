package billing

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestErrors_HaveDistinctCodes pins that every error this file declares
// carries a unique code -- a duplicate would mean two failures the API
// cannot tell apart.
func TestErrors_HaveDistinctCodes(t *testing.T) {
	errs := []*apperr.Error{
		ErrPlanNotFound,
		ErrPlanKeyRequired,
		ErrDuplicatePlanKey,
		ErrSubscriptionNotFound,
		ErrInvalidSubscriptionTransition,
		ErrInvoiceNotFound,
		ErrInvalidAmount,
		ErrIdempotencyKeyRequired,
		ErrInsufficientCredits,
		ErrCreditTransactionNotFound,
		ErrCreditTransactionAlreadyResolved,
		ErrCreditBalanceInconsistent,
		ErrWebhookSignatureInvalid,
		ErrWebhookPayloadUnrecognized,
		ErrChannelReferenceNotFound,
		ErrPaymentEventNotFound,
	}
	seen := make(map[string]bool, len(errs))
	for _, e := range errs {
		if seen[e.Code] {
			t.Errorf("duplicate error code %q", e.Code)
		}
		seen[e.Code] = true
	}
}

func TestHasCode(t *testing.T) {
	base := apperr.NotFound("billing.some_not_found")
	decorated := base.WithParam("id", "abc").WithCause(errors.New("boom"))

	tests := []struct {
		name string
		err  error
		code string
		want bool
	}{
		{name: "matching code on the undecorated sentinel", err: base, code: "billing.some_not_found", want: true},
		{name: "matching code survives WithParam/WithCause decoration", err: decorated, code: "billing.some_not_found", want: true},
		{name: "mismatched code", err: base, code: "billing.other", want: false},
		{name: "a plain non-apperr error", err: errors.New("plain"), code: "billing.some_not_found", want: false},
		{name: "nil error", err: nil, code: "billing.some_not_found", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCode(tc.err, tc.code); got != tc.want {
				t.Errorf("hasCode(%v, %q) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}
