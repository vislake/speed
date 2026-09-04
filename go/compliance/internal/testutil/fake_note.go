package testutil

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// FakeNote is a minimal tenant-scoped, SoftDeletable fixture model,
// standing in for a real business module's own model -- the shape
// pkgcore.RetentionParticipant's own doc comment assumes: a participant
// implements Sweep and Erase by calling its own dbkit.Repository[T].
// HardDelete, never by compliance code touching the table directly.
type FakeNote struct {
	ID        string     `gorm:"primaryKey;size:36"`
	TenantID  string     `gorm:"primaryKey;size:64;not null"`
	SubjectID string     `gorm:"size:64;not null"`
	Content   string     `gorm:"size:500;not null"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

// TableName pins FakeNote to a name that cannot collide with any real
// module's own table, so this fixture is safe to migrate onto a shared
// test database alongside real tables if a future test ever needs to.
func (FakeNote) TableName() string { return "compliance_test_fake_notes" }

// GetTenantID satisfies dbkit.TenantScoped.
func (n FakeNote) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(n.TenantID) }

// GetDeletedAt satisfies dbkit.SoftDeletable.
func (n FakeNote) GetDeletedAt() *time.Time { return n.DeletedAt }

// fakeNoteTableSQL is portable DDL, valid unchanged on both PostgreSQL and
// SQLite, mirroring go/dbkit/internal/testutil's SoftDeletableWidgetTableSQL
// precedent for a test-only fixture table: no dialect-specific types, no
// gen_random_uuid(), no NOW().
const fakeNoteTableSQL = `CREATE TABLE compliance_test_fake_notes (
	id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	subject_id TEXT NOT NULL,
	content TEXT NOT NULL,
	deleted_at TIMESTAMP NULL,
	deleted_by TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
);`

// NewDB returns a migrated SQLite *gorm.DB carrying FakeNote's table,
// built on dbkit.Open through dbtest.NewSQLite (dbkit's full wiring --
// tenant-isolation plugin, soft-delete auto-scope plugin -- included) plus
// this fixture's own DDL, executed directly since it is a test-only table
// rather than a real migration.
func NewDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	if err := db.Exec(fakeNoteTableSQL).Error; err != nil {
		t.Fatalf("testutil: create compliance_test_fake_notes table: %v", err)
	}
	return db
}

// compile-time checks that FakeNote satisfies both marker interfaces.
var (
	_ dbkit.TenantScoped  = FakeNote{}
	_ dbkit.SoftDeletable = FakeNote{}
)
