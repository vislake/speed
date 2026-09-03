package testutil

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestWidget_GetTenantID_ReturnsTenantIDField(t *testing.T) {
	tests := []struct {
		name string
		w    Widget
		want pkgcore.TenantID
	}{
		{name: "non-empty tenant", w: Widget{TenantID: "tenant-a"}, want: pkgcore.TenantID("tenant-a")},
		{name: "empty tenant", w: Widget{TenantID: ""}, want: pkgcore.TenantID("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.w.GetTenantID(); got != tt.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWidget_AuditResourceType_ReturnsWidget(t *testing.T) {
	if got, want := (Widget{}).AuditResourceType(), "widget"; got != want {
		t.Errorf("AuditResourceType() = %q, want %q", got, want)
	}
}
