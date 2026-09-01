//go:build integration

// Package tenancytest_test holds tenancytest's integration tier: the
// postgres-dialect leg of tenancytest's own self-tests (AssertIsolated and
// AssertNotTenantScoped proving themselves end to end against a real
// dbkit.Repository[T] / *gorm.DB). It is physically separate from
// tenancytest's unit tests (assert_isolated_test.go,
// assert_not_tenant_scoped_test.go, and friends, all in package tenancytest
// itself, one directory up) and carries the "integration" build tag, per
// the backend coding standard's testing layout rule (§13): a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// go/dbkit/integration_test's own package-doc comment and directory shape
// one module over.
//
// Every test here starts its own disposable PostgreSQL container via
// testutil.Dialects()'s index-1 entry (dbtest.NewPostgres) and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path beyond what dbtest.NewPostgres itself already
// provides (t.Skip when no daemon is reachable) -- the SQLite leg of each of
// these same scenarios already runs unconditionally, in the unit tier, from
// the corresponding non-integration-tagged test named in each function's
// doc comment below.
package tenancytest_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// sprocket mirrors the unexported fixture of the same name in
// assert_isolated_test.go (package tenancytest, one directory up). This
// package is a separate compiled package rooted in a different directory,
// so it cannot reach that one's unexported type -- tenancytest's own doc
// comment on AssertIsolated tells every caller, including this one, to
// define its own small fixture directly rather than share one across
// packages.
type sprocket struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Label    string `gorm:"column:label;size:255"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s sprocket) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// compile-time check that sprocket satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = sprocket{}

// createSprocketsTableSQL matches assert_isolated_test.go's table of the
// same name exactly, so the postgres and sqlite legs of this scenario stay
// identical apart from dialect.
const createSprocketsTableSQL = `CREATE TABLE sprockets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	label VARCHAR(255) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

// sprocketIDSeq gives every sprocket fixture record a distinct id across an
// entire test binary run, satisfying AssertIsolated's requirement that
// newRecord never repeat an id.
var sprocketIDSeq atomic.Uint64

// newSprocket returns a factory suitable for AssertIsolated's newRecord
// parameter: every call returns a sprocket with a fresh, unique id.
func newSprocket(tenant pkgcore.TenantID) *sprocket {
	return &sprocket{
		ID:       fmt.Sprintf("sprocket-%d", sprocketIDSeq.Add(1)),
		TenantID: string(tenant),
		Label:    "gadget",
	}
}

// TestAssertIsolated_Sprocket_Postgres is the postgres-dialect leg of
// tenancytest.TestAssertIsolated_Sprocket (assert_isolated_test.go, package
// tenancytest); see that test's doc comment for what it proves and this
// package's own doc comment above for why the postgres leg lives here
// instead of there.
func TestAssertIsolated_Sprocket_Postgres(t *testing.T) {
	db := testutil.Dialects()[1].NewDB(t) // postgres: see this package's doc comment
	if err := db.Exec(createSprocketsTableSQL).Error; err != nil {
		t.Fatalf("create sprockets table: %v", err)
	}
	repo := dbkit.NewRepository[sprocket](db)

	tenancytest.AssertIsolated(t, repo, newSprocket)
}

// decoyTenantWidget mirrors the unexported fixture of the same name used
// by the unit tier's own system-context/Repository[T] composition test for
// this same scenario (package tenancytest, one directory up) -- see
// postgres_assert_isolated_test.go's doc comment on sprocket for why this
// package defines its own copy rather than sharing one.
type decoyTenantWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Note     string `gorm:"column:note;size:64"`
}

func (d decoyTenantWidget) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(d.TenantID) }

var _ dbkit.TenantScoped = decoyTenantWidget{}

