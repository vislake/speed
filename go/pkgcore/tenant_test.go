package pkgcore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestWithTenant_TenantFromContext_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		tenant TenantID
	}{
		{name: "ulid style id", tenant: TenantID("01HQ8Z3XK9V0T7M2P5RGBW4NCD")},
		{name: "slug style id", tenant: TenantID("acme-corp")},
		{name: "single character id", tenant: TenantID("t")},
		// Escaped rather than written literally: the repository forbids CJK
		// characters in source outside docs/internal, and what this case
		// exercises is the bytes surviving the round trip, not the encoding
		// of this file.
		{name: "unicode id", tenant: TenantID("\u79df\u6237-1")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := WithTenant(context.Background(), tt.tenant)

			got, ok := TenantFromContext(ctx)
			if !ok {
				t.Fatalf("TenantFromContext reported no tenant after WithTenant(%q)", tt.tenant)
			}
			if got != tt.tenant {
				t.Errorf("TenantFromContext = %q, want %q", got, tt.tenant)
			}

			mustGot, err := MustTenantFromContext(ctx)
			if err != nil {
				t.Fatalf("MustTenantFromContext returned unexpected error: %v", err)
			}
			if mustGot != tt.tenant {
				t.Errorf("MustTenantFromContext = %q, want %q", mustGot, tt.tenant)
			}
		})
	}
}

func TestWithTenant_OverwritesEnclosingTenant(t *testing.T) {
	const (
		outer TenantID = "tenant-outer"
		inner TenantID = "tenant-inner"
	)

	outerCtx := WithTenant(context.Background(), outer)
	innerCtx := WithTenant(outerCtx, inner)

	got, ok := TenantFromContext(innerCtx)
	if !ok || got != inner {
		t.Errorf("inner TenantFromContext = (%q, %t), want (%q, true)", got, ok, inner)
	}
	// The enclosing context must be untouched: context values are immutable.
	got, ok = TenantFromContext(outerCtx)
	if !ok || got != outer {
		t.Errorf("outer TenantFromContext = (%q, %t), want (%q, true)", got, ok, outer)
	}
}

func TestTenantFromContext_NoUsableTenant_ReportsAbsent(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "background context", ctx: context.Background()},
		{name: "empty tenant is not a tenant", ctx: WithTenant(context.Background(), TenantID(""))},
		{
			name: "value of a foreign type under the tenant key",
			ctx:  context.WithValue(context.Background(), ctxKeyTenant, "plain string, not a TenantID"),
		},
		{
			name: "system context alone carries no tenant",
			ctx:  context.WithValue(context.Background(), ctxKeySystemReason, SystemReason{Actor: "ops"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := TenantFromContext(tt.ctx)
			if ok {
				t.Errorf("TenantFromContext reported tenant %q, want none", got)
			}
			if got != "" {
				t.Errorf("TenantFromContext = %q, want the zero TenantID", got)
			}
		})
	}
}

func TestMustTenantFromContext_NoTenant_ReturnsDescriptiveError(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "background context", ctx: context.Background()},
		{name: "empty tenant", ctx: WithTenant(context.Background(), TenantID(""))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MustTenantFromContext(tt.ctx)
			if err == nil {
				t.Fatal("MustTenantFromContext returned no error, want one (it must fail closed)")
			}
			if got != "" {
				t.Errorf("MustTenantFromContext = %q on failure, want the zero TenantID", got)
			}
			if !errors.Is(err, ErrNoTenant) {
				t.Errorf("error %v does not match ErrNoTenant", err)
			}

			// The message has to tell a reader what is missing and how to
			// supply it, since this error surfaces far from its cause.
			msg := err.Error()
			for _, want := range []string{"tenant", "context", "WithTenant"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestRegisterSystemPurpose_Idempotent(t *testing.T) {
	const purpose SystemPurpose = "pkgcore_test.idempotent_registration"
	const registrations = 3

	for range registrations {
		RegisterSystemPurpose(purpose)
	}

	ctx, err := WithSystemContext(context.Background(), SystemReason{Actor: "ops", Purpose: purpose})
	if err != nil {
		t.Fatalf("WithSystemContext rejected a repeatedly registered purpose: %v", err)
	}
	if _, ok := SystemReasonFromContext(ctx); !ok {
		t.Error("SystemReasonFromContext reported no reason after a successful WithSystemContext")
	}
}

func TestRegisterSystemPurpose_EmptyPurposeIsNeverGranted(t *testing.T) {
	RegisterSystemPurpose(SystemPurpose(""))

	_, err := WithSystemContext(context.Background(), SystemReason{Actor: "ops"})
	if !errors.Is(err, ErrSystemPurposeNotRegistered) {
		t.Errorf("WithSystemContext with an empty purpose returned %v, want ErrSystemPurposeNotRegistered", err)
	}
}

func TestWithSystemContext(t *testing.T) {
	const (
		registeredPurpose SystemPurpose = "pkgcore_test.tenant_search"
		otherRegistered   SystemPurpose = "pkgcore_test.data_export"
	)
	RegisterSystemPurpose(registeredPurpose)
	RegisterSystemPurpose(otherRegistered)

	tests := []struct {
		name    string
		reason  SystemReason
		wantErr error // nil means the call must succeed
	}{
		{
			name:   "registered purpose with actor",
			reason: SystemReason{Actor: "admin@example.com", Purpose: registeredPurpose, Ticket: "SUP-1234"},
		},
		{
			name:   "ticket is optional",
			reason: SystemReason{Actor: "jobs-worker", Purpose: otherRegistered},
		},
		{
			name:    "unregistered purpose is rejected",
			reason:  SystemReason{Actor: "admin@example.com", Purpose: SystemPurpose("pkgcore_test.never_registered")},
			wantErr: ErrSystemPurposeNotRegistered,
		},
		{
			name:    "empty purpose is rejected",
			reason:  SystemReason{Actor: "admin@example.com"},
			wantErr: ErrSystemPurposeNotRegistered,
		},
		{
			name:    "empty actor is rejected even for a registered purpose",
			reason:  SystemReason{Purpose: registeredPurpose, Ticket: "SUP-1234"},
			wantErr: ErrSystemActorRequired,
		},
		{
			name:    "missing actor is reported before an unregistered purpose",
			reason:  SystemReason{Purpose: SystemPurpose("pkgcore_test.never_registered")},
			wantErr: ErrSystemActorRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := context.Background()
			ctx, err := WithSystemContext(parent, tt.reason)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("WithSystemContext error = %v, want one matching %v", err, tt.wantErr)
				}
				if ctx == nil {
					t.Fatal("WithSystemContext returned a nil context on failure")
				}
				// Failing closed: a caller that ignores the error must not
				// end up holding the escape hatch.
				if reason, ok := SystemReasonFromContext(ctx); ok {
					t.Errorf("failed WithSystemContext still granted system context %+v", reason)
				}
				return
			}

			if err != nil {
				t.Fatalf("WithSystemContext returned unexpected error: %v", err)
			}
			got, ok := SystemReasonFromContext(ctx)
			if !ok {
				t.Fatal("SystemReasonFromContext reported no reason after a successful call")
			}
			if got != tt.reason {
				t.Errorf("SystemReasonFromContext = %+v, want %+v", got, tt.reason)
			}
			if _, ok := SystemReasonFromContext(parent); ok {
				t.Error("WithSystemContext mutated the parent context")
			}
		})
	}
}

