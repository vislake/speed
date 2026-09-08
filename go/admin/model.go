package admin

import (
	"time"

	"github.com/vislake/speed/go/tenancy"
)

// Tenant is one row of admin_tenants, the operator-facing TENANT LEDGER:
// a record of which tenants the platform believes exist, never the
// authoritative source of tenant existence (pkgcore.TenantID stays an
// opaque string everywhere else, and no other module's write path checks
// this table before writing).
//
// This is platform data: it describes ALL tenants, not the affairs of one,
// so it must NOT implement dbkit.TenantScoped -- doing so would put "which
// tenants exist" itself behind a single tenant's own isolation filter,
// which is incoherent. Every write goes through dbkit.Open()'s plain
// *gorm.DB directly (tenant_repository.go), and
// TestTenant_IsNotTenantScoped (model_test.go) runs
// tenancytest.AssertNotTenantScoped over it.
type Tenant struct {
	// TenantID is the primary key: the string value of the
	// pkgcore.TenantID this row describes.
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`

	// DisplayName is empty on a row created by the event-driven lazy
	// population path until an operator fills it in, and whatever the
	// operator supplied on a manually created row.
	DisplayName string `gorm:"column:display_name;size:255;not null"`

	// Status is the tenant's ledger status -- which state the platform
	// operator believes this tenant is in. It is typed with go/tenancy's
	// own TenantStatus, the vocabulary tenancy.Middleware's optional
	// TenantStatusResolver gate reports, deliberately ONE type rather than
	// an admin-local duplicate plus a hand-written translation. With the
	// shared type there is no translation to forget: a stored value is
	// reported to the seam as-is, and the gate refuses anything other
	// than tenancy.TenantStatusActive by default, so a future ledger
	// state a tenant must not be served under (an "archived" tenant, say)
	// needs no code change here at all -- declaring it as a
	// tenancy.TenantStatus value is enough, and the gate refuses it. A
	// state a tenant's requests SHOULD flow under requires the deliberate
	// widening of tenancy.Middleware's own check, which is where that
	// decision belongs.
	Status tenancy.TenantStatus `gorm:"column:status;size:32;not null"`

	// SuspendedReason is the operator-supplied reason recorded the last
	// time Status became tenancy.TenantStatusSuspended. It is left in
	// place (not cleared) when the tenant is later resumed, as a
	// historical note -- SuspendedAt being nil is what says "not currently
	// suspended", not this field being empty.
	SuspendedReason string `gorm:"column:suspended_reason;size:2000;not null"`

	// SuspendedAt is when Status last became
	// tenancy.TenantStatusSuspended, or nil when the tenant is not
	// currently suspended.
	SuspendedAt *time.Time `gorm:"column:suspended_at"`

	// CreatedAt is when this ledger row was created -- NOT necessarily
	// when the tenant's first business data was written, on the
	// event-driven path this lags slightly behind the real first write.
	CreatedAt time.Time `gorm:"column:created_at;not null"`

	// CreatedBy is the operator user id that manually registered this row,
	// or empty when the row was created by the event-driven lazy
	// population path, which has no human operator to name.
	CreatedBy string `gorm:"column:created_by;size:64;not null"`

	// Notes is free-text operator commentary -- a sales contact, an
	// account tier, anything the operator wants on record. Never
	// user-facing, never rendered to the tenant itself.
	Notes string `gorm:"column:notes;size:4000;not null"`
}

// TableName implements gorm's Tabler, naming the table explicitly rather
// than relying on GORM's pluralization of the Go type name.
func (Tenant) TableName() string { return "admin_tenants" }

// ImpersonationGrant is one row of admin_impersonation_grants: the
// short-lived, explicitly revocable authorization credential issued when a
// platform administrator starts an impersonation session. It is never a
// real authn access or refresh token -- impersonation substitutes the
// target's identity for the rest of one request, never mints credentials
// the target (or the impersonating administrator) could carry away.
//
// Like Tenant, this is platform data and must NOT implement
// dbkit.TenantScoped: one grant names a target tenant as DATA, but the
// table itself describes grants across every tenant, and only a platform
// administrator (never a tenant's own members) ever reads or writes it.
// TestImpersonationGrant_IsNotTenantScoped (model_test.go) runs
// tenancytest.AssertNotTenantScoped over it.
type ImpersonationGrant struct {
	// ID is the primary key: the grant's own credential value, a random,
	// unguessable identifier (never derived from AdminUserID or
	// TargetUserID). It is what a caller presents in the
	// X-Admin-Impersonation request header.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// AdminUserID is the platform administrator who started the grant.
	AdminUserID string `gorm:"column:admin_user_id;size:64;not null;index"`

	// TargetUserID is the user being impersonated.
	TargetUserID string `gorm:"column:target_user_id;size:64;not null;index"`

	// TargetTenantID is the tenant the impersonation is scoped to. A grant
	// is valid inside exactly this one tenant -- it has no meaning, and is
	// never consulted, for a request resolving to any other tenant.
	TargetTenantID string `gorm:"column:target_tenant_id;size:64;not null;index"`

	// Reason is the operator-supplied justification, required at creation
	// (ErrImpersonationReasonRequired otherwise) -- the reason is itself
	// part of the audit trail.
	Reason string `gorm:"column:reason;size:2000;not null"`

	CreatedAt time.Time `gorm:"column:created_at;not null"`

	// ExpiresAt is when the grant stops being usable on its own, with no
	// action from anyone -- the short-lived half of "short-lived,
	// explicitly revocable" (defaultGrantTTL's own doc comment names the
	// default). It is never extended: there is no renew operation.
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// EndedAt is when the grant was explicitly ended before its natural
	// expiry (DELETE /api/v1/admin/impersonation/{id}), or nil when it has
	// not been. A grant can be inactive (Active reports false) with
	// EndedAt still nil, simply because ExpiresAt has passed -- EndedAt
	// specifically means "someone ended this early".
	EndedAt *time.Time `gorm:"column:ended_at"`

	// EndedBy is the user id that ended the grant early -- the
	// administrator who started it, or a higher-privileged operator.
	// Empty when EndedAt is nil.
	EndedBy string `gorm:"column:ended_by;size:64;not null"`
}

// TableName implements gorm's Tabler.
func (ImpersonationGrant) TableName() string { return "admin_impersonation_grants" }

// Active reports whether g is still a usable impersonation credential at
// instant now: not explicitly ended, and not past its natural expiry.
//
// This is the ONE method every consumer of a looked-up grant must call
// before trusting it (the middleware in particular -- see
// ImpersonationMiddleware in pipeline.go) -- a grant read back from storage
// that fails this check must be treated exactly like no grant at all,
// falling back to the administrator's own real identity, never failing
// open as the target user.
func (g ImpersonationGrant) Active(now time.Time) bool {
	if g.EndedAt != nil {
		return false
	}
	return now.Before(g.ExpiresAt)
}
