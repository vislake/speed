package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(serviceTestSchemaItems...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(serviceTestSchemaFlags...) },
		module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			if between := module.Handle(); between != early {
				return errors.New("Handle() changed across Register; a host capturing it at assembly would lose its reads")
			}
			if _, attachErr := module.Attach(r); attachErr != nil {
				return attachErr
			}
			if after := module.Handle(); after != early {
				return errors.New("Handle() changed across Attach; a host capturing it at assembly would lose its reads")
			}
			return nil
		},
	); err != nil {
		t.Fatalf("declare and attach: %v", err)
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
	if err := componenttest.DeclareAll(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.Features.Add(serviceTestSchemaFlags...)
	}); err != nil {
		t.Fatalf("declare the schema: %v", err)
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
	var svc *Service
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(serviceTestSchemaItems...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(serviceTestSchemaFlags...) },
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			svc = attached
			return attachErr
		},
	); err != nil {
		t.Fatalf("declare and attach: %v", err)
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

// TestHandle_TypedReads_FollowTheContextTenant pins the typed reads
// (Duration, Int, String): each resolves the schema default with ok=false,
// an explicit row for the CONTEXT's tenant with ok=true, and a row written
// at another tenant does not leak into a tenant-less read.
func TestHandle_TypedReads_FollowTheContextTenant(t *testing.T) {
	db := openModuleTestDB(t)
	module := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	handle := module.Handle()

	reg := newPlainRegistry()
	var svc *Service
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(serviceTestSchemaItems...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(serviceTestSchemaFlags...) },
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			svc = attached
			return attachErr
		},
	); err != nil {
		t.Fatalf("declare and attach: %v", err)
	}

	ctx := context.Background()

	// Schema defaults resolve with ok=false: "no explicit row produced
	// this", the cue a seam takes to fall back to its own value.
	d, ok, err := handle.Duration(ctx, "brand.welcome_interval")
	if err != nil {
		t.Fatalf("Duration at the schema default: %v", err)
	}
	if ok || d != 90*time.Second {
		t.Fatalf("Duration at the schema default = (%v, ok=%v); want the declared default with ok=false", d, ok)
	}
	n, ok, err := handle.Int(ctx, "billing.retry_limit")
	if err != nil {
		t.Fatalf("Int at the schema default: %v", err)
	}
	if ok || n != 3 {
		t.Fatalf("Int at the schema default = (%d, ok=%v); want the declared default with ok=false", n, ok)
	}
	s, ok, err := handle.String(ctx, "brand.site_name")
	if err != nil {
		t.Fatalf("String at the schema default: %v", err)
	}
	if ok || s != "Smile Studio" {
		t.Fatalf("String at the schema default = (%q, ok=%v); want the declared default with ok=false", s, ok)
	}

	// An explicit system row flips ok to true for the tenant-less read.
	// ScopeSystem writes demand a system context, exactly as they do in
	// production (the platform-scope write gate).
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "ops",
		Purpose: SystemPurposeSystemWrite,
		Ticket:  "ticket-42",
	})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if setErr := svc.Set(sysCtx, ScopeSystem, "brand.site_name", Value{Data: "Platform Studio"}, "ops"); setErr != nil {
		t.Fatalf("Set system row: %v", setErr)
	}
	s, ok, err = handle.String(ctx, "brand.site_name")
	if err != nil {
		t.Fatalf("String after a system row: %v", err)
	}
	if !ok || s != "Platform Studio" {
		t.Fatalf("String after a system row = (%q, ok=%v); want the row's value with ok=true", s, ok)
	}

	// A tenant row is visible only to a context carrying that tenant.
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	if setErr := svc.Set(tenantCtx, ScopeTenant, "billing.retry_limit", Value{Data: int64(7)}, "ops"); setErr != nil {
		t.Fatalf("Set tenant row: %v", setErr)
	}
	n, ok, err = handle.Int(tenantCtx, "billing.retry_limit")
	if err != nil {
		t.Fatalf("Int under the tenant: %v", err)
	}
	if !ok || n != 7 {
		t.Fatalf("Int under the tenant row = (%d, ok=%v); want (7, ok=true)", n, ok)
	}
	n, ok, err = handle.Int(ctx, "billing.retry_limit")
	if err != nil {
		t.Fatalf("Int without a tenant: %v", err)
	}
	if ok || n != 3 {
		t.Fatalf("Int under a tenant-less context = (%d, ok=%v); want the declared default with ok=false (the tenant row must not leak)", n, ok)
	}

	// A wrongly-typed read refuses before the row test, like
	// TenantDuration's own order.
	_, _, typeErr := handle.Int(ctx, "brand.site_name")
	assertCode(t, typeErr, ErrTypedValueMismatch)
	_, _, typeErr = handle.String(ctx, "billing.retry_limit")
	assertCode(t, typeErr, ErrTypedValueMismatch)

	// Unknown keys pass Get's own refusal through.
	_, _, unknownErr := handle.String(ctx, "no.such_key")
	assertCode(t, unknownErr, ErrUnknownKey)
}
