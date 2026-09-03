package testutil

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// SoftDeletableWidget is dbkit's minimal SoftDeletable fixture model: a
// tenant-scoped model with the composite (tenant_id, id) primary key
// convention, one ordinary Name column, and the DeletedAt/DeletedBy pair
// dbkit.SoftDeletable requires.
//
// It exists as its own fixture, distinct from Widget, deliberately: Widget
// is load-bearing for many existing, unrelated tests (audit capture's own
// field-set assertions, migration tests), so retrofitting two new columns
// onto it would ripple into tests this round has no business touching —
// exactly the precedent IDAndTenantOnlyMarker already set for the same
// reason.
type SoftDeletableWidget struct {
	ID        string     `gorm:"primaryKey;size:26"`
	TenantID  string     `gorm:"primaryKey;size:26;not null"`
	Name      string     `gorm:"size:255;not null"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

// GetTenantID returns the tenant SoftDeletableWidget belongs to, satisfying
// dbkit.TenantScoped.
func (w SoftDeletableWidget) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(w.TenantID)
}

// GetDeletedAt returns w's soft-delete marker, satisfying
// dbkit.SoftDeletable. Like TenantScoped.GetTenantID, this is never called
// by dbkit's own soft-delete plugin or by Repository[T] itself — it is a
// pure marker used only for the type assertion that opts w into the
// soft-delete path; the actual field writes go through reflection on fixed
// field names.
func (w SoftDeletableWidget) GetDeletedAt() *time.Time { return w.DeletedAt }

// AuditResourceType returns "soft_deletable_widget", satisfying
// dbkit.Auditable, so this fixture can also stand in for the
// audit-capture-classifies-soft-delete-as-update proof (see
// audit_capture_test.go) alongside its tenant-scoping and soft-delete
// roles — a real business model participating in all three together is the
// realistic shape this fixture is meant to approximate.
func (w SoftDeletableWidget) AuditResourceType() string { return "soft_deletable_widget" }

// SoftDeletableWidgetTableSQL is the DDL that creates the
// soft_deletable_widgets table backing SoftDeletableWidget, as a single
// portable Go string constant rather than two per-dialect migration files —
// following IDAndTenantOnlyMarkerTableSQL's exact precedent. Both
// statements below are byte-identical, valid DDL on both PostgreSQL and
// SQLite (docs/internal/04-data-and-tenancy.md's dual-dialect constraint:
// "CREATE UNIQUE INDEX ... WHERE deleted_at IS NULL" is standard,
// non-dialect-specific SQL supported by both engines), so this one literal
// is the dual-dialect proof itself: callers db.Exec it directly against
// either a SQLite unit-test connection or a real PostgreSQL integration
// connection, with no separate migration files to keep in sync.
//
// The partial unique index on (tenant_id, name) WHERE deleted_at IS NULL is
// this round's adjudicated answer to the unique-index interaction
// docs/internal/04-data-and-tenancy.md's §4 names as a design decision
// every soft-deletable model must make explicitly: a soft-deleted row is
// still a real row and still occupies a plain unique constraint, so a name
// cannot be reused until the row is folded out of the index somehow. This
// fixture proves the partial-index answer — reuse allowed immediately after
// soft-delete, once the prior row's deleted_at is no longer NULL — is a
// real, working, dual-dialect-safe option; the alternative ("accept no
// reuse until hard-deleted") remains legitimate for a model that wants it,
// and this fixture takes no position on which a future caller should pick,
// only that the choice must be made deliberately. See soft_delete_unique_index_test.go
// and go/dbkit/AGENTS.md's "Soft deletion" section for the general guidance
// this proof backs.
const SoftDeletableWidgetTableSQL = `CREATE TABLE soft_deletable_widgets (id TEXT NOT NULL, tenant_id TEXT NOT NULL, name TEXT NOT NULL, deleted_at TIMESTAMP NULL, deleted_by TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE UNIQUE INDEX uq_soft_deletable_widgets_tenant_name ON soft_deletable_widgets (tenant_id, name) WHERE deleted_at IS NULL;`
