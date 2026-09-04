package metering

import "time"

// The PeriodBucket vocabulary: the calendar granularity real-time counters
// and usage-summary rows reset on. Both are calendar-aligned (a real
// billing month, not a rolling 30-day window), matching how Plan.Grants'
// ResetPeriod is framed in docs/internal/06-billing-and-metering.md.
const (
	PeriodBucketDaily   = "daily"
	PeriodBucketMonthly = "monthly"
)

// defaultPeriodBucket is what NewModule wires when the host does not call
// WithPeriodBucket -- monthly, matching the doc's own worked example and
// how a subscription billing cycle ordinarily resets.
const defaultPeriodBucket = PeriodBucketMonthly

// periodBounds returns the inclusive start and exclusive end of the
// calendar bucket (UTC) that occurredAt falls into, for bucket
// (PeriodBucketDaily or PeriodBucketMonthly). Any other value is rejected
// with ErrInvalidPeriodBucket -- a fail-closed default, since silently
// falling back to one bucket size when the configured one is unrecognized
// would put later events under the wrong period without any signal.
func periodBounds(occurredAt time.Time, bucket string) (start, end time.Time, err error) {
	t := occurredAt.UTC()
	switch bucket {
	case PeriodBucketDaily:
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case PeriodBucketMonthly:
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default:
		return time.Time{}, time.Time{}, ErrInvalidPeriodBucket.WithParam("bucket", bucket)
	}
	return start, end, nil
}

// summaryID derives UsageSummary's deterministic primary key from feature
// and periodStart: a summary row is the unique aggregate of one feature
// within one tenant's one period, and this is what lets Aggregator.upsertSummary
// reach it through dbkit.Repository[UsageSummary].FindByID rather than a
// hand-written query. It is deterministic (not random) specifically so
// two Ingest calls for the same (tenant, feature, period) resolve to the
// same row instead of racing to create two.
//
// The "|" separator is safe for the ordinary feature-key vocabulary this
// codebase uses elsewhere ("ai.generation", "api.calls" -- dotted
// lowercase segments) and periodStart's RFC 3339 rendering, neither of
// which contains "|"; a Feature value that does would only cause an id
// collision with another Feature value ending in the colliding prefix,
// not a cross-tenant or cross-period collision, since TenantID is a
// separate primary-key column UsageSummary declares alongside ID (see
// model.go) -- exactly the reasoning dbkit's TenantModel doc comment gives
// for when a plain indexed tenant_id column is enough versus when a
// composite primary key is required.
func summaryID(feature string, periodStart time.Time) string {
	return feature + "|" + periodStart.UTC().Format(time.RFC3339)
}
