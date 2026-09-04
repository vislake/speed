package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func newSubscriptionService(t *testing.T) *SubscriptionService {
	t.Helper()
	return NewSubscriptionService(NewSubscriptionRepository(newTestDB(t)), nil)
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

	svc := NewSubscriptionService(NewSubscriptionRepository(newTestDB(t)), bus)
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

func TestSubscriptionRepository_AssertIsolated(t *testing.T) {
	repo := NewSubscriptionRepository(newTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Subscription {
		return &Subscription{ID: uuid.NewString(), PlanID: "plan-1", Status: string(SubscriptionStatusCreated)}
	})
}
