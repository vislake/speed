package billing

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
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

// TestPlanStore_Update_EmptyID_RefusedNotSilentlyInserted is P2-16's
// regression: an Update whose plan.ID is empty must answer the coded
// not-found and change nothing. No stored Plan can carry an empty ID (Create
// generates a UUID whenever plan.ID is blank), but GORM's Save performs a
// CREATE whenever the primary key is blank -- the Where clause
// notwithstanding -- so on pre-fix code this call silently INSERTED a new
// row (with id "") and returned nil, and a subsequent Get("") found the
// ghost row the caller never meant to create.
func TestPlanStore_Update_EmptyID_RefusedNotSilentlyInserted(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()

	ghost := &Plan{Key: "ghost", Name: "Ghost"}
	if err := ghost.SetGrants([]Grant{{FeatureKey: "seats", Value: int64(5)}}); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}

	err := store.Update(ctx, ghost) // ghost.ID is empty: names no row
	if !hasCode(err, ErrPlanNotFound.Code) {
		t.Fatalf("Update with empty ID: err = %v, want %s (never a silent insert)", err, ErrPlanNotFound.Code)
	}
	if _, err := store.Get(ctx, ""); !hasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Get(\"\") after the empty-ID Update: err = %v, want %s (the update must not have created a row with id \"\")", err, ErrPlanNotFound.Code)
	}
}

// TestErrPlanNotFound_Message_RendersTheLookedUpValue is P2-20's regression:
// billing.plan_not_found's locale template must interpolate the value every
// lookup site actually decorates the error with. All four sites carry the
// looked-up value under the same "id" parameter name (PlanStore.Get/Update
// and SubscriptionService.Create by plan id, PlanStore.Resolve by plan key)
// and the template interpolates {{.id}} -- on pre-fix code three sites
// passed "id" while the template read {{.key}}, so the common lookup path
// (Get, and with it Entitlements.Check's deleted-plan handling) rendered
// the value as an empty slot ("<no value>") instead of the id.
func TestErrPlanNotFound_Message_RendersTheLookedUpValue(t *testing.T) {
	db := newTestDB(t)
	plans := NewPlanStore(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	build := i18n.NewBuilder()
	if err := build.AddModule("billing", NewModule(nil, stubUsage{}).Locales()); err != nil {
		t.Fatalf("AddModule: %v", err)
	}
	catalog := build.Build()

	// Exercise the four real call sites and render each one's error the way
	// the API envelope does: the code's own locale template over the
	// error's own structured params.
	sites := []struct {
		name  string
		err   error
		value string
	}{
		{
			name:  "PlanStore.Get by unknown id",
			err:   func() error { _, err := plans.Get(ctx, "plan-abc-123"); return err }(),
			value: "plan-abc-123",
		},
		{
			name: "PlanStore.Update by unknown id",
			err: func() error {
				return plans.Update(ctx, &Plan{ID: "plan-abc-123", Key: "pro", Name: "Pro"})
			}(),
			value: "plan-abc-123",
		},
		{
			name: "PlanStore.Resolve by unknown key",
			err: func() error {
				_, err := plans.Resolve(ctx, pkgcore.TenantID("tenant-a"), "no-such-key")
				return err
			}(),
			value: "no-such-key",
		},
		{
			name: "SubscriptionService.Create by unknown plan id",
			err: func() error {
				_, err := NewSubscriptionService(NewSubscriptionRepository(db), plans, nil).
					Create(ctx, CreateInput{PlanID: "plan-abc-123"})
				return err
			}(),
			value: "plan-abc-123",
		},
	}
	for _, tc := range sites {
		t.Run(tc.name, func(t *testing.T) {
			appErr, ok := apperr.As(tc.err)
			if !ok {
				t.Fatalf("err = %v, want an *apperr.Error", tc.err)
			}
			if !hasCode(tc.err, ErrPlanNotFound.Code) {
				t.Fatalf("err = %v, want %s", tc.err, ErrPlanNotFound.Code)
			}
			rendered, err := catalog.Lookup("en-US", appErr.Code, appErr.Params)
			if err != nil {
				t.Fatalf("Lookup(en-US, %q): %v", appErr.Code, err)
			}
			if !strings.Contains(rendered, tc.value) {
				t.Errorf("rendered = %q, want it to contain the looked-up value %q (a param-name mismatch renders an empty slot instead)", rendered, tc.value)
			}
			if strings.Contains(rendered, "<no value>") {
				t.Errorf("rendered = %q, want no empty slot", rendered)
			}
		})
	}

	// The zh-CN template must interpolate the same parameter name.
	appErr, ok := apperr.As(ErrPlanNotFound.WithParam("id", "plan-abc-123"))
	if !ok {
		t.Fatal("WithParam result is not an *apperr.Error")
	}
	rendered, err := catalog.Lookup("zh-CN", appErr.Code, appErr.Params)
	if err != nil {
		t.Fatalf("Lookup(zh-CN, %q): %v", appErr.Code, err)
	}
	if !strings.Contains(rendered, "plan-abc-123") {
		t.Errorf("zh-CN rendered = %q, want it to contain the looked-up value", rendered)
	}
}
