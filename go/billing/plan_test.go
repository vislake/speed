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

func TestPlanStore_CreateAndGetPlatformPlan(t *testing.T) {
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
	if !plan.IsPlatformWide() {
		t.Fatal("Create left the no-tenant Plan not platform-wide")
	}

	// A no-tenant Plan is a platform-wide row -- the reader that names the
	// scope it lives in is GetPlatformPlan.
	got, err := store.GetPlatformPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("GetPlatformPlan: %v", err)
	}
	if got.Key != "pro" || got.Name != "Pro" {
		t.Errorf("GetPlatformPlan returned %+v, want Key=pro Name=Pro", got)
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
	if !apperr.HasCode(err, ErrPlanKeyRequired.Code) {
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
	if !apperr.HasCode(err, ErrDuplicatePlanKey.Code) {
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

// TestPlanStore_Resolve_TenantCustomOverridesPlatformWide pins the
// tenant-custom Plan lookup precedence: a tenant-custom Plan for
// (tenantID, key) is used when one exists; the platform-wide Plan for key
// is used otherwise; ErrPlanNotFound when neither exists.
func TestPlanStore_Resolve_TenantCustomOverridesPlatformWide(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const tenant = pkgcore.TenantID("tenant-acme")
	const otherTenant = pkgcore.TenantID("tenant-globex")

	t.Run("neither exists", func(t *testing.T) {
		_, err := store.Resolve(ctx, tenant, "pro")
		if !apperr.HasCode(err, ErrPlanNotFound.Code) {
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

// TestPlanStore_Update_EmptyID_RefusedNotSilentlyInserted pins the
// empty-ID refusal: an Update whose plan.ID is empty must answer the coded
// not-found and change nothing. No stored Plan can carry an empty ID (Create
// generates a UUID whenever plan.ID is blank), but GORM's Save performs a
// CREATE whenever the primary key is blank -- the Where clause
// notwithstanding -- so without the guard the empty-ID call would silently
// insert a new row (with id "") and return nil, and a subsequent Get would
// find the ghost row the caller never meant to create.
func TestPlanStore_Update_EmptyID_RefusedNotSilentlyInserted(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const scope = pkgcore.TenantID("tenant-a")

	ghost := &Plan{TenantID: string(scope), Key: "ghost", Name: "Ghost"}
	if err := ghost.SetGrants([]Grant{{FeatureKey: "seats", Value: int64(5)}}); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}

	err := store.Update(ctx, scope, ghost) // ghost.ID is empty: names no row
	if !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Fatalf("Update with empty ID: err = %v, want %s (never a silent insert)", err, ErrPlanNotFound.Code)
	}
	if _, err := store.Get(ctx, scope, ""); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Get(scope, \"\") after the empty-ID Update: err = %v, want %s (the update must not have created a row with id \"\")", err, ErrPlanNotFound.Code)
	}
}

// TestPlanStore_ScopedGet_OwnRowsOnly pins the scope guard on the read
// side: a Get naming tenant-a's scope must return only tenant-a's own
// custom Plan, never tenant-b's -- an unscoped read would cross tenants
// freely. A platform-wide row is not in any tenant's own scope either: it
// is read through GetPlatformPlan, the named platform-level read.
func TestPlanStore_ScopedGet_OwnRowsOnly(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const a = pkgcore.TenantID("tenant-a")
	const b = pkgcore.TenantID("tenant-b")

	platform := &Plan{Key: "pro", Name: "Platform Pro"}
	if err := store.Create(ctx, platform); err != nil {
		t.Fatalf("Create platform plan: %v", err)
	}
	aPlan := &Plan{TenantID: string(a), Key: "pro", Name: "A's custom pro"}
	if err := store.Create(ctx, aPlan); err != nil {
		t.Fatalf("Create tenant-a plan: %v", err)
	}
	bPlan := &Plan{TenantID: string(b), Key: "pro", Name: "B's custom pro"}
	if err := store.Create(ctx, bPlan); err != nil {
		t.Fatalf("Create tenant-b plan: %v", err)
	}

	// A's scope reads A's own row...
	got, err := store.Get(ctx, a, aPlan.ID)
	if err != nil {
		t.Fatalf("Get(tenant-a, own plan): %v", err)
	}
	if got.ID != aPlan.ID {
		t.Errorf("Get(tenant-a) = plan %q, want tenant-a's own %q", got.ID, aPlan.ID)
	}
	// ...and never B's, never the platform's.
	if _, err = store.Get(ctx, a, bPlan.ID); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Get(tenant-a, tenant-b's plan): err = %v, want %s -- the tenant scope must not read another tenant's custom plan (pre-fix: the unscoped Get returned it)", err, ErrPlanNotFound.Code)
	}
	if _, err = store.Get(ctx, a, platform.ID); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Get(tenant-a, platform plan): err = %v, want %s -- a platform-wide row is not in any tenant's own scope", err, ErrPlanNotFound.Code)
	}

	// The platform-level read reaches the platform row by name, and only
	// the platform row.
	gotPlatform, err := store.GetPlatformPlan(ctx, platform.ID)
	if err != nil {
		t.Fatalf("GetPlatformPlan(platform id): %v", err)
	}
	if gotPlatform.ID != platform.ID {
		t.Errorf("GetPlatformPlan = plan %q, want the platform plan %q", gotPlatform.ID, platform.ID)
	}
	if _, err = store.GetPlatformPlan(ctx, aPlan.ID); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("GetPlatformPlan(tenant-a's custom id): err = %v, want %s -- the platform read must refuse a tenant-custom row", err, ErrPlanNotFound.Code)
	}
	if _, err = store.GetPlatformPlan(ctx, bPlan.ID); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("GetPlatformPlan(tenant-b's custom id): err = %v, want %s", err, ErrPlanNotFound.Code)
	}
}

// TestPlanStore_ScopedUpdate_OwnRowsOnly pins the scope guard on the
// write side: an Update naming tenant-a's scope must not modify tenant-b's
// custom Plan -- an unscoped update would write tenant-b's row. The named
// scope must own the row both as stored and as the plan struct carries it,
// so an update can neither touch another tenant's row nor move one of the
// caller's own rows into another scope.
func TestPlanStore_ScopedUpdate_OwnRowsOnly(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const a = pkgcore.TenantID("tenant-a")
	const b = pkgcore.TenantID("tenant-b")

	aPlan := &Plan{TenantID: string(a), Key: "pro", Name: "A's custom pro"}
	if err := store.Create(ctx, aPlan); err != nil {
		t.Fatalf("Create tenant-a plan: %v", err)
	}
	bPlan := &Plan{TenantID: string(b), Key: "pro", Name: "B's custom pro"}
	if err := store.Create(ctx, bPlan); err != nil {
		t.Fatalf("Create tenant-b plan: %v", err)
	}

	// A cross-scope update of B's row: refused, row untouched.
	mutB := *bPlan
	mutB.SetPrice(Money{Cents: 9999, Currency: "USD"})
	if err := store.Update(ctx, a, &mutB); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Update(tenant-a, tenant-b's plan): err = %v, want %s (pre-fix: the unscoped Update wrote it)", err, ErrPlanNotFound.Code)
	}
	got, err := store.Get(ctx, b, bPlan.ID)
	if err != nil {
		t.Fatalf("Get(tenant-b, own plan): %v", err)
	}
	if got.Price() != bPlan.Price() {
		t.Errorf("tenant-b's plan price after tenant-a's refused Update = %+v, want unchanged %+v", got.Price(), bPlan.Price())
	}

	// A scope-drifting update of A's OWN row (the struct's TenantID moved
	// to B's scope): refused, row stays A's.
	mutDrift := *aPlan
	mutDrift.TenantID = string(b)
	mutDrift.SetPrice(Money{Cents: 1111, Currency: "USD"})
	if err = store.Update(ctx, a, &mutDrift); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Update(tenant-a, own row drifted to tenant-b's scope): err = %v, want %s -- an update must not move a row across scopes", err, ErrPlanNotFound.Code)
	}
	got, err = store.Get(ctx, a, aPlan.ID)
	if err != nil {
		t.Fatalf("Get(tenant-a, own plan): %v", err)
	}
	if got.TenantID != string(a) {
		t.Errorf("tenant-a's plan TenantID after the refused drift = %q, want %q", got.TenantID, string(a))
	}

	// A within-scope update of A's own row: succeeds.
	mutA := *aPlan
	mutA.SetPrice(Money{Cents: 5555, Currency: "USD"})
	if err = store.Update(ctx, a, &mutA); err != nil {
		t.Fatalf("Update(tenant-a, own plan): %v", err)
	}
	got, err = store.Get(ctx, a, aPlan.ID)
	if err != nil {
		t.Fatalf("Get(tenant-a, own plan): %v", err)
	}
	if got.Price() != mutA.Price() {
		t.Errorf("tenant-a's plan price after its own Update = %+v, want %+v", got.Price(), mutA.Price())
	}
}

// TestPlanStore_ScopedUpdate_PlatformScope covers the platform face of
// the same rule: an Update naming the platform scope writes platform-wide
// rows and refuses tenant-custom ones, mirroring how a tenant scope
// refuses everything outside itself.
func TestPlanStore_ScopedUpdate_PlatformScope(t *testing.T) {
	store := NewPlanStore(newTestDB(t))
	ctx := context.Background()
	const platform = pkgcore.TenantID(platformScopeSentinel)
	const b = pkgcore.TenantID("tenant-b")

	platformPlan := &Plan{Key: "pro", Name: "Platform Pro"}
	if err := store.Create(ctx, platformPlan); err != nil {
		t.Fatalf("Create platform plan: %v", err)
	}
	bPlan := &Plan{TenantID: string(b), Key: "pro", Name: "B's custom pro"}
	if err := store.Create(ctx, bPlan); err != nil {
		t.Fatalf("Create tenant-b plan: %v", err)
	}

	mutPlatform := *platformPlan
	mutPlatform.SetPrice(Money{Cents: 7777, Currency: "USD"})
	if err := store.Update(ctx, platform, &mutPlatform); err != nil {
		t.Fatalf("Update(platform scope, platform plan): %v", err)
	}
	got, err := store.GetPlatformPlan(ctx, platformPlan.ID)
	if err != nil {
		t.Fatalf("GetPlatformPlan: %v", err)
	}
	if got.Price() != mutPlatform.Price() {
		t.Errorf("platform plan price after the platform-scope Update = %+v, want %+v", got.Price(), mutPlatform.Price())
	}

	mutB := *bPlan
	mutB.SetPrice(Money{Cents: 9999, Currency: "USD"})
	if err := store.Update(ctx, platform, &mutB); !apperr.HasCode(err, ErrPlanNotFound.Code) {
		t.Errorf("Update(platform scope, tenant-b's plan): err = %v, want %s", err, ErrPlanNotFound.Code)
	}
}

// TestErrPlanNotFound_Message_RendersTheLookedUpValue pins the rendered message:
// billing.plan_not_found's locale template must interpolate the value every
// lookup site actually decorates the error with. All four sites carry the
// looked-up value under the same "id" parameter name (PlanStore.Get/Update
// and SubscriptionService.Create by plan id, PlanStore.Resolve by plan key)
// and the template interpolates {{.id}}: a site passing a differently
// named parameter would render the common lookup path's value as an empty
// slot ("<no value>") instead of the id.
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
			name: "PlanStore.Get by unknown id",
			err: func() error {
				_, err := plans.Get(ctx, pkgcore.TenantID("tenant-a"), "plan-abc-123")
				return err
			}(),
			value: "plan-abc-123",
		},
		{
			name: "PlanStore.Update by unknown id",
			err: func() error {
				return plans.Update(ctx, pkgcore.TenantID("tenant-a"), &Plan{ID: "plan-abc-123", TenantID: "tenant-a", Key: "pro", Name: "Pro"})
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
			if !apperr.HasCode(tc.err, ErrPlanNotFound.Code) {
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
