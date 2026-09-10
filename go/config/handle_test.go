package config

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Tests for handle.go: the lazy read handle's identity across the module's
// life, its fail-closed refusal in every not-yet-attached state (before
// Attach, after a failed Attach, and through a nil handle), and -- the
// point of the type -- reads resolving through the Service once Attach has
// produced it.

// TestModule_Handle_IsStableAcrossTheModuleLife pins the handle's contract:
// the same non-nil handle is returned before Register, between Register and
// Attach, and after Attach, so a host can capture it once at assembly and
// hold it for the module's whole life.
func TestModule_Handle_IsStableAcrossTheModuleLife(t *testing.T) {
	db := openModuleTestDB(t)
	module := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))

	early := module.Handle()
	if early == nil {
		t.Fatal("Handle() before Attach = nil; the handle exists from NewModule on")
	}

	reg := newPlainRegistry()
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if between := module.Handle(); between != early {
		t.Fatal("Handle() changed across Register; a host capturing it at assembly would lose its reads")
	}
	if _, err := module.Attach(reg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if after := module.Handle(); after != early {
		t.Fatal("Handle() changed across Attach; a host capturing it at assembly would lose its reads")
	}
}

// TestHandle_ReadsBeforeAttach_FailClosedWithErrServiceNotAttached pins the
// refusal every read reports in the window before Attach: the module's own
// coded not-attached error, never a zero-value answer a caller could mistake
// for a real one.
func TestHandle_ReadsBeforeAttach_FailClosedWithErrServiceNotAttached(t *testing.T) {
	handle := NewModule(nil).Handle()

	enabled, err := handle.IsEnabled(context.Background(), "ai.smile_preview")
	assertCode(t, err, ErrServiceNotAttached)
	if enabled {
		t.Fatal("IsEnabled before Attach = true; want the refused zero value")
	}

	d, ok, err := handle.TenantDuration(context.Background(), "brand.welcome_interval", "tenant-a")
	assertCode(t, err, ErrServiceNotAttached)
	if ok || d != 0 {
		t.Fatalf("TenantDuration before Attach = (%v, %v); want the refused zero values", d, ok)
	}
}

// TestHandle_ReadsAfterAFailedAttach_FailClosedWithErrServiceNotAttached
// pins the second not-attached state: an Attach that never produced a
// Service (here: no database) leaves the handle reading exactly like the
// pre-Attach window, matching ErrServiceNotAttached's own "or whose Attach
// failed" contract.
func TestHandle_ReadsAfterAFailedAttach_FailClosedWithErrServiceNotAttached(t *testing.T) {
	reg := newPlainRegistry()
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	module := NewModule(nil, WithPollInterval(0))
	if _, err := module.Attach(reg); err == nil {
		t.Fatal("Attach without a database succeeded; the handle test needs a failed Attach")
	}

	_, err := module.Handle().IsEnabled(context.Background(), "ai.smile_preview")
	assertCode(t, err, ErrServiceNotAttached)
}

// TestHandle_NilHandle_ReadsFailClosed pins the nil handle's documented
// behavior: a host that never wired a real one gets the same coded refusal,
// not a nil-pointer panic.
func TestHandle_NilHandle_ReadsFailClosed(t *testing.T) {
	var handle *Handle
	_, err := handle.IsEnabled(context.Background(), "ai.smile_preview")
	assertCode(t, err, ErrServiceNotAttached)

	_, _, err = handle.TenantDuration(context.Background(), "brand.welcome_interval", "tenant-a")
	assertCode(t, err, ErrServiceNotAttached)
}

// TestHandle_ReadsAfterAttach_ResolveThroughTheService is the positive half:
// once Attach has run, both handle reads answer exactly as the Service
// methods they resolve to, including the error passthrough for a key the
// schema does not carry and the ok result that distinguishes a configured
// duration row from the schema default.
func TestHandle_ReadsAfterAttach_ResolveThroughTheService(t *testing.T) {
	db := openModuleTestDB(t)
	module := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	handle := module.Handle()

	reg := newPlainRegistry()
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	ctx := context.Background()
	// The flag's declared default is the answer until a row overrides it.
	enabled, err := handle.IsEnabled(ctx, "ai.smile_preview")
	if err != nil {
		t.Fatalf("IsEnabled: %v", err)
	}
	if enabled {
		t.Fatal("IsEnabled(ai.smile_preview) = true before any row; want the declared default false")
	}
	// A key the schema does not carry passes the Service's own error through.
	_, unknownErr := handle.IsEnabled(ctx, "ai.no_such_flag")
	if unknownErr == nil {
		t.Fatal("IsEnabled(unknown key) = nil error; want the Service's own ErrUnknownFlag")
	}
	assertCode(t, unknownErr, ErrUnknownFlag)

	// The duration item's schema default resolves with ok = false -- the
	// "this tenant has configured none" answer a tenant-configurable seam
	// takes as its cue to fall back to its own default -- and a tenant row
	// flips ok to true with the row's value.
	d, ok, err := handle.TenantDuration(ctx, "brand.welcome_interval", "tenant-a")
	if err != nil {
		t.Fatalf("TenantDuration: %v", err)
	}
	if ok || d != 0 {
		t.Fatalf("TenantDuration at the schema default = (%v, ok=%v); want the unconfigured zero values", d, ok)
	}
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	if setErr := svc.Set(tenantCtx, ScopeTenant, "brand.welcome_interval", Value{Data: 5 * time.Minute}, "alice"); setErr != nil {
		t.Fatalf("Set: %v", setErr)
	}
	d, ok, err = handle.TenantDuration(ctx, "brand.welcome_interval", "tenant-a")
	if err != nil {
		t.Fatalf("TenantDuration after Set: %v", err)
	}
	if !ok || d != 5*time.Minute {
		t.Fatalf("TenantDuration after a tenant row = (%v, ok=%v); want (5m, ok=true)", d, ok)
	}
}