func TestSystemReasonFromContext_NoSystemContext_ReportsAbsent(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "background context", ctx: context.Background()},
		{name: "tenant context alone", ctx: WithTenant(context.Background(), TenantID("tenant-1"))},
		{
			name: "value of a foreign type under the system key",
			ctx:  context.WithValue(context.Background(), ctxKeySystemReason, "plain string, not a SystemReason"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SystemReasonFromContext(tt.ctx)
			if ok {
				t.Errorf("SystemReasonFromContext reported %+v, want none", got)
			}
			if got != (SystemReason{}) {
				t.Errorf("SystemReasonFromContext = %+v, want the zero SystemReason", got)
			}
		})
	}
}

func TestTenantAndSystemContext_AreIndependent(t *testing.T) {
	const (
		purpose SystemPurpose = "pkgcore_test.independence"
		tenant  TenantID      = "tenant-42"
	)
	RegisterSystemPurpose(purpose)
	reason := SystemReason{Actor: "compliance-bot", Purpose: purpose, Ticket: "SUP-7"}

	t.Run("tenant context carries no system reason", func(t *testing.T) {
		ctx := WithTenant(context.Background(), tenant)
		if got, ok := SystemReasonFromContext(ctx); ok {
			t.Errorf("WithTenant also granted system context %+v", got)
		}
	})

	t.Run("system context carries no tenant", func(t *testing.T) {
		ctx, err := WithSystemContext(context.Background(), reason)
		if err != nil {
			t.Fatalf("WithSystemContext returned unexpected error: %v", err)
		}
		if got, ok := TenantFromContext(ctx); ok {
			t.Errorf("WithSystemContext also set tenant %q", got)
		}
		if _, err := MustTenantFromContext(ctx); !errors.Is(err, ErrNoTenant) {
			t.Errorf("MustTenantFromContext on a system context returned %v, want ErrNoTenant", err)
		}
	})

	t.Run("both can be combined", func(t *testing.T) {
		ctx, err := WithSystemContext(WithTenant(context.Background(), tenant), reason)
		if err != nil {
			t.Fatalf("WithSystemContext returned unexpected error: %v", err)
		}
		gotTenant, ok := TenantFromContext(ctx)
		if !ok || gotTenant != tenant {
			t.Errorf("TenantFromContext = (%q, %t), want (%q, true)", gotTenant, ok, tenant)
		}
		gotReason, ok := SystemReasonFromContext(ctx)
		if !ok || gotReason != reason {
			t.Errorf("SystemReasonFromContext = (%+v, %t), want (%+v, true)", gotReason, ok, reason)
		}
	})
}

func TestRegisterSystemPurpose_ConcurrentUse(t *testing.T) {
	const purpose SystemPurpose = "pkgcore_test.concurrent"
	const goroutines = 16

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			RegisterSystemPurpose(purpose)
			// Readers run alongside writers so -race covers both paths.
			_, _ = WithSystemContext(context.Background(), SystemReason{Actor: "ops", Purpose: purpose})
		}()
	}
	wg.Wait()

	if _, err := WithSystemContext(context.Background(), SystemReason{Actor: "ops", Purpose: purpose}); err != nil {
		t.Errorf("WithSystemContext rejected a concurrently registered purpose: %v", err)
	}
}
