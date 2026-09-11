package integration

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

func newTestRegistry(t *testing.T) *pkgcore.ComponentRegistry {
	t.Helper()
	return componenttest.NewRegistry()
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

func TestModule_Register_DeclaresTheAPIKeyExpirySweepSchedule(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)

	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}

	decls := reg.Schedules.Declarations()
	if len(decls) != 1 || decls[0] != apiKeyExpirySweepSchedule {
		t.Errorf("Register declared %+v, want exactly the API-key expiry-sweep schedule %+v", decls, apiKeyExpirySweepSchedule)
	}
}

func TestModule_Register_DeclaresPermissionsAndAuditActions(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)

	if err := componenttest.DeclareInto(reg, m); err != nil {
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

// TestModule_AuditActionVocabulary_HasNoDeadEntries pins the registered
// audit-action vocabulary to exactly the actions some Service call emits:
// Service.Rotate records a rotation as its two ordinary create/revoke
// events (see that method's own doc comment), so a registered
// integration.apikey.rotate action would be a dead vocabulary entry --
// declared, validated-against, but never emitted -- and is deliberately
// absent. The exact-list shape makes a reintroduced-but-unemitted action
// fail here.
func TestModule_AuditActionVocabulary_HasNoDeadEntries(t *testing.T) {
	want := []string{
		AuditActionAPIKeyCreate,
		AuditActionAPIKeyRevoke,
		AuditActionWebhookSubscriptionCreate,
		AuditActionWebhookSubscriptionUpdate,
		AuditActionWebhookSubscriptionDelete,
		AuditActionWebhookSubscriptionRestore,
	}
	if len(auditActionDecls) != len(want) {
		t.Fatalf("auditActionDecls = %v, want exactly %v (no action may be registered that nothing emits)", auditActionDecls, want)
	}
	for i, w := range want {
		if auditActionDecls[i] != w {
			t.Fatalf("auditActionDecls = %v, want exactly %v (no action may be registered that nothing emits)", auditActionDecls, want)
		}
	}
}

// TestModule_Register_RegistersJobHandlers proves the module's job-handler
// wiring: exactly the two handlers this module enqueues tasks under are
// registered, under jobTypeWebhookDeliver (the webhook delivery pipeline)
// and jobTypeAPIKeyExpirySweep (the expiry-sweep task, apikey_sweep.go),
// matching storage's and notification's identical "job handlers" Register
// assertion shape. A third registered type would mean a task nothing ever
// enqueues, and an enqueued task with no registered handler could never
// run.
func TestModule_Register_RegistersJobHandlers(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg := newTestRegistry(t)
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handlers := reg.Jobs.Handlers()
	if len(handlers) != 2 {
		t.Fatalf("len(handlers) = %d, want 2", len(handlers))
	}
	for _, want := range []string{jobTypeWebhookDeliver, jobTypeAPIKeyExpirySweep} {
		if _, ok := handlers[want]; !ok {
			t.Errorf("handlers = %v, want %q present", handlers, want)
		}
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
	if err := componenttest.DeclareInto(reg, m); err != nil {
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
	if err := componenttest.DeclareInto(reg, m); !apperr.HasCode(err, ErrDuplicateEventMapping.Code) {
		t.Errorf("Register error = %v, want ErrDuplicateEventMapping", err)
	}
}

func TestModule_Attach_BuildsWorkingService(t *testing.T) {
	m := NewModule(newTestDB(t), WithPermissionLister(alwaysHeld("notes:read")))
	reg := newTestRegistry(t)
	if err := componenttest.DeclareInto(reg, m); err != nil {
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
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Attach(reg); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if _, err := m.Attach(reg); !apperr.HasCode(err, ErrAlreadyAttached.Code) {
		t.Errorf("second Attach error = %v, want ErrAlreadyAttached", err)
	}
}

func TestModule_Attach_NilRegistry_Refused(t *testing.T) {
	m := NewModule(newTestDB(t))
	if _, err := m.Attach(nil); err == nil {
		t.Error("Attach(nil) returned no error")
	}
}

// TestModule_Attach_UsesInjectedClock proves withClock actually reaches the
// Service Attach builds, by pinning a fixed "now" through NewModule and
// checking Create's default expiry lands exactly at that fixed instant plus
// MaxAPIKeyLifetime -- an assertion that would flake against the wall clock
// without this seam, per withClock's own doc comment.
func TestModule_Attach_UsesInjectedClock(t *testing.T) {
	m := NewModule(newTestDB(t), withClock(func() time.Time { return fixedNow }))
	reg := newTestRegistry(t)
	if err := componenttest.DeclareInto(reg, m); err != nil {
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

// TestModule_WithMaxAPIKeyLifetime_OptionStoresOnlyPositiveValues pins the
// option's guard: a positive duration is stored as the Module's configured
// lifetime, while a zero or negative one is ignored and leaves the zero
// value standing -- which the Service resolves as the MaxAPIKeyLifetime
// package default (see maxAPIKeyLifetime). A value nobody can configure
// away by accident is a value enforcement can trust.
func TestModule_WithMaxAPIKeyLifetime_OptionStoresOnlyPositiveValues(t *testing.T) {
	m := NewModule(nil, WithMaxAPIKeyLifetime(30*24*time.Hour))
	if m.maxLifetime != 30*24*time.Hour {
		t.Errorf("maxLifetime = %v, want 720h0m0s", m.maxLifetime)
	}

	for _, nonsense := range []time.Duration{0, -time.Hour} {
		m := NewModule(nil, WithMaxAPIKeyLifetime(nonsense))
		if m.maxLifetime != 0 {
			t.Errorf("WithMaxAPIKeyLifetime(%v): maxLifetime = %v, want 0 (ignored, default stands)", nonsense, m.maxLifetime)
		}
	}
}

// TestModule_WithMaxAPIKeyLifetime_ConfiguredValue_FlowsToAttachedService
// proves the option's value actually reaches the Service a real
// Register+Attach builds, through the same wiring a host composes -- the
// field Service.Create reads (via maxAPIKeyLifetime) is the Module's
// configured one, and a Module that configured nothing hands over the zero
// value the Service resolves as the package default.
func TestModule_WithMaxAPIKeyLifetime_ConfiguredValue_FlowsToAttachedService(t *testing.T) {
	svc := attachedService(t, WithMaxAPIKeyLifetime(7*24*time.Hour))
	if svc.maxLifetime != 7*24*time.Hour {
		t.Errorf("attached svc.maxLifetime = %v, want 168h0m0s", svc.maxLifetime)
	}

	defaultSvc := attachedService(t)
	if defaultSvc.maxLifetime != 0 {
		t.Errorf("attached svc.maxLifetime = %v, want 0 (unconfigured)", defaultSvc.maxLifetime)
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
