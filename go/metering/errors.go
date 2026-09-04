package metering

import "github.com/vislake/speed/go/pkgcore/apperr"

// The error index of the metering module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below, since WithParam/WithCause derive a new *apperr.Error
// rather than mutating the receiver -- the same convention dbkit, tenancy,
// org and pki already document.
//
// Only the codes this round's own validation paths can actually return are
// declared here. In particular there is no "metering.unknown_feature":
// this round has no feature catalog to check an event's Feature against
// (that belongs to go/billing's Plan/Feature/Entitlement model, per
// AGENTS.md's Known limitations), so a code for a check nothing performs
// would be dead catalog weight, not forward compatibility -- the same
// discipline go/pki's error index documents for its own round boundary.
var (
	// ErrMissingTenantID reports that a UsageEvent's TenantID was empty.
	// Unlike an HTTP API, this Go-level Recorder/Enqueue surface takes the
	// tenant as an explicit UsageEvent field rather than reading it off ctx
	// (see UsageEvent's own doc comment for why), so this is the
	// caller-error validation that field still needs.
	ErrMissingTenantID = apperr.Invalid("metering.missing_tenant_id")

	// ErrMissingFeature reports that a UsageEvent's Feature was empty.
	ErrMissingFeature = apperr.Invalid("metering.missing_feature")

	// ErrMissingIdempotencyKey reports that a UsageEvent's IdempotencyKey
	// was empty. It is mandatory, not optional: it is what lets a retried
	// Record or Enqueue call be told apart from a second, genuinely new
	// unit of usage.
	ErrMissingIdempotencyKey = apperr.Invalid("metering.missing_idempotency_key")

	// ErrInvalidQuantity reports that a UsageEvent's Quantity was negative,
	// NaN or infinite. Zero is legal (a usage event that measures something
	// other than a positive quantity, e.g. a heartbeat); negative is not --
	// a correction or refund is a business-level concept metering's own
	// pipeline does not model this round, not a negative usage record.
	ErrInvalidQuantity = apperr.Invalid("metering.invalid_quantity")

	// ErrMetadataTooLarge reports that a UsageEvent's Metadata exceeded the
	// bounds documented on UsageEvent.Metadata itself: it is small,
	// bounded context, never a dumping ground for arbitrary payloads.
	ErrMetadataTooLarge = apperr.Invalid("metering.metadata_too_large")

	// ErrInvalidPeriodBucket reports that a period-bucket size string was
	// neither PeriodBucketDaily nor PeriodBucketMonthly.
	ErrInvalidPeriodBucket = apperr.Invalid("metering.invalid_period_bucket")
)

// hasCode reports whether err is (or wraps, via apperr.As's Unwrap chain
// walk) an *apperr.Error whose Code equals code. This is the standard way
// this codebase compares against a dbkit or apperr sentinel once it may
// have been decorated with WithParam/WithCause -- see go/org/tree.go's
// identical helper.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
