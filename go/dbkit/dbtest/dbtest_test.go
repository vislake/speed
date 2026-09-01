// dbtest_test.go is a deliberate, named exception to this repo's usual
// "one test file per source file" convention (backend coding standard
// §13): it is the explicitly-designated happy-path suite for dbtest's two
// public entry points taken together, NewSQLite and NewPostgres, the same
// way example_test.go is a recognized single exception for Example
// functions. dockerAvailable/dockerHostAddress -- the one genuinely
// source-file-scoped piece of test-worthy logic in this package -- has its
// own docker_probe_test.go instead, following the ordinary convention.
package dbtest_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tenantA and tenantB are the two tenants used by this file's round-trip
// checks, matching the naming already established by dbkit's own
// tenant_scope_test.go and integration_test files.
const (
	tenantA = pkgcore.TenantID("tenant-a")
	tenantB = pkgcore.TenantID("tenant-b")
)

// probe is a minimal tenant-scoped fixture used only to prove that the
// *gorm.DB NewSQLite/NewPostgres hand back is genuinely usable end to end
// -- through dbkit.Repository[T], exactly as a real caller would use it --
// not merely that dbkit.Open succeeded. It is deliberately not
// dbkit's own internal/testutil.Widget: that fixture is unexported and
// unreachable from this external package, which is exactly the situation
// dbtest itself exists to fix for every OTHER module (see dbtest's own doc
// comment) -- this package's tests are held to the same standard as any
// other caller.
type probe struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Name     string `gorm:"size:255;not null"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (p probe) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(p.TenantID) }

// TableName pins probe's table name explicitly, matching the raw CREATE
// TABLE in createProbeTable independent of GORM's pluralization rules.
func (probe) TableName() string { return "dbtest_probe" }

var _ dbkit.TenantScoped = probe{}

// isRecordNotFound reports whether err is dbkit.ErrRecordNotFound, matched
// by Code rather than identity: apperr.WithParam always returns a new
// *apperr.Error, so the pointer a Repository method returns is never the
// same pointer as the package-level sentinel (see dbkit's own
// repository_test.go and AGENTS.md's Rules section for the same pattern).
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// createProbeTable adds probe's table via a plain Exec -- never
// AutoMigrate, per the project-wide rule -- with a schema portable across
// both dialects, so the identical statement works whether db came from
// NewSQLite or NewPostgres.
func createProbeTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	err := db.Exec(`CREATE TABLE dbtest_probe (
		id        VARCHAR(26)  NOT NULL,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error
	if err != nil {
		t.Fatalf("create dbtest_probe table: %v", err)
	}
}

// assertRepositoryRoundTrip proves db is a fully-wired dbkit connection --
// not just an open one -- by driving dbkit.Repository[T]'s own public API
// against it: a Create followed by a FindByID under one tenant, and a
// cross-tenant FindByID that must come back ErrRecordNotFound. That second
// half only passes if dbkit.Open's tenant-scoping plugin (isolation layer
// 1) or Repository[T]'s own independent check (layer 2) is genuinely
// active on db -- exactly the "callers get all three isolation layers, not
// a bare connection" promise both NewSQLite and NewPostgres document.
func assertRepositoryRoundTrip(t *testing.T, db *gorm.DB) {
	t.Helper()
	createProbeTable(t, db)

	repo := dbkit.NewRepository[probe](db)
	ctx := pkgcore.WithTenant(context.Background(), tenantA)

	want := &probe{ID: "probe-1", Name: "gadget"}
	if err := repo.Create(ctx, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.FindByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.Name != "gadget" || got.TenantID != string(tenantA) {
		t.Errorf("FindByID() = %+v, want {ID:%s TenantID:%s Name:gadget}", *got, want.ID, tenantA)
	}

	otherTenantCtx := pkgcore.WithTenant(context.Background(), tenantB)
	if _, err := repo.FindByID(otherTenantCtx, want.ID); !isRecordNotFound(err) {
		t.Errorf("FindByID() from a different tenant error = %v, want ErrRecordNotFound (tenant isolation must be active on a dbtest-provided connection)", err)
	}
}

// TestNewSQLite_RepositoryRoundTrip is NewSQLite's happy path: open, apply
// a caller's own migration (createProbeTable stands in for it here), and
// drive a real Repository[T] through it successfully.
func TestNewSQLite_RepositoryRoundTrip(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if db == nil {
		t.Fatal("NewSQLite() = nil")
	}
	assertRepositoryRoundTrip(t, db)
}

// TestNewSQLite_CalledTwice_ReturnsIndependentDatabases locks in NewSQLite's
// own "private, per-call" promise, mirroring
// internal/testutil.NewTestSQLite's identically-named test for the same
// property.
func TestNewSQLite_CalledTwice_ReturnsIndependentDatabases(t *testing.T) {
	ctx := pkgcore.WithTenant(context.Background(), tenantA)

	first := dbtest.NewSQLite(t)
	createProbeTable(t, first)
	if err := dbkit.NewRepository[probe](first).Create(ctx, &probe{ID: "only-on-first", Name: "gadget"}); err != nil {
		t.Fatalf("Create() on the first database error = %v", err)
	}

	second := dbtest.NewSQLite(t)
	createProbeTable(t, second)
	list, err := dbkit.NewRepository[probe](second).List(ctx)
	if err != nil {
		t.Fatalf("List() on the second database error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() on the second database = %+v, want empty; NewSQLite must not share state across calls", list)
	}
}

// TestNewPostgres_RepositoryRoundTrip is NewPostgres's happy path, run
// against a real, disposable PostgreSQL container. It reports itself
// skipped rather than failed on a machine with no Docker (see
// docker_probe_test.go for the isolated proof that the skip-detection
// logic itself fails closed); this development/CI environment has Docker
// available, so here it is expected to run for real, not skip.
func TestNewPostgres_RepositoryRoundTrip(t *testing.T) {
	db := dbtest.NewPostgres(t)
	if db == nil {
		t.Fatal("NewPostgres() = nil")
	}
	assertRepositoryRoundTrip(t, db)
}
