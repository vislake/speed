package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newSubscriptionService returns a SubscriptionService wired over a fresh
// test database that already holds one platform-wide Plan, id "plan-1" --
// the id every case below passes as CreateInput.PlanID. Create validates
// PlanID against the PlanStore (see Create's own doc comment), so a
// SubscriptionService under test needs a real, visible Plan row to
// reference, not just an opaque string.
func newSubscriptionService(t *testing.T) *SubscriptionService {
	t.Helper()
	db := newTestDB(t)
	plans := NewPlanStore(db)
	if err := plans.Create(context.Background(), &Plan{ID: "plan-1", Key: "plan-1", Name: "Plan One"}); err != nil {
		t.Fatalf("create plan-1: %v", err)
	}
	return NewSubscriptionService(NewSubscriptionRepository(db), plans, nil)
}

func TestSubscriptionService_Create_StartsAtCreated(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sub.Status != string(SubscriptionStatusCreated) {
		t.Errorf("Status = %q, want %q", sub.Status, SubscriptionStatusCreated)
	}
}

func TestSubscriptionService_LifecycleTransitions(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Activate(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got, err := svc.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(SubscriptionStatusActive) {
		t.Errorf("Status after Activate = %q, want %q", got.Status, SubscriptionStatusActive)
	}

	_, err = svc.MarkPastDue(ctx, sub.ID)
	if err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	_, err = svc.Activate(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Activate (recovery from PastDue): %v", err)
	}
	_, err = svc.Cancel(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, err = svc.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(SubscriptionStatusCanceled) {
		t.Errorf("Status after Cancel = %q, want %q", got.Status, SubscriptionStatusCanceled)
	}
}

func TestSubscriptionService_Transition_IllegalMove_Refused(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Cancel(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Canceled is terminal: nothing may transition out of it.
	_, err = svc.Activate(ctx, sub.ID)
	if !hasCode(err, ErrInvalidSubscriptionTransition.Code) {
		t.Errorf("Activate a canceled subscription: err = %v, want %s", err, ErrInvalidSubscriptionTransition.Code)
	}

	// Created cannot jump straight to PastDue.
	sub2, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.MarkPastDue(ctx, sub2.ID)
	if !hasCode(err, ErrInvalidSubscriptionTransition.Code) {
		t.Errorf("MarkPastDue a Created subscription: err = %v, want %s", err, ErrInvalidSubscriptionTransition.Code)
	}
}

func TestSubscriptionService_Active_FindsTheActiveOne(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	got, err := svc.Active(ctx)
	if err != nil || got != nil {
		t.Fatalf("Active before any subscription exists: %v, %v, want nil, nil", got, err)
	}

	created, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err = svc.Active(ctx)
	if err != nil || got != nil {
		t.Fatalf("Active while still Created: %v, %v, want nil, nil", got, err)
	}

	_, err = svc.Activate(ctx, created.ID)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got, err = svc.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got == nil || got.ID != created.ID {
		t.Errorf("Active = %v, want the activated subscription %q", got, created.ID)
	}
}

func TestSubscriptionService_Get_NotFound(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.Get(ctx, "does-not-exist")
	if !hasCode(err, ErrSubscriptionNotFound.Code) {
		t.Errorf("Get(missing): err = %v, want %s", err, ErrSubscriptionNotFound.Code)
	}
}

func TestSubscriptionService_Transition_PublishesEvent(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	received := make(chan pkgcore.Event, 1)
	bus.Subscribe(EventSubscriptionStatusChanged, func(_ context.Context, evt pkgcore.Event) error {
		received <- evt
		return nil
	})

	db := newTestDB(t)
	plans := NewPlanStore(db)
	if err := plans.Create(context.Background(), &Plan{ID: "plan-1", Key: "plan-1", Name: "Plan One"}); err != nil {
		t.Fatalf("create plan-1: %v", err)
	}
	svc := NewSubscriptionService(NewSubscriptionRepository(db), plans, bus)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Activate(ctx, sub.ID)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	select {
	case evt := <-received:
		payload, ok := evt.Payload.(SubscriptionStatusChangedEvent)
		if !ok {
			t.Fatalf("Payload type = %T, want SubscriptionStatusChangedEvent", evt.Payload)
		}
		if payload.SubscriptionID != sub.ID || payload.ToStatus != string(SubscriptionStatusActive) {
			t.Errorf("payload = %+v, want SubscriptionID=%q ToStatus=%q", payload, sub.ID, SubscriptionStatusActive)
		}
	default:
		t.Fatal("EventSubscriptionStatusChanged was not published")
	}
}

// TestSubscriptionService_Create_RejectsAnotherTenantsCustomPlan reproduces
// the cross-tenant entitlement leak: without this check, a tenant could
// point its own Subscription at another tenant's private, tenant-custom
// Plan (PlanStore has no ambient tenant filter of its own -- Plan's own
// doc comment) and silently inherit that other tenant's negotiated
// Grants/quota limits through EntitlementsService.Check.
func TestSubscriptionService_Create_RejectsAnotherTenantsCustomPlan(t *testing.T) {
	db := newTestDB(t)
	plans := NewPlanStore(db)
	victimsPlan := &Plan{TenantID: "tenant-victim", Key: "negotiated", Name: "Victim's Deal"}
	if err := plans.Create(context.Background(), victimsPlan); err != nil {
		t.Fatalf("create victim's tenant-custom plan: %v", err)
	}

	svc := NewSubscriptionService(NewSubscriptionRepository(db), plans, nil)
	attackerCtx := pkgcore.WithTenant(context.Background(), "tenant-attacker")

	_, err := svc.Create(attackerCtx, CreateInput{PlanID: victimsPlan.ID})
	if !hasCode(err, ErrPlanNotFound.Code) {
		t.Fatalf("Create with another tenant's custom PlanID: err = %v, want %s (never allowed, never a distinguishing error)", err, ErrPlanNotFound.Code)
	}
}

// TestSubscriptionService_Create_AllowsOwnTenantCustomPlan is the positive
// counterpart of the rejection test above: a tenant may still subscribe to
// its OWN tenant-custom Plan.
func TestSubscriptionService_Create_AllowsOwnTenantCustomPlan(t *testing.T) {
	db := newTestDB(t)
	plans := NewPlanStore(db)
	ownPlan := &Plan{TenantID: "tenant-a", Key: "negotiated", Name: "Tenant A's Deal"}
	if err := plans.Create(context.Background(), ownPlan); err != nil {
		t.Fatalf("create tenant-a's own custom plan: %v", err)
	}

	svc := NewSubscriptionService(NewSubscriptionRepository(db), plans, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.Create(ctx, CreateInput{PlanID: ownPlan.ID})
	if err != nil {
		t.Fatalf("Create with the ctx tenant's own custom PlanID: %v", err)
	}
	if sub.PlanID != ownPlan.ID {
		t.Errorf("PlanID = %q, want %q", sub.PlanID, ownPlan.ID)
	}
}

// TestSubscriptionService_Create_UnknownPlanID_Refused pins that a PlanID
// naming no Plan row at all is refused with the same ErrPlanNotFound the
// cross-tenant case above uses, rather than being accepted and left to
// fail later inside EntitlementsService.Check.
func TestSubscriptionService_Create_UnknownPlanID_Refused(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.Create(ctx, CreateInput{PlanID: "does-not-exist"})
	if !hasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Create with an unknown PlanID: err = %v, want %s", err, ErrPlanNotFound.Code)
	}
}

func TestSubscriptionRepository_AssertIsolated(t *testing.T) {
	repo := NewSubscriptionRepository(newTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Subscription {
		return &Subscription{ID: uuid.NewString(), PlanID: "plan-1", Status: string(SubscriptionStatusCreated)}
	})
}
