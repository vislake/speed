package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func TestPlanStore_CreateAndGet(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()

	plan := &Plan{Key: "pro", Name: "Pro"}
	plan.SetPrice(Money{Cents: 4900, Currency: "USD"})
	if err := plan.SetGrants([]Grant{{FeatureKey: "seats", Value: int64(5)}}); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}
	if err := store.Create(ctx, plan); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if plan.ID == "" {
		t.Fatal("Create left plan.ID empty")
	}

	got, err := store.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != "pro" || got.Name != "Pro" {
		t.Errorf("Get returned %+v, want Key=pro Name=Pro", got)
	}
	if g, ok := got.Grant("seats"); !ok || g.Value != float64(5) {
		// Grant.Value round-trips through JSON as float64 -- see
		// grantQuotaLimit's own doc comment for why callers normalize this.
		t.Errorf("Grant(seats) = %v, %v", g, ok)
	}
}

func TestPlanStore_Create_EmptyKey_Refused(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	err := store.Create(context.Background(), &Plan{Name: "No key"})
	if !hasCode(err, ErrPlanKeyRequired.Code) {
		t.Errorf("Create with empty key: err = %v, want %s", err, ErrPlanKeyRequired.Code)
	}
}

func TestPlanStore_Create_DuplicateTenantKey_Refused(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()

	if err := store.Create(ctx, &Plan{Key: "pro", Name: "Pro"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := store.Create(ctx, &Plan{Key: "pro", Name: "Pro again"})
	if !hasCode(err, ErrDuplicatePlanKey.Code) {
		t.Errorf("duplicate Create: err = %v, want %s", err, ErrDuplicatePlanKey.Code)
	}
}

func TestPlanStore_Create_SameKeyDifferentTenants_Allowed(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()

	if err := store.Create(ctx, &Plan{Key: "pro", Name: "Platform pro"}); err != nil {
		t.Fatalf("platform-wide Create: %v", err)
	}
	if err := store.Create(ctx, &Plan{TenantID: "tenant-acme", Key: "pro", Name: "Acme's pro"}); err != nil {
		t.Errorf("tenant-custom Create with the same key: %v, want success -- distinct tenant scopes must not collide", err)
	}
}

// TestPlanStore_Resolve_TenantCustomOverridesPlatformWide is the round's
// mandated proof of the tenant-custom Plan lookup precedence
// (docs/internal/06-billing-and-metering.md): a tenant-custom Plan for
// (tenantID, key) is used when one exists; the platform-wide Plan for key
// is used otherwise; ErrPlanNotFound when neither exists.
func TestPlanStore_Resolve_TenantCustomOverridesPlatformWide(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const tenant = pkgcore.TenantID("tenant-acme")
	const otherTenant = pkgcore.TenantID("tenant-globex")

	t.Run("neither exists", func(t *testing.T) {
		_, err := store.Resolve(ctx, tenant, "pro")
		if !hasCode(err, ErrPlanNotFound.Code) {
			t.Errorf("Resolve with nothing created: err = %v, want %s", err, ErrPlanNotFound.Code)
		}
	})

	platform := &Plan{Key: "pro", Name: "Platform Pro"}
	if err := store.Create(ctx, platform); err != nil {
		t.Fatalf("create platform-wide plan: %v", err)
	}

	t.Run("falls back to platform-wide when no tenant-custom plan exists", func(t *testing.T) {
		got, err := store.Resolve(ctx, tenant, "pro")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.ID != platform.ID {
			t.Errorf("Resolve = plan %q, want the platform-wide plan %q", got.ID, platform.ID)
		}
	})

	custom := &Plan{TenantID: string(tenant), Key: "pro", Name: "Acme's custom Pro"}
	if err := store.Create(ctx, custom); err != nil {
		t.Fatalf("create tenant-custom plan: %v", err)
	}

	t.Run("tenant-custom plan overrides platform-wide once it exists", func(t *testing.T) {
		got, err := store.Resolve(ctx, tenant, "pro")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.ID != custom.ID {
			t.Errorf("Resolve = plan %q, want the tenant-custom plan %q", got.ID, custom.ID)
		}
	})

	t.Run("a different tenant is unaffected and still resolves platform-wide", func(t *testing.T) {
		got, err := store.Resolve(ctx, otherTenant, "pro")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.ID != platform.ID {
			t.Errorf("Resolve for a different tenant = plan %q, want the platform-wide plan %q", got.ID, platform.ID)
		}
	})

	t.Run("the empty-string sentinel tenant resolves platform-wide directly", func(t *testing.T) {
		got, err := store.Resolve(ctx, pkgcore.TenantID(platformScopeSentinel), "pro")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.ID != platform.ID {
			t.Errorf("Resolve = plan %q, want the platform-wide plan %q", got.ID, platform.ID)
		}
	})
}

// TestPlan_AssertNotTenantScoped proves Plan's data-domain design choice
// (see plan.go's own doc comment): it is genuinely visible/writable
// regardless of which tenant, if any, is in the calling context -- the
// scoping this table actually needs is enforced entirely by PlanStore's
// own Resolve method, never by dbkit's isolation plugin.
func TestPlan_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	createFn := func(db *gorm.DB) error {
		p := &Plan{ID: uuid.NewString(), Key: uniqueKey(), Name: "probe", GrantsJSON: []byte("[]")}
		return db.Create(p).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Table(billingPlansTable).Count(&n).Error
		return n, err
	}
	tenancytest.AssertNotTenantScoped(t, db, Plan{}, createFn, findFn)
}

func TestPlanService_Create_PublishesEventPlanChanged(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	received := make(chan pkgcore.Event, 1)
	bus.Subscribe(EventPlanChanged, func(_ context.Context, evt pkgcore.Event) error {
		received <- evt
		return nil
	})

	svc := NewPlanService(NewPlanStore(newTestDB(t)), bus)
	plan := &Plan{Key: "pro", Name: "Pro"}
	if err := svc.Create(context.Background(), plan); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case evt := <-received:
		payload, ok := evt.Payload.(PlanChangedEvent)
		if !ok {
			t.Fatalf("Payload type = %T, want PlanChangedEvent", evt.Payload)
		}
		if payload.PlanID != plan.ID || payload.Action != "created" {
			t.Errorf("payload = %+v, want PlanID=%q Action=created", payload, plan.ID)
		}
	default:
		t.Fatal("EventPlanChanged was not published")
	}
}

// uniqueKey returns a fresh key string, so TestPlan_AssertNotTenantScoped's
// several createFn calls never collide on billing_plans' own
// (tenant_id, key) unique index.
func uniqueKey() string {
	return "probe-key-" + uuid.NewString()
}
