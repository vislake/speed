package metering

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestUsageSummary_TableName(t *testing.T) {
	if got := (UsageSummary{}).TableName(); got != tableUsageSummaries {
		t.Errorf("TableName() = %q, want %q", got, tableUsageSummaries)
	}
}

func TestOutboxRecord_TableName(t *testing.T) {
	if got := (OutboxRecord{}).TableName(); got != tableOutboxRecords {
		t.Errorf("TableName() = %q, want %q", got, tableOutboxRecords)
	}
}

func TestUsageSummary_GetTenantID(t *testing.T) {
	tests := []struct {
		name    string
		summary UsageSummary
		want    pkgcore.TenantID
	}{
		{name: "populated", summary: UsageSummary{TenantID: "tenant-a"}, want: "tenant-a"},
		{name: "zero value", summary: UsageSummary{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.summary.GetTenantID(); got != tc.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tc.want)
			}
		})
	}
}
