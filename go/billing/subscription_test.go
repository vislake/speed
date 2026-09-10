package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
	svc, _ := newSubscriptionServiceWithPlans(t, "plan-1")
	return svc
}

// newSubscriptionServiceWithPlans mirrors newSubscriptionService for cases
// that need more than one Plan row -- the EnsureActive cases below name a
// second (or an unknown) Plan id -- and also returns the repository, so a
// case can assert on the tenant's full subscription list, not only on the
// Active read.
func newSubscriptionServiceWithPlans(t *testing.T, planIDs ...string) (*SubscriptionService, *SubscriptionRepository) {
	t.Helper()
	db := newTestDB(t)
	plans := NewPlanStore(db)
	for _, id := range planIDs {
		if err := plans.Create(context.Background(), &Plan{ID: id, Key: id, Name: id}); err != nil {
			t.Fatalf("create plan %s: %v", id, err)
		}
	}
	repo := NewSubscriptionRepository(db)
	return NewSubscriptionService(repo, plans, nil), repo
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
	if !apperr.HasCode(err, ErrInvalidSubscriptionTransition.Code) {
		t.Errorf("Activate a canceled subscription: err = %v, want %s", err, ErrInvalidSubscriptionTransition.Code)
	}

	// Created cannot jump straight to PastDue.
	sub2, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.MarkPastDue(ctx, sub2.ID)
	if !apperr.HasCode(err, ErrInvalidSubscriptionTransition.Code) {
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

// TestSubscriptionService_EnsureActive_NoActive_CreatesAndActivates pins
// the create half of EnsureActive's contract: a tenant with no Active
// subscription gets one created against in.PlanID and activated, persisted
// as a real row the Active read then finds.
func TestSubscriptionService_EnsureActive_NoActive_CreatesAndActivates(t *testing.T) {
	svc, _ := newSubscriptionServiceWithPlans(t, "plan-1")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	sub, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}
	if sub.Status != string(SubscriptionStatusActive) {
		t.Errorf("ensured subscription Status = %q, want %q", sub.Status, SubscriptionStatusActive)
	}
	if sub.PlanID != "plan-1" {
		t.Errorf("ensured subscription PlanID = %q, want %q", sub.PlanID, "plan-1")
	}

	active, err := svc.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil || active.ID != sub.ID {
		t.Errorf("Active after EnsureActive = %v, want the ensured subscription %q", active, sub.ID)
	}
}

// TestSubscriptionService_EnsureActive_UnknownPlan_Refused pins that the
// create path validates in.PlanID through the same Create lookup every
// creation uses: a tenant with no Active row and an unknown Plan id is
// refused with the same ErrPlanNotFound Create reports.
func TestSubscriptionService_EnsureActive_UnknownPlan_Refused(t *testing.T) {
	svc, _ := newSubscriptionServiceWithPlans(t, "plan-1")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.EnsureActive(ctx, CreateInput{PlanID: "does-not-exist"})
	if !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("EnsureActive with an unknown PlanID: err = %v, want %s", err, ErrPlanNotFound.Code)
	}
}

// TestSubscriptionService_EnsureActive_ExistingActive_ReturnedAsIs pins
// the read half: an existing Active subscription on ANY Plan is returned
// untouched -- in.PlanID is a creation-time parameter, so an ensure naming
// a different Plan neither replaces the row nor fails, and an unknown Plan
// id is not even validated while an Active row exists -- and nothing new
// is created.
func TestSubscriptionService_EnsureActive_ExistingActive_ReturnedAsIs(t *testing.T) {
	svc, repo := newSubscriptionServiceWithPlans(t, "plan-1", "plan-2")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	first, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("EnsureActive (plan-1): %v", err)
	}

	again, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-2"})
	if err != nil {
		t.Fatalf("EnsureActive (plan-2): %v", err)
	}
	if again.ID != first.ID || again.PlanID != "plan-1" {
		t.Errorf("EnsureActive on a tenant holding an Active subscription = %s on %s, want the existing %s on plan-1 untouched",
			again.ID, again.PlanID, first.ID)
	}

	again, err = svc.EnsureActive(ctx, CreateInput{PlanID: "does-not-exist"})
	if err != nil {
		t.Fatalf("EnsureActive (unknown Plan, existing Active): %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("EnsureActive with an unknown Plan returned %s, want the existing %s", again.ID, first.ID)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("subscription rows after repeated ensures = %d, want 1", len(rows))
	}
}

