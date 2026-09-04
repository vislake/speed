package metering

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

func validUsageEvent() UsageEvent {
	return UsageEvent{
		TenantID:       "tenant-a",
		Feature:        "ai.generation",
		Quantity:       1,
		IdempotencyKey: "idem-1",
		OccurredAt:     time.Now(),
	}
}

func TestUsageEvent_Validate(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(e *UsageEvent)
		wantCode string
	}{
		{name: "valid event", mutate: func(e *UsageEvent) {}, wantCode: ""},
		{name: "missing tenant", mutate: func(e *UsageEvent) { e.TenantID = "" }, wantCode: ErrMissingTenantID.Code},
		{name: "missing feature", mutate: func(e *UsageEvent) { e.Feature = "" }, wantCode: ErrMissingFeature.Code},
		{name: "missing idempotency key", mutate: func(e *UsageEvent) { e.IdempotencyKey = "" }, wantCode: ErrMissingIdempotencyKey.Code},
		{name: "zero quantity is valid", mutate: func(e *UsageEvent) { e.Quantity = 0 }, wantCode: ""},
		{name: "negative quantity", mutate: func(e *UsageEvent) { e.Quantity = -1 }, wantCode: ErrInvalidQuantity.Code},
		{name: "NaN quantity", mutate: func(e *UsageEvent) { e.Quantity = math.NaN() }, wantCode: ErrInvalidQuantity.Code},
		{name: "infinite quantity", mutate: func(e *UsageEvent) { e.Quantity = math.Inf(1) }, wantCode: ErrInvalidQuantity.Code},
		{
			name: "too many metadata entries",
			mutate: func(e *UsageEvent) {
				m := make(map[string]string, maxMetadataEntries+1)
				for i := 0; i < maxMetadataEntries+1; i++ {
					m[strings.Repeat("k", 1)+string(rune('a'+i))] = "v"
				}
				e.Metadata = m
			},
			wantCode: ErrMetadataTooLarge.Code,
		},
		{
			name: "metadata key too long",
			mutate: func(e *UsageEvent) {
				e.Metadata = map[string]string{strings.Repeat("k", maxMetadataFieldLength+1): "v"}
			},
			wantCode: ErrMetadataTooLarge.Code,
		},
		{
			name: "metadata value too long",
			mutate: func(e *UsageEvent) {
				e.Metadata = map[string]string{"k": strings.Repeat("v", maxMetadataFieldLength+1)}
			},
			wantCode: ErrMetadataTooLarge.Code,
		},
		{
			name: "metadata at the exact bound is valid",
			mutate: func(e *UsageEvent) {
				e.Metadata = map[string]string{strings.Repeat("k", maxMetadataFieldLength): strings.Repeat("v", maxMetadataFieldLength)}
			},
			wantCode: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validUsageEvent()
			tc.mutate(&e)
			err := e.validate()
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("validate() = %v, want an *apperr.Error", err)
			}
			if appErr.Code != tc.wantCode {
				t.Errorf("validate() code = %q, want %q", appErr.Code, tc.wantCode)
			}
		})
	}
}
