package admin

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/tenancy"
)

// TenantRepository reads and writes admin_tenants.
//
// It holds a plain *gorm.DB rather than embedding dbkit.Repository[T]:
// admin_tenants is platform data describing every tenant, not one tenant's
// own data, so it must NOT implement dbkit.TenantScoped and therefore
// cannot satisfy dbkit.Repository[T]'s generic constraint.
//
// Two rules apply to this file, mirroring every other platform-data
// repository in the codebase: no .Table/.Model/.Raw (the semgrep-checked
// bypass entry points), and no hand-written "WHERE tenant_id = ?" -- there
// is no per-row tenant filter to apply here at all, by design.
type TenantRepository struct {
	db *gorm.DB
}

// NewTenantRepository binds db.
func NewTenantRepository(db *gorm.DB) *TenantRepository {
	return &TenantRepository{db: db}
}

// Create inserts t, returning ErrTenantAlreadyExists when a row with this
// TenantID already exists. It is the manual-registration path: an operator
// registers a tenant name before any business write has happened.
func (r *TenantRepository) Create(ctx context.Context, t *Tenant) error {
	if t.TenantID == "" {
		return ErrTenantIDRequired
	}
	if t.Status == "" {
		t.Status = tenancy.TenantStatusActive
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	err := r.db.WithContext(ctx).Create(t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrTenantAlreadyExists.WithParam("tenant_id", t.TenantID)
		}
		return err
	}
	return nil
}

// EnsureExists idempotently creates an active, blank-display-name ledger
// row for tenantID if none exists yet, and does nothing (reporting no
// error) if one already does. This is the event-driven lazy population
// path (tenant_service.go's org.node.created subscriber): a redelivered
// event, or a tenant already registered manually, must never fail or
// overwrite an operator's own edits.
//
// It reports whether it actually created a row, purely for the caller's
// own logging -- callers must not branch business behavior on it.
func (r *TenantRepository) EnsureExists(ctx context.Context, tenantID string) (created bool, err error) {
	if tenantID == "" {
		return false, ErrTenantIDRequired
	}
	err = r.db.WithContext(ctx).Create(&Tenant{
		TenantID:  tenantID,
		Status:    tenancy.TenantStatusActive,
		CreatedAt: time.Now().UTC(),
	}).Error
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return false, nil
	}
	return false, err
}

// Get returns the ledger row for tenantID, or ErrTenantNotFound.
func (r *TenantRepository) Get(ctx context.Context, tenantID string) (*Tenant, error) {
	var t Tenant
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTenantNotFound.WithParam("tenant_id", tenantID)
		}
		return nil, err
	}
	return &t, nil
}

// TenantFilter narrows a List call. Every field is optional; the zero
// TenantFilter{} lists every ledger row.
type TenantFilter struct {
	// Status, when non-empty, matches exactly.
	Status tenancy.TenantStatus
	// Limit bounds the number of rows returned. Zero uses
	// defaultTenantListLimit; anything above maxTenantListLimit is
	// clamped to it.
	Limit int
	// Cursor pages through results: List returns rows whose TenantID
	// sorts strictly after Cursor. Empty starts from the beginning.
	Cursor string
}

const (
	defaultTenantListLimit = 50
	maxTenantListLimit     = 500
)

// List returns ledger rows matching filter, ordered by TenantID for stable
// pagination.
func (r *TenantRepository) List(ctx context.Context, filter TenantFilter) ([]Tenant, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultTenantListLimit
	}
	if limit > maxTenantListLimit {
		limit = maxTenantListLimit
	}

	q := r.db.WithContext(ctx).Order("tenant_id")
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.Cursor != "" {
		q = q.Where("tenant_id > ?", filter.Cursor)
	}
	var rows []Tenant
	if err := q.Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// TenantPatch is Update's input: every non-nil field replaces the current
// value; a nil field leaves it untouched. This is what lets PATCH
// /api/v1/admin/tenants/{id} express "rename only" and "suspend only" (or
// both together) with the same method.
type TenantPatch struct {
	DisplayName     *string
	Status          *tenancy.TenantStatus
	SuspendedReason *string
	Notes           *string
}