const createDecoyTenantWidgetsTableSQL = `CREATE TABLE decoy_tenant_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	note VARCHAR(64) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

var decoyTenantWidgetIDSeq atomic.Uint64

func newDecoyTenantWidget(tenant pkgcore.TenantID) *decoyTenantWidget {
	return &decoyTenantWidget{
		ID:       fmt.Sprintf("decoy-widget-%d", decoyTenantWidgetIDSeq.Add(1)),
		TenantID: string(tenant),
		Note:     "gadget",
	}
}

// decoySystemPurpose is this package's own registration, distinct from the
// unit tier's identically-purposed constant in its own system-context
// composition test: the two run as separate test binaries (different
// packages), so nothing requires the string values to match, and keeping
// them textually distinct avoids any confusion if a system-purpose audit
// log ever sees both. See pkgcore.RegisterSystemPurpose's own doc comment
// for why registering a purpose more than once is a safe no-op.
const decoySystemPurpose = pkgcore.SystemPurpose("tenancytest.assert_isolated_system_context.decoy_postgres")

// TestAssertIsolated_SystemCtxElevatedDB_Postgres is the postgres-dialect
// leg of tenancytest.TestAssertIsolated_SystemCtxElevatedDB, which proves
// that AssertIsolated's guarantees hold unchanged even when the
// Repository[T]'s underlying *gorm.DB was itself built from a base context
// already elevated via pkgcore.WithSystemContext and scoped to an unrelated
// decoy tenant; see that test's own doc comment for the full mechanism and
// postgres_assert_isolated_test.go's own doc comment for why the postgres
// leg lives here instead of there.
func TestAssertIsolated_SystemCtxElevatedDB_Postgres(t *testing.T) {
	pkgcore.RegisterSystemPurpose(decoySystemPurpose)

	const decoyTenant = pkgcore.TenantID("tenancytest-decoy-tenant-should-never-be-used")
	decoyCtx := pkgcore.WithTenant(context.Background(), decoyTenant)
	elevatedDecoyCtx, err := pkgcore.WithSystemContext(decoyCtx, pkgcore.SystemReason{
		Actor: "test-setup", Purpose: decoySystemPurpose, Ticket: "SUP-DECOY",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext(decoy) error = %v, want success", err)
	}

	baseDB := testutil.Dialects()[1].NewDB(t) // postgres: see postgres_assert_isolated_test.go's doc comment
	if err = baseDB.Exec(createDecoyTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create decoy_tenant_widgets table: %v", err)
	}

	// The critical setup step: build the Repository[T] from a *gorm.DB
	// whose OWN base session context is already elevated and already
	// tenant-scoped to the decoy tenant, before AssertIsolated ever calls
	// Create/FindByID/List with its own, unrelated per-call contexts.
	elevatedBase := baseDB.WithContext(elevatedDecoyCtx)
	repo := dbkit.NewRepository[decoyTenantWidget](elevatedBase)

	tenancytest.AssertIsolated(t, repo, newDecoyTenantWidget)

	// Explicit confirmation, beyond whatever AssertIsolated itself already
	// checked: the decoy tenant baked into the base db's context never
	// received any row at all. Reading it back uses an ordinary,
	// non-elevated context for the decoy tenant -- deliberately not
	// elevatedDecoyCtx again -- so this List call is itself a plain,
	// unprivileged same-tenant read.
	decoyRows, err := repo.List(pkgcore.WithTenant(context.Background(), decoyTenant))
	if err != nil {
		t.Fatalf("List(decoy tenant) error = %v", err)
	}
	if len(decoyRows) != 0 {
		t.Errorf("List(decoy tenant) = %d rows, want 0; the Repository's base *gorm.DB context (elevated, scoped to the decoy tenant) must never leak into an AssertIsolated-driven write", len(decoyRows))
	}
}

// TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects_Postgres
// is the postgres-dialect leg of
// tenancytest.TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects,
// the regression test proving a long, descriptive subtest name can no
// longer derive a tenant id overflowing the 64-character tenant_id column
// -- see that test's own doc comment for the full mechanism. PostgreSQL is
// the dialect that actually enforces VARCHAR(64) and is what surfaced the
// tenant-id-length bug being regression-tested in the first place, so this
// leg matters more than most -- and postgres_assert_isolated_test.go's own
// doc comment for why it lives here instead of in the unit tier.
//
// Reuses the sprocket fixture (type, newSprocket, createSprocketsTableSQL)
// declared in postgres_assert_isolated_test.go: same package, same
// directory, so no re-declaration is needed the way the unit tier's own
// AGENTS.md/doc-comment guidance requires across separate packages.
func TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects_Postgres(t *testing.T) {
	db := testutil.Dialects()[1].NewDB(t) // postgres: see postgres_assert_isolated_test.go's doc comment
	if err := db.Exec(createSprocketsTableSQL).Error; err != nil {
		t.Fatalf("create sprockets table: %v", err)
	}
	repo := dbkit.NewRepository[sprocket](db)

	t.Run("ADeliberatelyLongAndDescriptiveSubtestNameLikeTheProjectConventionAsksFor", func(t *testing.T) {
		tenancytest.AssertIsolated(t, repo, newSprocket)
	})
}
