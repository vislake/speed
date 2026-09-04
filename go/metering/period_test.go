package metering

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

func mustUTC(t *testing.T, layout, value string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, value)
	if err != nil {
		t.Fatalf("time.Parse(%q, %q): %v", layout, value, err)
	}
	return tm
}

func TestPeriodBounds_Daily(t *testing.T) {
	at := mustUTC(t, time.RFC3339, "2026-09-04T15:04:05Z")
	start, end, err := periodBounds(at, PeriodBucketDaily)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	wantStart := mustUTC(t, time.RFC3339, "2026-09-04T00:00:00Z")
	wantEnd := mustUTC(t, time.RFC3339, "2026-09-05T00:00:00Z")
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

func TestPeriodBounds_Monthly(t *testing.T) {
	at := mustUTC(t, time.RFC3339, "2026-09-04T15:04:05Z")
	start, end, err := periodBounds(at, PeriodBucketMonthly)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	wantStart := mustUTC(t, time.RFC3339, "2026-09-01T00:00:00Z")
	wantEnd := mustUTC(t, time.RFC3339, "2026-10-01T00:00:00Z")
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

// TestPeriodBounds_Monthly_DecemberRollsIntoNextYear pins the year
// boundary: AddDate(0, 1, 0) from December must land on January of the
// following year, not month "13" of the same one.
func TestPeriodBounds_Monthly_DecemberRollsIntoNextYear(t *testing.T) {
	at := mustUTC(t, time.RFC3339, "2026-12-15T00:00:00Z")
	_, end, err := periodBounds(at, PeriodBucketMonthly)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	want := mustUTC(t, time.RFC3339, "2027-01-01T00:00:00Z")
	if !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

func TestPeriodBounds_NonUTCInputIsNormalized(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)
	// 2026-09-04T23:00:00+09:00 is 2026-09-04T14:00:00Z -- still the same
	// UTC calendar day, proving periodBounds normalizes to UTC before
	// truncating rather than truncating in the input's own zone.
	at := time.Date(2026, 9, 4, 23, 0, 0, 0, loc)
	start, _, err := periodBounds(at, PeriodBucketDaily)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	want := mustUTC(t, time.RFC3339, "2026-09-04T00:00:00Z")
	if !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
}

func TestPeriodBounds_InvalidBucket(t *testing.T) {
	_, _, err := periodBounds(time.Now(), "weekly")
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrInvalidPeriodBucket.Code {
		t.Fatalf("periodBounds with an invalid bucket = %v, want %s", err, ErrInvalidPeriodBucket.Code)
	}
}

func TestSummaryID_DeterministicAndDistinctPerFeatureAndPeriod(t *testing.T) {
	p1 := mustUTC(t, time.RFC3339, "2026-09-01T00:00:00Z")
	p2 := mustUTC(t, time.RFC3339, "2026-10-01T00:00:00Z")

	if got, want := summaryID("ai.generation", p1), summaryID("ai.generation", p1); got != want {
		t.Errorf("summaryID is not deterministic: %q != %q", got, want)
	}
	if summaryID("ai.generation", p1) == summaryID("api.calls", p1) {
		t.Error("summaryID collided across two different features in the same period")
	}
	if summaryID("ai.generation", p1) == summaryID("ai.generation", p2) {
		t.Error("summaryID collided across two different periods for the same feature")
	}
}
