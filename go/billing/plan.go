package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// billingPlansTable names the shared billing_plans table.
const billingPlansTable = "billing_plans"

// platformScopeSentinel is the TenantID value a platform-wide Plan carries.
// Empty, never NULL, for the same reason go/config's row and
// go/dbkit/audit's AuditEvent both already document: NULLs are distinct in
// a PostgreSQL unique index, so two platform rows for one Key could coexist
// under NULL where the empty-string sentinel collapses them into the one
// row the (tenant_id, key) unique index promises.
const platformScopeSentinel = ""

// Plan is one subscribable tier: what it costs, how often, and which
// Feature grants it awards.
//
// # Tenant-custom Plans, and why TenantID is a plain string, not a *string
//
// docs/internal/06-billing-and-metering.md's own sketch spells this field
// as "TenantID *string // nil = platform-wide; set = tenant-custom" --
// illustrative Go, not this module's literal storage shape. Plan is a dual-
// domain table (docs/internal/04-data-and-tenancy.md): a platform-wide row
// is genuinely platform data, visible to any tenant's lookup, while a
// tenant-custom row is genuinely that one tenant's private data -- and no
// single dbkit data-domain capability spans both faces of the same table
// (dbkit.TenantScoped's own isolation plugin would hide every platform-wide
// row from a tenant-scoped query, exactly the opposite of what "falls back
// to platform-wide when no tenant-custom Plan exists" requires). This is
// the identical duality go/config's own scope-tiered "configs" table
// already solves, and Plan adopts its exact answer: TenantID is a plain,
// NOT NULL string column holding platformScopeSentinel ("") for a
// platform-wide row, reached through a plain *gorm.DB (PlanStore below,
// never dbkit.Repository[T] -- Repository[T]'s generic constraint requires
// TenantScoped, which this table must NOT implement), isolation proven with
// tenancytest.AssertNotTenantScoped rather than AssertIsolated -- see
// AGENTS.md's "Design choice: Plan's tenant-scoping duality" section for
// the full write-up, including why this is not a gap against the "every
// tenant table must run AssertIsolated" rule.
//
// # Lookup precedence
//
// Key is the stable identifier PlanStore.Resolve looks up by, independent
// of scope: a tenant-custom Plan for (tenantID, key) overrides the
// platform-wide Plan for key when both exist, so "give this one customer a
// custom price on the same product tier" needs no coordination with the
// platform catalog. See PlanStore.Resolve's own doc comment.
type Plan struct {
	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one -- the backend coding standard forbids
	// gen_random_uuid().
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantID is platformScopeSentinel ("") for a platform-wide Plan, or
	// the owning tenant's id for a tenant-custom one. See the type's own
	// doc comment for why this is a plain string, not tenant-scoped.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// Key is the stable, scope-independent identifier PlanStore.Resolve
	// looks up by (e.g. "pro", "enterprise"). Unique per (TenantID, Key).
	Key string `gorm:"column:plan_key;size:100;not null"`

	// Name is the plan's display name, shown to end users -- distinct
	// from Key, which is never rendered.
	Name string `gorm:"column:name;size:200;not null"`

	// PriceCents and Currency are Money's flattened storage columns; use
	// Price/SetPrice to convert.
	PriceCents int64  `gorm:"column:price_cents;not null"`
	Currency   string `gorm:"column:currency;size:3;not null"`

	// Interval is a BillingInterval value: "month", "year" or "one_time".
	Interval string `gorm:"column:billing_interval;size:16;not null"`

	// GrantsJSON is the JSON-encoded []Grant this plan awards; use
	// Grants/SetGrants to convert. Stored as a plain, portable TEXT
	// column (gorm.io/datatypes.JSON is used for its Value/Scan
	// convenience only) -- this package never filters on its content
	// with a native JSONB operator, which the backend coding standard
	// forbids.
	GrantsJSON datatypes.JSON `gorm:"column:grants;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// TableName pins Plan to the billing_plans table.
func (Plan) TableName() string { return billingPlansTable }

// IsPlatformWide reports whether p is the platform-wide default for its
// Key, as opposed to a tenant-custom override.
func (p Plan) IsPlatformWide() bool { return p.TenantID == platformScopeSentinel }

// Price decodes PriceCents/Currency into a Money value.
func (p Plan) Price() Money { return Money{Cents: p.PriceCents, Currency: p.Currency} }

// SetPrice encodes m into PriceCents/Currency.
func (p *Plan) SetPrice(m Money) {
	p.PriceCents = m.Cents
	p.Currency = m.Currency
}

// Grants decodes GrantsJSON into a []Grant. A decode failure (which no
// write path in this package can produce, since SetGrants is the only
// writer) returns nil rather than an error -- there is nothing a caller
// could do to repair a corrupted column value at read time.
func (p Plan) Grants() []Grant {
	if len(p.GrantsJSON) == 0 {
		return nil
	}
	var out []Grant
	if err := json.Unmarshal(p.GrantsJSON, &out); err != nil {
		return nil
	}
	return out
}

// SetGrants encodes grants into GrantsJSON.
func (p *Plan) SetGrants(grants []Grant) error {
	b, err := json.Marshal(grants)
	if err != nil {
		return fmt.Errorf("billing: encode plan grants: %w", err)
	}
	p.GrantsJSON = datatypes.JSON(b)
	return nil
}

// Grant returns the Grant for featureKey, if p awards one.
func (p Plan) Grant(featureKey string) (Grant, bool) {
	for _, g := range p.Grants() {
		if g.FeatureKey == featureKey {
			return g, true
		}
	}
	return Grant{}, false
}

// PlanStore is the billing_plans table accessor. It is deliberately a thin
// row accessor over a plain *gorm.DB, never dbkit.Repository[T] -- see
// Plan's own doc comment for why the table's dual-domain shape rules that
// out. It needs no .Table/.Model/.Raw escape hatch: Create/Where/First/Find
// all suffice, so it is unaffected by the raw-gorm-bypass semgrep rule.
type PlanStore struct {
	db *gorm.DB
}

// NewPlanStore returns a PlanStore over db. db is expected to come from
// dbkit.Open with this module's migrations applied.
func NewPlanStore(db *gorm.DB) *PlanStore {
	return &PlanStore{db: db}
}

// Create inserts plan. plan.ID is generated when left empty. plan.TenantID
// is left exactly as given -- platformScopeSentinel for a platform-wide
// plan, an explicit tenant id for a tenant-custom one -- since, unlike a
// genuinely tenant-scoped table, PlanStore has no ambient request tenant to
// force it to. A duplicate (TenantID, Key) pair fails with
// ErrDuplicatePlanKey, translated from the migration's own unique index.
func (s *PlanStore) Create(ctx context.Context, plan *Plan) error {
	if plan.Key == "" {
		return ErrPlanKeyRequired
	}
	if plan.ID == "" {
		plan.ID = uuid.NewString()
	}
	if len(plan.GrantsJSON) == 0 {
		// The grants column is NOT NULL: a caller that never called
		// SetGrants (a Plan with no grants at all, at least not yet) must
		// still write a valid empty JSON array rather than leaving GORM to
		// pass a nil []byte through as SQL NULL.
		plan.GrantsJSON = datatypes.JSON("[]")
	}
	err := s.db.WithContext(ctx).Create(plan).Error
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicatePlanKey.WithParam("tenant_id", plan.TenantID).WithParam("key", plan.Key)
	}
	return fmt.Errorf("billing: create plan: %w", err)
}

// Update saves every field of plan, matched by ID. It returns
// ErrPlanNotFound if no row with that ID exists.
func (s *PlanStore) Update(ctx context.Context, plan *Plan) error {
	if len(plan.GrantsJSON) == 0 {
		// See Create's identical guard: the grants column is NOT NULL.
		plan.GrantsJSON = datatypes.JSON("[]")
	}
	res := s.db.WithContext(ctx).Where("id = ?", plan.ID).Select("*").Save(plan)
	if res.Error != nil {
		return fmt.Errorf("billing: update plan %q: %w", plan.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrPlanNotFound.WithParam("id", plan.ID)
	}
	return nil
}

// Get returns the Plan with the given id, or ErrPlanNotFound.
func (s *PlanStore) Get(ctx context.Context, id string) (*Plan, error) {
	var out Plan
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrPlanNotFound.WithParam("id", id)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: get plan %q: %w", id, err)
	}
	return &out, nil
}

// Resolve is the tenant-custom Plan lookup precedence:
// docs/internal/06-billing-and-metering.md's rationale that negotiating a
// custom deal with one enterprise customer is routine in on-premise
// delivery, realized as one rule -- a tenant-custom Plan for (tenantID,
// key) is used when one exists; otherwise the platform-wide Plan for key is
// used; otherwise ErrPlanNotFound. tenantID may be platformScopeSentinel
// (""), in which case only the platform-wide lookup runs (there is no
// narrower scope to prefer).
//
// Resolve takes tenantID as an explicit parameter rather than reading it
// off ctx: PlanStore is not itself tenant-scoped machinery (see the type's
// own doc comment), and a caller resolving on behalf of a specific
// subscription -- not necessarily "the current request's tenant" -- needs
// to name the tenant explicitly. EntitlementsService and
// SubscriptionService both pass pkgcore.TenantFromContext(ctx)'s own answer
// through unchanged for their own calls.
func (s *PlanStore) Resolve(ctx context.Context, tenantID pkgcore.TenantID, key string) (*Plan, error) {
	if tenantID != "" && string(tenantID) != platformScopeSentinel {
		custom, err := s.findByScopeAndKey(ctx, string(tenantID), key)
		if err != nil {
			return nil, err
		}
		if custom != nil {
			return custom, nil
		}
	}
	platform, err := s.findByScopeAndKey(ctx, platformScopeSentinel, key)
	if err != nil {
		return nil, err
	}
	if platform == nil {
		return nil, ErrPlanNotFound.WithParam("key", key)
	}
	return platform, nil
}

// findByScopeAndKey returns the Plan for the exact (tenantID, key) pair, or
// (nil, nil) when none exists.
func (s *PlanStore) findByScopeAndKey(ctx context.Context, tenantID, key string) (*Plan, error) {
	var out Plan
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND plan_key = ?", tenantID, key).
		First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: resolve plan (tenant=%q, key=%q): %w", tenantID, key, err)
	}
	return &out, nil
}

// PlanService is the write-side wrapper over PlanStore that publishes
// EventPlanChanged after every mutation, per
// docs/internal/06-billing-and-metering.md's rule that entitlement changes
// take effect immediately, broadcast over the event bus. PlanStore itself
// stays a plain, event-free row accessor
// (used directly by EntitlementsService's read path, which needs no
// event); PlanService is the seam anything that WRITES a Plan should go
// through instead.
type PlanService struct {
	store  *PlanStore
	events pkgcore.EventBus
}

// NewPlanService returns a PlanService over store, publishing EventPlanChanged
// on bus. bus may be nil (no publish -- see SubscriptionService's identical
// convention).
func NewPlanService(store *PlanStore, bus pkgcore.EventBus) *PlanService {
	return &PlanService{store: store, events: bus}
}

// Create inserts plan and publishes EventPlanChanged with Action "created".
func (s *PlanService) Create(ctx context.Context, plan *Plan) error {
	if err := s.store.Create(ctx, plan); err != nil {
		return err
	}
	s.publish(ctx, plan, "created")
	return nil
}

// Update saves plan and publishes EventPlanChanged with Action "updated".
func (s *PlanService) Update(ctx context.Context, plan *Plan) error {
	if err := s.store.Update(ctx, plan); err != nil {
		return err
	}
	s.publish(ctx, plan, "updated")
	return nil
}

// Get returns the Plan with the given id, or ErrPlanNotFound.
func (s *PlanService) Get(ctx context.Context, id string) (*Plan, error) {
	return s.store.Get(ctx, id)
}

// Resolve is PlanStore.Resolve's pass-through -- see that method's own doc
// comment for the tenant-custom-override lookup precedence.
func (s *PlanService) Resolve(ctx context.Context, tenantID pkgcore.TenantID, key string) (*Plan, error) {
	return s.store.Resolve(ctx, tenantID, key)
}

func (s *PlanService) publish(ctx context.Context, plan *Plan, action string) {
	if s.events == nil {
		return
	}
	tenantID := pkgcore.TenantID(plan.TenantID)
	_ = s.events.Publish(ctx, pkgcore.Event{
		Type:     EventPlanChanged,
		TenantID: tenantID,
		Payload: PlanChangedEvent{
			PlanID:   plan.ID,
			TenantID: plan.TenantID,
			Key:      plan.Key,
			Action:   action,
		},
	})
}
