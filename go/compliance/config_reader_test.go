package compliance

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Tests for config_reader.go: the reader's fail-closed half. The live half
// -- a tenant's configured value actually governing an export's expiry --
// runs in ExampleNewConfigReader (compiled and executed by go test) and,
// against the real assembly, in
// examples/reference-app/flowtests/tenant_config_reader_flow_test.go.

// TestConfigReader_BeforeConfigAttach_FailsClosed pins the reader's
// behavior in the wiring window it exists for: a reader captured before the
// config module is attached must report the config module's own
// not-attached refusal, never an ok=false answer a caller could mistake for
// "the tenant configured nothing".
func TestConfigReader_BeforeConfigAttach_FailsClosed(t *testing.T) {
	reader := NewConfigReader(config.NewModule(nil).Handle())

	d, ok, err := reader.ExportDeliveryExpiry(context.Background(), pkgcore.TenantID("tenant-a"))
	if err == nil {
		t.Fatal("ExportDeliveryExpiry before the config module is attached = nil error, want the refused read")
	}
	if !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
		t.Fatalf("ExportDeliveryExpiry before attach error = %v, want code %q", err, config.ErrServiceNotAttached.Code)
	}
	if ok || d != 0 {
		t.Fatalf("ExportDeliveryExpiry before attach = (%v, ok=%v), want the refused zero values", d, ok)
	}
}

// TestConfigReader_NilHandle_FailsClosed pins the documented nil-handle
// behavior: a host that wired no handle at all gets the same coded refusal
// rather than a panic.
func TestConfigReader_NilHandle_FailsClosed(t *testing.T) {
	reader := NewConfigReader(nil)

	if _, _, err := reader.ExportDeliveryExpiry(context.Background(), pkgcore.TenantID("tenant-a")); !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
		t.Fatalf("ExportDeliveryExpiry through a nil handle = %v, want code %q", err, config.ErrServiceNotAttached.Code)
	}
}
