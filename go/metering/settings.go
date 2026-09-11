package metering

// settings.go carries the seam through which this module's declared dynamic
// configuration (ConfigPeriodBucketSize and ConfigDefaultOverageThreshold,
// declared on the registry by Register) reaches the behavior it controls.
//
// Before the seam existed both declarations were schema-only -- this module
// said so itself (see overage.go's OverageThresholds doc comment) -- while
// the operative knobs were module options (WithPeriodBucket,
// WithOverageThresholds). The seam is the structurally-typed shape the authn
// and sharing precedents use: it is defined in terms of the config module's
// own Handle reads, so a *config.Handle satisfies it with no adapter, and
// the component descriptor wires configModule.Handle() when a config
// component is part of the assembly.
//
// Reading semantics: an explicit row wins; an unset row (ok=false), a nil
// reader, or a failed read falls back to the construction-time value, so a
// host that configures nothing dynamically behaves exactly as before the
// seam existed. The one non-obvious part is what an explicit row MEANS for
// the threshold, whose zero is itself meaningful: see resolveThreshold.

import (
	"context"

	"github.com/vislake/speed/go/observability"
)

// SettingsReader is the structurally-typed seam through which this module's
// declared config items are read at runtime: the subset of
// (*config.Handle)'s typed reads metering consumes. A host wires it with
//
//	metering.WithSettingsReader(configModule.Handle())
//
// and this module's component descriptor wires the same value when a config
// component is part of the assembly. Nil is legal: every read falls back to
// the construction-time value.
type SettingsReader interface {
	// Int returns key's effective integer value for the tenant the
	// context carries, and whether an explicit row produced it.
	Int(ctx context.Context, key string) (int64, bool, error)
	// String returns key's effective string value for the tenant the
	// context carries, and whether an explicit row produced it.
	String(ctx context.Context, key string) (string, bool, error)
}

// WithSettingsReader wires the reader this module's declared dynamic
// configuration is resolved through at runtime. See SettingsReader.
func WithSettingsReader(reader SettingsReader) Option {
	return func(m *Module) { m.aggregator.settings = reader }
}

// bucketFor resolves the calendar bucket this fold uses: the construction-
// time bucket (WithPeriodBucket, default PeriodBucketMonthly) unless an
// explicit metering.period_bucket_size row names a different SHIPPED bucket.
// An unrecognized value is logged and the construction-time bucket stands --
// deliberately fail-soft: an invalid bucket would otherwise fail periodBounds
// on EVERY fold, and metering must not stop measuring because of a bad
// configuration value (a billing-grade ingest that cannot fold is lost
// usage). A bucket change takes effect on the next fold; the windows of
// events already folded are untouched, because each fold computes its own
// period bounds from this answer (see the counter key's doc comment: a new
// window simply gets a fresh counter).
func (a *Aggregator) bucketFor(ctx context.Context) string {
	if a.settings == nil {
		return a.bucket
	}
	value, ok, err := a.settings.String(ctx, ConfigPeriodBucketSize)
	switch {
	case err != nil:
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value",
			"key", ConfigPeriodBucketSize, "error", err)
		return a.bucket
	case !ok:
		return a.bucket
	case value == PeriodBucketDaily || value == PeriodBucketMonthly:
		return value
	default:
		observability.FromContext(ctx).Warn("dynamic configuration resolved an unknown period bucket; using the construction-time value",
			"key", ConfigPeriodBucketSize, "value", value)
		return a.bucket
	}
}

// resolveThreshold returns the overage threshold in force for feature right
// now: a per-feature entry wins (it is always construction-time -- the
// declaration has no per-feature shape), then an explicit, positive
// metering.default_overage_threshold row, then the construction-time
// OverageThresholds.Default. Zero from the dynamic row means "no default
// threshold", matching the declared item's own semantics and
// OverageThresholds.Default's nil -- so an operator can turn the default off
// as well as on.
//
// A mid-run threshold change is correct by design, not merely tolerated:
// every fold re-resolves the threshold and the summary row records the value
// each fold used (UsageSummary.OverageThreshold), which the restart
// reconstruction compares for equality -- see the Aggregator type's
// "Reconstruction after a restart" doc comment, whose threshold-identity
// rule was built for exactly the "an operator lowering a limit" case.
func (a *Aggregator) resolveThreshold(ctx context.Context, feature string) (float64, bool) {
	if v, ok := a.thresholds.PerFeature[feature]; ok {
		return v, true
	}
	if a.settings != nil {
		n, ok, err := a.settings.Int(ctx, ConfigDefaultOverageThreshold)
		switch {
		case err != nil:
			observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value",
				"key", ConfigDefaultOverageThreshold, "error", err)
		case ok:
			if n > 0 {
				return float64(n), true
			}
			return 0, false
		}
	}
	if a.thresholds.Default != nil {
		return *a.thresholds.Default, true
	}
	return 0, false
}