// TestSubscriptionService_EnsureActive_NonActiveRows_NeverRevived pins the
// third leg: a non-active row is never adopted or revived -- a canceled
// (terminal) subscription stays canceled and a past_due one stays past_due,
// and each ensure mints and activates a NEW row instead.
func TestSubscriptionService_EnsureActive_NonActiveRows_NeverRevived(t *testing.T) {
	svc, repo := newSubscriptionServiceWithPlans(t, "plan-1")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	canceled, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}
	if _, err = svc.Cancel(ctx, canceled.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	afterCancel, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("EnsureActive after Cancel: %v", err)
	}
	if afterCancel.ID == canceled.ID {
		t.Errorf("EnsureActive after Cancel returned the canceled row %q, want a new subscription", canceled.ID)
	}
	if afterCancel.Status != string(SubscriptionStatusActive) {
		t.Errorf("new subscription Status = %q, want %q", afterCancel.Status, SubscriptionStatusActive)
	}
	old, err := svc.Get(ctx, canceled.ID)
	if err != nil {
		t.Fatalf("Get (canceled): %v", err)
	}
	if old.Status != string(SubscriptionStatusCanceled) {
		t.Errorf("the canceled row's Status = %q, want it left %q", old.Status, SubscriptionStatusCanceled)
	}

	if _, err = svc.MarkPastDue(ctx, afterCancel.ID); err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	afterPastDue, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("EnsureActive after MarkPastDue: %v", err)
	}
	if afterPastDue.ID == afterCancel.ID {
		t.Errorf("EnsureActive after MarkPastDue returned the past_due row %q, want a new subscription", afterCancel.ID)
	}
	old, err = svc.Get(ctx, afterCancel.ID)
	if err != nil {
		t.Fatalf("Get (past_due): %v", err)
	}
	if old.Status != string(SubscriptionStatusPastDue) {
		t.Errorf("the past_due row's Status = %q, want it left %q", old.Status, SubscriptionStatusPastDue)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("subscription rows = %d, want 3 (one canceled, one past_due, one Active)", len(rows))
	}
}

