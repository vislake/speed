package integration

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

func newTestRegistry(t *testing.T) *pkgcore.Registry {
	t.Helper()
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

func TestModule_Name(t *testing.T) {
	m := NewModule(newTestDB(t))
	if m.Name() != moduleName {
		t.Errorf("Name() = %q, want %q", m.Name(), moduleName)
	}
}

func TestModule_DependsOn_Empty(t *testing.T) {
	m := NewModule(newTestDB(t))
	if got := m.DependsOn(); len(got) != 0 {
		t.Errorf("DependsOn() = %v, want empty", got)
	}
}

func TestModule_Register_DeclaresPermissionsAndAuditActions(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)

	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionManage, PermissionWebhookRead, PermissionWebhookManage} {
		if !contains(perms, want) {
			t.Errorf("Permissions() = %v, want %q present", perms, want)
		}
	}

	actions := reg.AuditActions.Actions()
	for _, want := range auditActionDecls {
		if !contains(actions, want) {
			t.Errorf("Actions() = %v, want %q present", actions, want)
		}
	}
}

// TestModule_Register_RegistersWebhookDeliveryJobHandler proves round 2's
// job-handler wiring: exactly one handler is registered, under
// jobTypeWebhookDeliver, matching storage's and notification's identical
// "job handlers" Register assertion shape.
func TestModule_Register_RegistersWebhookDeliveryJobHandler(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handlers := reg.Jobs.Handlers()
	if len(handlers) != 1 {
		t.Fatalf("len(handlers) = %d, want 1", len(handlers))
	}
	if _, ok := handlers[jobTypeWebhookDeliver]; !ok {
		t.Errorf("handlers = %v, want %q present", handlers, jobTypeWebhookDeliver)
	}
}

// TestModule_Register_SubscribesEveryDeclaredEventMapping proves
// WithEventMapping's InternalType actually reaches reg.Events.Subscribe: a
// domain event published on the host's own bus, after Register but before
// Attach, is observed (Module.handleDomainEvent's own nil-Service guard
// logs and swallows it rather than panicking -- this test only proves the
// SUBSCRIPTION happened, not delivery, which webhook_delivery_test.go
// covers against a real Attach-ed Service).
func TestModule_Register_SubscribesEveryDeclaredEventMapping(t *testing.T) {
	published := make(chan struct{}, 1)
	mapping := EventMapping{
		InternalType:  "test.subscribe.proof",
		PublicType:    "test.subscribe.proof",
		PublicVersion: "v1",
		Transform:     fixedTransform(nil),
	}
	m := NewModule(newTestDB(t), WithEventMapping(mapping))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reg.Events.Subscribe(mapping.InternalType, func(context.Context, pkgcore.Event) error {
		published <- struct{}{}
		return nil
	})
	if err := reg.EventBus().Publish(context.Background(), pkgcore.Event{Type: mapping.InternalType}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-published:
	default:
		t.Error("the second subscriber never observed the event -- Module's own subscription may not have registered on the same bus")
	}
}

func TestModule_Register_DuplicateEventMapping_Refused(t *testing.T) {
	dup := EventMapping{InternalType: "x", PublicType: "y", PublicVersion: "v1", Transform: fixedTransform(nil)}
	m := NewModule(newTestDB(t), WithEventMapping(dup, dup))
	reg := newTestRegistry(t)
	if err := m.Register(reg); !apperrIs(err, ErrDuplicateEventMapping) {
		t.Errorf("Register error = %v, want ErrDuplicateEventMapping", err)
	}
}

func TestModule_Attach_BuildsWorkingService(t *testing.T) {
	m := NewModule(newTestDB(t), WithPermissionLister(alwaysHeld("notes:read")))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if svc == nil {
		t.Fatal("Attach returned a nil *Service")
	}

	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", Scopes: []string{"notes:read"}}); err != nil {
		t.Errorf("Create through an Attach-built Service: %v", err)
	}
}

func TestModule_Attach_SecondCall_Refused(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Attach(reg); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if _, err := m.Attach(reg); !apperrIs(err, ErrAlreadyAttached) {
		t.Errorf("second Attach error = %v, want ErrAlreadyAttached", err)
	}
}

func TestModule_Attach_NilRegistry_Refused(t *testing.T) {
	m := NewModule(newTestDB(t))
	if _, err := m.Attach(nil); err == nil {
		t.Error("Attach(nil) returned no error")
	}
}

func TestModule_ImplementsPkgcoreModule(t *testing.T) {
	var _ pkgcore.Module = NewModule(newTestDB(t))
}

// TestModule_Attach_UsesInjectedClock proves withClock actually reaches the
// Service Attach builds, by pinning a fixed "now" through NewModule and
// checking Create's default expiry lands exactly at that fixed instant plus
// MaxAPIKeyLifetime -- an assertion that would flake against the wall clock
// without this seam, per withClock's own doc comment.
func TestModule_Attach_UsesInjectedClock(t *testing.T) {
	m := NewModule(newTestDB(t), withClock(func() time.Time { return fixedNow }))
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := fixedNow.Add(MaxAPIKeyLifetime)
	if !created.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (computed from the injected clock, not the wall clock)", created.ExpiresAt, want)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