// Update applies patch to the ledger row named by tenantID and returns the
// updated row, or ErrTenantNotFound.
//
// SuspendedAt is derived from the Status transition, never taken as a
// caller-supplied value (TenantPatch has no field for it): a patch whose
// Status becomes tenancy.TenantStatusSuspended while the row was not
// already suspended stamps SuspendedAt to now; a patch whose Status
// becomes anything else clears SuspendedAt to nil, matching its own doc
// comment ("nil means not currently suspended"); a patch that does not
// change Status at all, or restates the row's current Status, leaves
// SuspendedAt untouched.
//
// The read (this call's own Get) and the write are NOT one atomic
// operation -- see applyGuardedPatch's own doc comment for the
// compare-and-set guard that closes the resulting race.
func (r *TenantRepository) Update(ctx context.Context, tenantID string, patch TenantPatch) (*Tenant, error) {
	observed, err := r.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return r.applyGuardedPatch(ctx, tenantID, *observed, patch)
}

// applyGuardedPatch computes patch's effect on observed -- a snapshot this
// method's caller read moments earlier, ordinarily via Get -- and writes
// the result back under a conditional UPDATE whose WHERE clause requires
// every mutable column to still hold EXACTLY the value observed carried.
//
// The guard exists because an unconditional write would let two concurrent
// PATCH calls (one suspending, one resuming, say) both reading the row
// before either wrote back race -- the second write would always win,
// silently discarding the first's intent with no error to either caller,
// and the admin.tenant.status_changed audit event TenantService.SetStatus
// records from the CALLER'S OWN patch (never a re-read of what actually
// landed) would then describe a status the row never actually reached. The
// guard makes the second of two such concurrent writes affect 0 rows
// instead: refused with ErrTenantConcurrentUpdate rather than silently
// overwriting, the conditional-UPDATE idiom
// go/notification/send_record.go's SaveGuarded also uses (that guard is a
// business-semantic never-downgrade-succeeded check; this one is a plain
// optimistic-concurrency snapshot match, since admin_tenants carries no
// version column and PATCH's fields have no such fixed ordering to
// enforce) -- including that idiom's naming no .Model()/.Table(): GORM
// infers the table from t's own struct type via Updates(&t), where a
// map-shaped Updates call would otherwise have to name the table
// explicitly through the raw-GORM-bypass entry points.
//
// applyGuardedPatch is Update's own apply step, a separate method so a test
// can force the exact race deterministically -- two callers sharing one
// Get'd snapshot, racing their writes -- rather than relying on incidental
// goroutine timing.
func (r *TenantRepository) applyGuardedPatch(ctx context.Context, tenantID string, observed Tenant, patch TenantPatch) (*Tenant, error) {
	t := observed
	wasSuspended := t.Status == tenancy.TenantStatusSuspended

	if patch.DisplayName != nil {
		t.DisplayName = *patch.DisplayName
	}
	if patch.SuspendedReason != nil {
		t.SuspendedReason = *patch.SuspendedReason
	}
	if patch.Notes != nil {
		t.Notes = *patch.Notes
	}
	if patch.Status != nil {
		t.Status = *patch.Status
		nowSuspended := t.Status == tenancy.TenantStatusSuspended
		switch {
		case nowSuspended && !wasSuspended:
			now := time.Now().UTC()
			t.SuspendedAt = &now
		case !nowSuspended && wasSuspended:
			t.SuspendedAt = nil
		}
	}

	res := r.db.WithContext(ctx).
		Where("tenant_id = ? AND status = ? AND display_name = ? AND suspended_reason = ? AND notes = ?",
			tenantID, observed.Status, observed.DisplayName, observed.SuspendedReason, observed.Notes).
		Select("display_name", "status", "suspended_reason", "suspended_at", "notes").
		Updates(&t)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrTenantConcurrentUpdate.WithParam("tenant_id", tenantID)
	}
	return &t, nil
}