// TestSubscriptionService_SecondActiveForOneTenant_RefusedByTheDatabaseIndex
// pins the single-active invariant and the index that arbitrates it: a
// second activation for a tenant that already holds an Active row is
// refused with a duplicate-key error (matched with errors.Is against
// gorm.ErrDuplicatedKey, the translation dialect-agnostic through
// dbkit.Open's TranslateError), the refused row stays Created, and the
// partial unique index uq_billing_subscriptions_one_active is present in
// the migrated schema with its WHERE status = 'active' predicate. This is
// the semantic a direct Activate caller sees in place of a second Active
// row.
func TestSubscriptionService_SecondActiveForOneTenant_RefusedByTheDatabaseIndex(t *testing.T) {
	svc, repo := newSubscriptionServiceWithPlans(t, "plan-1")
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	first, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	if _, err = svc.Activate(ctx, first.ID); err != nil {
		t.Fatalf("Activate (first): %v", err)
	}

	second, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatalf("Create (second): %v", err)
	}
	_, err = svc.Activate(ctx, second.ID)
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("Activate (second, tenant already Active): err = %v, want a gorm.ErrDuplicatedKey match", err)
	}
	got, err := svc.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("Get (second): %v", err)
	}
	if got.Status != string(SubscriptionStatusCreated) {
		t.Errorf("the refused subscription's Status = %q, want it left %q", got.Status, SubscriptionStatusCreated)
	}

	// The refusal comes from the declared index, not from incidental
	// behavior: the migrated schema carries it, partial on Active rows.
	var indexSQL []string
	if err := repo.db.Raw(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'billing_subscriptions' AND name = 'uq_billing_subscriptions_one_active'`,
	).Scan(&indexSQL).Error; err != nil {
		t.Fatalf("query sqlite_master for the single-active index: %v", err)
	}
	if len(indexSQL) != 1 {
		t.Fatalf("uq_billing_subscriptions_one_active index rows = %d, want 1", len(indexSQL))
	}
	if !strings.Contains(indexSQL[0], "WHERE status = 'active'") {
		t.Errorf("the index is not partial on Active rows: %s", indexSQL[0])
	}
}

// TestSubscriptionService_ConcurrentEnsureActive_ConvergesToOneActive pins
// the race convergence EnsureActive's doc comment promises: two concurrent
// ensures of a tenant's first subscription may each create a
// created-status row, but the single-active index admits exactly one
// activation; the losing ensure absorbs its duplicate-key refusal by
// re-reading and returns the winner's row, so both callers see the same
// Active subscription and neither reports an error.
func TestSubscriptionService_ConcurrentEnsureActive_ConvergesToOneActive(t *testing.T) {
	const iterations = 25
	for i := 0; i < iterations; i++ {
		svc, repo := newSubscriptionServiceWithPlans(t, "plan-1")
		ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

		type result struct {
			sub *Subscription
			err error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		ensure := func() {
			<-start
			sub, err := svc.EnsureActive(ctx, CreateInput{PlanID: "plan-1"})
			results <- result{sub: sub, err: err}
		}
		go ensure()
		go ensure()
		close(start)
		r1, r2 := <-results, <-results

		if r1.err != nil || r2.err != nil {
			t.Fatalf("iteration %d: EnsureActive errors: %v, %v -- a losing ensure must converge, not surface the race", i, r1.err, r2.err)
		}
		if r1.sub.ID != r2.sub.ID {
			t.Fatalf("iteration %d: the ensures converged on different subscriptions %q and %q, want one winner", i, r1.sub.ID, r2.sub.ID)
		}

		active, err := svc.Active(ctx)
		if err != nil {
			t.Fatalf("iteration %d: Active: %v", i, err)
		}
		if active == nil || active.ID != r1.sub.ID {
			t.Fatalf("iteration %d: Active = %v, want the ensured subscription %q", i, active, r1.sub.ID)
		}

		rows, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("iteration %d: List: %v", i, err)
		}
		activeRows := 0
		for j := range rows {
			if rows[j].Status == string(SubscriptionStatusActive) {
				activeRows++
			}
		}
		if activeRows != 1 {
			t.Fatalf("iteration %d: %d Active rows after concurrent ensures, want exactly 1", i, activeRows)
		}
	}
}

func TestSubscriptionService_Get_NotFound(t *testing.T) {
	svc := newSubscriptionService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := svc.Get(ctx, "does-not-exist")
	if !apperr.HasCode(err, ErrSubscriptionNotFound.Code) {
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
	if !apperr.HasCode(err, ErrPlanNotFound.Code) {
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
	if !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Create with an unknown PlanID: err = %v, want %s", err, ErrPlanNotFound.Code)
	}
}

// TestSubscriptionService_ConcurrentCancelAndMarkPastDue_CanceledIsTerminal
// pins the compare-and-swap terminality: Cancel and MarkPastDue are legal
// moves from Active, so two racing calls can both read Active before
// either write commits. An unguarded full-row update would let whichever
// write lands LAST win outright -- when Cancel's write lands first and
// MarkPastDue's second, a successfully canceled (terminal) subscription is
// silently overwritten back to PastDue, and Cancel returns success for a
// subscription that ends up non-terminal. The status change is therefore
// database-arbitrated: each attempt is a guarded UPDATE whose WHERE
// carries the status the move was validated from, with RowsAffected as the
// arbiter, so a transition whose guard misses (the row already moved)
// re-reads and re-attempts while the move stays legal -- and nothing can
// ever overwrite a Canceled row, since no legal move exists out of
// Canceled and no stale guard can match it. The assertion pins exactly
// that invariant: a Cancel that reported success must leave the
// subscription Canceled, no matter how the two calls interleave.
func TestSubscriptionService_ConcurrentCancelAndMarkPastDue_CanceledIsTerminal(t *testing.T) {
	const iterations = 150
	for i := 0; i < iterations; i++ {
		svc := newSubscriptionService(t)
		ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

		sub, err := svc.Create(ctx, CreateInput{PlanID: "plan-1"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, actErr := svc.Activate(ctx, sub.ID); actErr != nil {
			t.Fatalf("Activate: %v", actErr)
		}

		// Launch both transitions from a common start line so both reads
		// race the same window, the interleaving the guard must survive.
		start := make(chan struct{})
		cancelErr := make(chan error, 1)
		pastDueErr := make(chan error, 1)
		go func() {
			<-start
			_, cerr := svc.Cancel(ctx, sub.ID)
			cancelErr <- cerr
		}()
		go func() {
			<-start
			_, merr := svc.MarkPastDue(ctx, sub.ID)
			pastDueErr <- merr
		}()
		close(start)

		cerr := <-cancelErr
		<-pastDueErr

		got, err := svc.Get(ctx, sub.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if cerr == nil && got.Status != string(SubscriptionStatusCanceled) {
			t.Fatalf("iteration %d: Cancel returned success but the subscription ended %q -- a canceled (terminal) subscription was left non-terminal", i, got.Status)
		}
	}
}

func TestSubscriptionRepository_AssertIsolated(t *testing.T) {
	repo := NewSubscriptionRepository(newTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Subscription {
		return &Subscription{ID: uuid.NewString(), PlanID: "plan-1", Status: string(SubscriptionStatusCreated)}
	})
}
