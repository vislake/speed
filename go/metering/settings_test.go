package metering

import (
	"context"
	"errors"
	"testing"
)

// fakeSettings is a SettingsReader whose answers are scripted per key, so
// settings.go's resolution arms can be pinned without a config module.
type fakeSettings struct {
	ints     map[string]int64
	strings  map[string]string
	set      map[string]bool
	failKeys map[string]error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		ints:     map[string]int64{},
		strings:  map[string]string{},
		set:      map[string]bool{},
		failKeys: map[string]error{},
	}
}

func (f *fakeSettings) Int(_ context.Context, key string) (int64, bool, error) {
	if err := f.failKeys[key]; err != nil {
		return 0, false, err
	}
	return f.ints[key], f.set[key], nil
}

func (f *fakeSettings) String(_ context.Context, key string) (string, bool, error) {
	if err := f.failKeys[key]; err != nil {
		return "", false, err
	}
	return f.strings[key], f.set[key], nil
}

// TestBucketFor_ResolutionArms pins the bucket read: no reader, no row, a
// failed read and an unknown value all keep the construction-time bucket
// (fail-soft: a bad value must never fail every fold), while an explicit
// shipped-bucket row wins.
func TestBucketFor_ResolutionArms(t *testing.T) {
	ctx := context.Background()
	a := &Aggregator{bucket: PeriodBucketMonthly}

	if got := a.bucketFor(ctx); got != PeriodBucketMonthly {
		t.Fatalf("no reader = %q; want the construction-time bucket", got)
	}

	reader := newFakeSettings()
	a.settings = reader
	if got := a.bucketFor(ctx); got != PeriodBucketMonthly {
		t.Fatalf("unset row = %q; want the construction-time bucket", got)
	}

	reader.failKeys[ConfigPeriodBucketSize] = errors.New("store down")
	if got := a.bucketFor(ctx); got != PeriodBucketMonthly {
		t.Fatalf("failed read = %q; want the construction-time bucket", got)
	}
	delete(reader.failKeys, ConfigPeriodBucketSize)

	reader.set[ConfigPeriodBucketSize] = true
	reader.strings[ConfigPeriodBucketSize] = PeriodBucketDaily
	if got := a.bucketFor(ctx); got != PeriodBucketDaily {
		t.Fatalf("explicit row = %q; want daily", got)
	}

	reader.strings[ConfigPeriodBucketSize] = "yearly"
	if got := a.bucketFor(ctx); got != PeriodBucketMonthly {
		t.Fatalf("unknown value = %q; want the construction-time bucket", got)
	}
}

// TestResolveThreshold_ResolutionArms pins the threshold precedence: a
// per-feature entry (always construction-time) wins; then an explicit
// POSITIVE default row; an explicit ZERO row means "no default threshold"
// (the off switch, matching OverageThresholds.Default's nil); an unset or
// failed read falls back to the construction-time Default.
func TestResolveThreshold_ResolutionArms(t *testing.T) {
	ctx := context.Background()
	constructionDefault := 100.0
	a := &Aggregator{thresholds: OverageThresholds{
		Default:    &constructionDefault,
		PerFeature: map[string]float64{"image.render": 5},
	}}

	if v, ok := a.resolveThreshold(ctx, "image.render"); !ok || v != 5 {
		t.Fatalf("per-feature = (%v, %v); want (5, true)", v, ok)
	}
	if v, ok := a.resolveThreshold(ctx, "other"); !ok || v != 100 {
		t.Fatalf("unset row, construction default = (%v, %v); want (100, true)", v, ok)
	}

	reader := newFakeSettings()
	a.settings = reader
	reader.set[ConfigDefaultOverageThreshold] = true
	reader.ints[ConfigDefaultOverageThreshold] = 7
	if v, ok := a.resolveThreshold(ctx, "other"); !ok || v != 7 {
		t.Fatalf("explicit row = (%v, %v); want (7, true) -- the row must override the construction default", v, ok)
	}
	if v, ok := a.resolveThreshold(ctx, "image.render"); !ok || v != 5 {
		t.Fatalf("per-feature under an explicit default row = (%v, %v); want (5, true)", v, ok)
	}

	// An explicit zero row disables the default threshold outright.
	reader.ints[ConfigDefaultOverageThreshold] = 0
	if _, ok := a.resolveThreshold(ctx, "other"); ok {
		t.Fatal("explicit zero row: threshold reported; want none (zero means no default threshold)")
	}

	// A failed read falls back to the construction-time default.
	reader.failKeys[ConfigDefaultOverageThreshold] = errors.New("store down")
	if v, ok := a.resolveThreshold(ctx, "other"); !ok || v != 100 {
		t.Fatalf("failed read = (%v, %v); want the construction default (100, true)", v, ok)
	}
	delete(reader.failKeys, ConfigDefaultOverageThreshold)

	// With no construction default either, an unset row means no threshold.
	reader.set[ConfigDefaultOverageThreshold] = false
	b := &Aggregator{settings: reader}
	if _, ok := b.resolveThreshold(ctx, "other"); ok {
		t.Fatal("no default anywhere: threshold reported; want none")
	}
}
