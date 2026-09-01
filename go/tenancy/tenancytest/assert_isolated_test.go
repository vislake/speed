package tenancytest

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// sprocket is a minimal tenant-scoped fixture used only to prove
// AssertIsolated works end to end against a real dbkit.Repository[T].
// dbkit's own tenant-scoped fixture (internal/testutil.Widget) lives in an
// unexported package reachable only from within the dbkit module itself
// (see dbkit/AGENTS.md), so every module that needs one of its own,
// including this package's own tests, defines a small fixture directly —
// exactly as this package's own doc comment on AssertIsolated tells callers
// to do. It follows the same exported "ID"/"TenantID" string-field
// convention dbkit.Repository[T] requires of every T (see idFieldName and
// tenantIDFieldName in assert_isolated.go).
type sprocket struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Label    string `gorm:"column:label;size:255"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s sprocket) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// compile-time check that sprocket satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = sprocket{}

// createSprocketsTableSQL is portable across both dialects dbkit supports —
// no dialect-specific types, per the project's dual-dialect rules — and is
// applied with a plain db.Exec rather than AutoMigrate: dbkit/dbtest applies
// no migration of its own on purpose (see its own doc comment), and
// AutoMigrate is banned project-wide even for a throwaway test table (see
// dbkit's own tenant_scope_test.go, which follows the identical pattern for
// its platformFlag fixture).
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

// newSprocketRepo migrates the sprockets table on db and returns a
// dbkit.Repository[sprocket] backed by it.
func newSprocketRepo(t *testing.T, db *gorm.DB) *dbkit.Repository[sprocket] {
	t.Helper()
	if err := db.Exec(createSprocketsTableSQL).Error; err != nil {
		t.Fatalf("create sprockets table: %v", err)
	}
	return dbkit.NewRepository[sprocket](db)
}

// newSprocket returns a factory suitable for AssertIsolated's newRecord
// parameter: every call returns a sprocket with a fresh, unique id.
func newSprocket(tenant pkgcore.TenantID) *sprocket {
	return &sprocket{
		ID:       fmt.Sprintf("sprocket-%d", sprocketIDSeq.Add(1)),
		TenantID: string(tenant),
		Label:    "gadget",
	}
}

// TestAssertIsolated_Sprocket proves AssertIsolated works end to end against
// a small tenant-scoped fixture of this package's own (see sprocket's doc
// comment for why it can't reuse dbkit's own Widget fixture).
//
// SQLite only, deliberately: this file carries no build tag, so per the
// backend coding standard's testing layout rule (§13) it must stay fast and
// hermetic under a plain "go test ./...". testutil.Dialects()'s postgres
// entry (index 1) starts a real, disposable PostgreSQL container via
// testcontainers-go on every call, which is exactly the cost that rule
// requires to live behind a package-level integration_test/ directory
// guarded by //go:build integration instead -- see dialects.go's own doc
// comment. The postgres leg of this same scenario runs from
// tenancytest/integration_test/postgres_assert_isolated_test.go, behind
// that tag, mirroring dbkit's own integration_test/ precedent one module
// over.
func TestAssertIsolated_Sprocket(t *testing.T) {
	repo := newSprocketRepo(t, testutil.Dialects()[0].NewDB(t)) // sqlite: see doc comment above for why postgres runs only in integration_test/
	AssertIsolated(t, repo, newSprocket)
}

// brokenTenantWidget is a deliberately broken TenantScoped fixture: its
// GetTenantID always reports a fixed, wrong tenant, completely ignoring its
// own TenantID column — the kind of copy-paste bug ("return the wrong
// field") a real model could plausibly ship with. It exists solely for
// TestAssertIsolated_DetectsBrokenGetTenantID below; see that test's own
// doc comment for why this, rather than a genuine cross-tenant leak, is the
// failure mode chosen to prove AssertIsolated has real detective power.
type brokenTenantWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
}

// alwaysWrongTenant can never equal a real tenant id AssertIsolated
// generates (isolationTenants always derives its ids from the calling
// test's name), so every defense-in-depth check in
// dbkit.Repository[T].FindByID that consults GetTenantID is guaranteed to
// disagree with the real caller's tenant for brokenTenantWidget.
const alwaysWrongTenant = pkgcore.TenantID("tenancytest-always-wrong-tenant")

// GetTenantID deliberately ignores TenantID — see the type's doc comment.
func (brokenTenantWidget) GetTenantID() pkgcore.TenantID { return alwaysWrongTenant }

// compile-time check that brokenTenantWidget satisfies dbkit.TenantScoped
// (it must, to be usable as AssertIsolated's T at all — its bug is in what
// GetTenantID answers, not in whether it exists).
var _ dbkit.TenantScoped = brokenTenantWidget{}

const createBrokenTenantWidgetsTableSQL = `CREATE TABLE broken_tenant_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	PRIMARY KEY (tenant_id, id)
)`

var brokenTenantWidgetIDSeq atomic.Uint64

// runBrokenGetTenantIDHelperEnv is the environment variable
// TestAssertIsolated_DetectsBrokenGetTenantID sets, and
// TestAssertIsolated_DetectsBrokenGetTenantID_Helper checks, to tell "run
// for real, as a deliberately-failing subprocess" apart from "just being
// enumerated by an ordinary go test ./..." — see the detector test's own
// doc comment for why this indirection exists at all.
const runBrokenGetTenantIDHelperEnv = "TENANCYTEST_RUN_BROKEN_GET_TENANT_ID_HELPER"

// TestAssertIsolated_DetectsBrokenGetTenantID_Helper is not meant to run as
// part of the ordinary suite — it skips itself unless
// runBrokenGetTenantIDHelperEnv is set — and exists solely to be invoked as
// a subprocess by TestAssertIsolated_DetectsBrokenGetTenantID (see that
// test's doc comment for why a subprocess, rather than a plain t.Run, is
// required here).
func TestAssertIsolated_DetectsBrokenGetTenantID_Helper(t *testing.T) {
	if os.Getenv(runBrokenGetTenantIDHelperEnv) != "1" {
		t.Skip("only meant to run as TestAssertIsolated_DetectsBrokenGetTenantID's subprocess")
	}

	db := testutil.Dialects()[0].NewDB(t) // sqlite: this fixture needs no dialect-specific behavior
	if err := db.Exec(createBrokenTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create broken_tenant_widgets table: %v", err)
	}
	repo := dbkit.NewRepository[brokenTenantWidget](db)
	newRecord := func(tenant pkgcore.TenantID) *brokenTenantWidget {
		return &brokenTenantWidget{
			ID:       fmt.Sprintf("broken-%d", brokenTenantWidgetIDSeq.Add(1)),
			TenantID: string(tenant),
		}
	}

	AssertIsolated(t, repo, newRecord)
}

// TestAssertIsolated_DetectsBrokenGetTenantID is AssertIsolated's negative
// test: proof that it can actually fail, not just that it happens to pass
// against already-correct code — "a verification helper that can't be shown
// to detect a real violation is not trustworthy on faith alone."
//
// It cannot prove the strongest version of that claim — that AssertIsolated
// catches a genuine cross-tenant LEAK, tenant B reading tenant A's row —
// because that failure mode cannot be manufactured against a real
// dbkit.Repository[T] at all: every WHERE clause Repository[T] issues is
// scoped by the tenant dbkit itself resolves from ctx (pkgcore.
// MustTenantFromContext), never by anything T or newRecord supply (see
// dbkit's repository.go: Create, FindByID and List all read the tenant from
// ctx and never consult the model's own fields or methods to decide who may
// see a row). No choice of fixture model or newRecord implementation can
// make a real Repository[T] hand back another tenant's row; that guarantee
// is dbkit's own, already proved by dbkit's own repository_test.go and
// tenant_scope_test.go, and is not this package's to re-prove.
//
// What AssertIsolated CAN be shown to catch is the other reachable
// direction: a TenantScoped implementation whose GetTenantID disagrees with
// its own persisted tenant. Repository[T].FindByID layers GetTenantID as a
// defense-in-depth check on top of the SQL WHERE clause (see repository.go),
// so a model that always reports the wrong tenant makes even ITS OWN
// tenant's legitimate FindByID calls fail — a real, plausible bug, and
// exactly the class of mistake a module author could actually ship. This
// test proves AssertIsolated's same-tenant-read assertion actually fires
// when that happens.
//
// Observing "the failing scenario actually failed" cannot be done with a
// plain t.Run and its bool return: Go's testing package gives a failing
// subtest no way to avoid marking every ancestor failed too —
// (*testing.common).Fail walks c.parent all the way up unconditionally, by
// design — so running the broken-fixture scenario as a subtest of this test
// would permanently fail this package's own `go test`, forever, which is
// not an acceptable price for a test that is doing exactly what it should.
// Instead this re-runs the test binary as a subprocess
// (testutil.ExpectFailingSubprocess), selecting only
// TestAssertIsolated_DetectsBrokenGetTenantID_Helper via -test.run, and
// inspects that subprocess's own exit code — the same technique net/http's
// and os/exec's own test suites use for code that is expected to fail. No
// fake *testing.T stand-in is needed or possible: *testing.T is a concrete
// struct with no exported interface seam for one.
func TestAssertIsolated_DetectsBrokenGetTenantID(t *testing.T) {
	output, failed := testutil.ExpectFailingSubprocess(t,
		"TestAssertIsolated_DetectsBrokenGetTenantID_Helper", runBrokenGetTenantIDHelperEnv)
	if !failed {
		t.Fatalf("AssertIsolated passed against a TenantScoped implementation whose GetTenantID always disagrees with its real tenant; want it to fail (same-tenant FindByID should have been denied). Helper subprocess output:\n%s", output)
	}
	if !strings.Contains(output, "same_tenant_find_succeeds") {
		t.Errorf("helper subprocess failed, as wanted, but apparently not because of the same_tenant_find_succeeds check; output:\n%s", output)
	}
}
