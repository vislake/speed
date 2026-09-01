package tenancytest

import (
	"context"
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

// This file used to document an upstream dbkit.Repository[T].Update defect
// that independent testing of THIS package surfaced: AssertIsolated, run
// against a TenantScoped model whose ONLY fields are its "ID"/"TenantID"
// primary key (no other column at all), used to correctly report a failure
// -- but the failure was dbkit's, not AssertIsolated's, own.
//
// Root cause, confirmed by tracing gorm.io/gorm v1.31.2's own
// callbacks/update.go: Repository[T].Update issues
// tx.Where(id...).Where(tenant_id...).Select("*").Save(m). GORM's Update
// callback builds the statement's SET clause from ConvertToAssignments(stmt)
// -- which considers only NON-primary-key columns, since primary-key
// columns belong in the WHERE clause, not SET -- and when that came back
// empty, the callback took an early return (see callbacks/update.go's
// Update func) that built and executed no SQL at all, leaving
// RowsAffected at 0. dbkit.Repository[T] treated rowsAffected == 0 as "no
// such row for this tenant" and returned ErrRecordNotFound (see
// repository.go's Update), even though the row genuinely existed and was
// genuinely owned by the calling tenant.
//
// dbkit.Repository[T].Update has since closed this gap: when gorm reports
// that no SQL was built at all (not merely that RowsAffected == 0), Update
// now falls back to an explicit, still id-and-tenant-scoped existence
// check inside the same transaction before deciding between success and
// ErrRecordNotFound -- see Update's own doc comment in
// go/dbkit/repository.go, and TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp
// / _DifferentTenant_ReturnsNotFound / _NoSuchID_ReturnsNotFound in
// go/dbkit/repository_test.go for dbkit's own fix and its regression
// coverage (including proof the fallback stays tenant-isolated: a
// cross-tenant Update attempt against this exact model shape still
// collapses to ErrRecordNotFound, never a false-positive success).
//
// What stays in THIS package, now that the dbkit gap itself is fixed at
// its source, is proof that AssertIsolated correctly PASSES bareTenantWidget
// -- the narrowest legal TenantScoped shape, a "pure marker" link/tenant
// table whose entire content IS its identity and tenant (a plausible,
// spec-legal shape for the tenant/link data domain, backend coding
// standard §3.3) -- rather than this package silently losing coverage of
// that shape now that dbkit no longer trips on it.
// TestAssertIsolated_MinimalTenantScopedModel is now a forward-looking
// regression guard, not a defect report: if dbkit's Update fallback above
// is ever weakened or removed, this is the test that notices, by failing
// on the same same_tenant_update_succeeds check that used to fail before
// the dbkit fix.

// bareTenantWidget is deliberately the narrowest possible TenantScoped
// model: nothing beyond the "ID"/"TenantID" pair dbkit.Repository[T]
// itself requires. Every OTHER fixture in this package (sprocket,
// platformSetting, decoyTenantWidget, brokenTenantWidget included)
// declares at least one additional column and so never exercises this
// path; that is precisely the gap this file closes.
type bareTenantWidget struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
}

func (b bareTenantWidget) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(b.TenantID) }

var _ dbkit.TenantScoped = bareTenantWidget{}

const createBareTenantWidgetsTableSQL = `CREATE TABLE bare_tenant_widgets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	PRIMARY KEY (tenant_id, id)
)`

var bareTenantWidgetIDSeq atomic.Uint64

func newBareTenantWidget(tenant pkgcore.TenantID) *bareTenantWidget {
	return &bareTenantWidget{
		ID:       fmt.Sprintf("bare-%d", bareTenantWidgetIDSeq.Add(1)),
		TenantID: string(tenant),
	}
}

// TestAssertIsolated_MinimalTenantScopedModel proves AssertIsolated works
// end to end against the narrowest legal TenantScoped shape: a model with
// no column beyond ID/TenantID. See this file's doc comment for the
// dbkit.Repository[T].Update gap this test used to expose (fixed in
// go/dbkit/repository.go) and why this test's job, now that the fix is in
// place, is to guard against it ever regressing rather than to document
// the defect itself -- a plain, direct assertion that AssertIsolated
// passes, exactly like TestAssertIsolated_Sprocket in assert_isolated_test.go,
// rather than the subprocess-based "expect a failure" machinery this test
// used before the dbkit fix landed (that machinery is still the right tool
// for a genuinely-expected failure; see
// TestAssertIsolated_DetectsBrokenGetTenantID in assert_isolated_test.go
// for that pattern and why it exists).
//
// SQLite only, deliberately, mirroring TestAssertIsolated_Sprocket in
// assert_isolated_test.go: this file carries no build tag, so per the
// backend coding standard's testing layout rule (§13) it must stay fast and
// hermetic under a plain "go test ./...". testutil.Dialects()[0] is always
// SQLite; see that function's own doc comment for why a plain _test.go file
// must never reach index 1 (PostgreSQL, via a real disposable container).
func TestAssertIsolated_MinimalTenantScopedModel(t *testing.T) {
	db := testutil.Dialects()[0].NewDB(t) // sqlite: see doc comment above for why postgres has no place in this file
	if err := db.Exec(createBareTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create bare_tenant_widgets table: %v", err)
	}
	repo := dbkit.NewRepository[bareTenantWidget](db)

	AssertIsolated(t, repo, newBareTenantWidget)
}

// This file investigates a combination the package's own doc comment and
// assert_isolated_test.go never consider: what happens when the *gorm.DB
// underlying the *dbkit.Repository[T] handed to AssertIsolated was itself
// obtained through a context.Context that pkgcore.WithSystemContext had
// already elevated -- e.g. a caller that does
// db.WithContext(systemElevatedCtx) before ever constructing the
// Repository, on the theory that "this repository operates with elevated
// privileges by default."
//
// The short answer, proved below: it does not matter, and AssertIsolated
// works identically either way -- because that combination cannot actually
// smuggle anything through. Every dbkit.Repository[T] method
// (Create/FindByID/Update/Delete/List) ignores whatever context its own
// *gorm.DB happened to be constructed or last used with; each one instead
// calls dbkit.WithTenantSession(ctx, r.db, fn), which itself calls
// r.db.WithContext(ctx) using the ctx PARAMETER PASSED TO THAT METHOD CALL
// (AssertIsolated's own ctxA/ctxB), not r.db's own stored context. GORM's
// WithContext replaces a session's Statement.Context outright rather than
// merging it with whatever the receiver already carried (confirmed by the
// decoy-tenant assertions below, not merely asserted from reading GORM's
// source), so a context value set on the base *gorm.DB before
// dbkit.NewRepository[T] ever saw it has no path left by which it could
// reach an actual query. "Elevating" the base db this way is not dangerous
// -- it is simply inert, silently discarded on the first real call.
//
// This means the combination the question asks about does not really
// "make sense" as a way to grant a Repository[T] elevated privileges: there
// is no such thing as an "elevated Repository[T]" today, only individual
// elevated CONTEXT VALUES passed to individual method calls (and, per
// system_context_repository_test.go in the parent tenancy package, even
// those have no effect on Repository[T] yet either). A caller trying to
// build a "privileged repository" by pre-elevating db is relying on
// behavior that silently does nothing, which is a plausible real mistake --
// exactly the kind AssertIsolated ought to remain correct in the presence
// of, which is what this file confirms rather than assumes.

// decoyTenantWidget is a tenant-scoped fixture local to this file, distinct
// from sprocket and brokenTenantWidget so this investigation does not lean
// on either of their setups. It deliberately carries a non-key Note field
// -- see assert_isolated_minimal_model_test.go in this same package for why
// that is load-bearing here rather than cosmetic: a TenantScoped model
// whose ONLY fields are its "ID"/"TenantID" primary key trips an unrelated
// dbkit.Repository[T].Update defect this file has nothing to do with
// investigating, and every other fixture in this package (sprocket,
// platformSetting) already avoids it the same way, by happening to declare
// an extra column.
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

// decoySystemPurpose is registered once for every subtest in this file; see
// pkgcore.RegisterSystemPurpose's own doc comment for why registering the
// same purpose more than once is a safe no-op.
const decoySystemPurpose = pkgcore.SystemPurpose("tenancytest.assert_isolated_system_context.decoy")

// TestAssertIsolated_SystemCtxElevatedDB proves
// AssertIsolated's own guarantees hold unchanged when the *gorm.DB behind
// the Repository[T] it is given was built from a base context that was
// itself elevated via pkgcore.WithSystemContext AND ALSO carried a decoy
// tenant no test-driven call ever uses. If that base context leaked into
// any real query -- through some future refactor of WithTenantSession that
// starts trusting r.db's own stored context instead of always deriving from
// the per-call ctx -- rows would end up written under decoyTenant instead
// of the tenants AssertIsolated itself derives from t.Name(), and
// AssertIsolated's own subtests (same_tenant_find_succeeds in particular)
// would fail on their own. The explicit decoy-tenant row count check at the
// end additionally rules out a subtler failure AssertIsolated's own
// assertions would not catch by themselves: rows silently ALSO becoming
// visible under decoyTenant without disturbing tenant A/B's own view of
// their data.
//
// SQLite only -- see TestAssertIsolated_Sprocket's doc comment
// (assert_isolated_test.go) for why a plain _test.go file must not reach
// testutil.Dialects()'s postgres entry. The postgres leg of this same
// scenario runs from
// tenancytest/integration_test/postgres_assert_isolated_system_context_test.go,
// behind //go:build integration.
func TestAssertIsolated_SystemCtxElevatedDB(t *testing.T) {
	pkgcore.RegisterSystemPurpose(decoySystemPurpose)

	const decoyTenant = pkgcore.TenantID("tenancytest-decoy-tenant-should-never-be-used")
	decoyCtx := pkgcore.WithTenant(context.Background(), decoyTenant)
	elevatedDecoyCtx, err := pkgcore.WithSystemContext(decoyCtx, pkgcore.SystemReason{
		Actor: "test-setup", Purpose: decoySystemPurpose, Ticket: "SUP-DECOY",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext(decoy) error = %v, want success", err)
	}

	baseDB := testutil.Dialects()[0].NewDB(t) // sqlite: see doc comment above for why postgres runs only in integration_test/
	if err = baseDB.Exec(createDecoyTenantWidgetsTableSQL).Error; err != nil {
		t.Fatalf("create decoy_tenant_widgets table: %v", err)
	}

	// The critical setup step: build the Repository[T] from a *gorm.DB
	// whose OWN base session context is already elevated and already
	// tenant-scoped to the decoy tenant, before AssertIsolated ever calls
	// Create/FindByID/List with its own, unrelated per-call contexts.
	elevatedBase := baseDB.WithContext(elevatedDecoyCtx)
	repo := dbkit.NewRepository[decoyTenantWidget](elevatedBase)

	AssertIsolated(t, repo, newDecoyTenantWidget)

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

// This file is a regression test for a bug independent testing found in
// isolationTenants (assert_isolated.go): it built every tenant id as
// "tenancytest-" + sanitizeForTenantID(t.Name()) + "-a"/"-b" with no length
// bound at all. This project's own testing convention explicitly asks for
// descriptive case/test names (backend-coding-standards §13), and a
// sufficiently descriptive t.Name() -- entirely plausible, not a contrived
// edge case -- produced a tenant id longer than the VARCHAR(64) tenant_id
// column every fixture in this package (and the backend coding standard's
// own Subscription example) uses. SQLite silently accepts a value of any
// length, so this was completely invisible there; PostgreSQL enforces
// VARCHAR(n) strictly and rejected it with "value too long for type
// character varying(64)" (SQLSTATE 22001) -- a failure that reads as
// unrelated to the isolation property actually under test, discovered only
// once such a test happened to run against Postgres.
//
// The fix bounds the derived name segment to isolationNameBudget, replacing
// an overflowing name's tail with a short deterministic hash of the FULL
// original name (see boundedTenantIDSegment) rather than truncating alone,
// so two long names sharing a common prefix still cannot collide once
// shortened.

// TestBoundedTenantIDSegment_ShortNamePassesThroughUnchanged proves the fix
// is fully backward compatible: any test name that already fits within
// budget derives the exact same segment sanitizeForTenantID alone would
// have produced, so no existing (short) test name's tenant id changes.
func TestBoundedTenantIDSegment_ShortNamePassesThroughUnchanged(t *testing.T) {
	const name = "TestAssertIsolated_Sprocket/sqlite"
	if len(name) > isolationNameBudget {
		t.Fatalf("test fixture error: name %q (len %d) does not actually fit isolationNameBudget (%d); pick a shorter one", name, len(name), isolationNameBudget)
	}

	got := boundedTenantIDSegment(name)
	want := sanitizeForTenantID(name)
	if got != want {
		t.Errorf("boundedTenantIDSegment(%q) = %q, want %q (unchanged from sanitizeForTenantID since it already fits)", name, got, want)
	}
}

// TestBoundedTenantIDSegment_LongNameIsBoundedAndDeterministic is the core
// regression case: a name whose sanitized form overflows isolationNameBudget
// must still come back no longer than isolationNameBudget, and must derive
// the identical segment on every call (isolationTenants relies on this to
// produce the same pair of tenant ids for a given *testing.T no matter how
// many times a caller might ask).
func TestBoundedTenantIDSegment_LongNameIsBoundedAndDeterministic(t *testing.T) {
	name := "TestAssertIsolated_" + strings.Repeat("VeryDescriptive", 6) + "/postgres"
	if len(name) <= isolationNameBudget {
		t.Fatalf("test fixture error: name %q (len %d) does not actually overflow isolationNameBudget (%d); make it longer", name, len(name), isolationNameBudget)
	}

	first := boundedTenantIDSegment(name)
	if len(first) > isolationNameBudget {
		t.Errorf("boundedTenantIDSegment(%q) = %q (len %d), want length <= isolationNameBudget (%d)", name, first, len(first), isolationNameBudget)
	}

	second := boundedTenantIDSegment(name)
	if second != first {
		t.Errorf("boundedTenantIDSegment(%q) is not deterministic: got %q then %q", name, first, second)
	}
}

// TestBoundedTenantIDSegment_LongNamesSharingAPrefixDoNotCollide proves the
// fix hashes the FULL original name rather than merely truncating it: two
// long names that agree on everything up to, and well past,
// isolationNameBudget characters -- so a naive truncate-only fix would have
// produced the identical, colliding segment for both -- still derive
// different segments, because the hash folds in the tail where they
// actually differ.
func TestBoundedTenantIDSegment_LongNamesSharingAPrefixDoNotCollide(t *testing.T) {
	commonPrefix := strings.Repeat("A", isolationNameBudget+10)
	name1 := commonPrefix + "-CaseOne"
	name2 := commonPrefix + "-CaseTwo"
	if len(commonPrefix) <= isolationNameBudget {
		t.Fatalf("test fixture error: commonPrefix (len %d) must alone already overflow isolationNameBudget (%d)", len(commonPrefix), isolationNameBudget)
	}

	seg1 := boundedTenantIDSegment(name1)
	seg2 := boundedTenantIDSegment(name2)
	if seg1 == seg2 {
		t.Errorf("boundedTenantIDSegment(%q) and boundedTenantIDSegment(%q) collided on %q; a truncate-only fix would wrongly produce the same segment for both, since they agree on the first %d characters (> isolationNameBudget)", name1, name2, seg1, isolationNameBudget)
	}
}

// TestIsolationTenants_LongDescriptiveSubtestName_StaysWithinMaxTenantIDLen
// reproduces the exact originally reported failure shape end to end through
// the actual exported entry point isolationTenants feeds: a realistic,
// descriptive subtest name -- the kind backend-coding-standards §13 asks
// authors to write -- long enough that the OLD, unbounded formula
// ("tenancytest-" + sanitizeForTenantID(t.Name()) + "-a"/"-b", with no
// bound at all) would have exceeded the 64-character tenant_id column every
// fixture in this package declares. Before the fix, the two assertions
// below on len(a)/len(b) fail for exactly this t.Name(); after the fix,
// they pass, because the overflowing tail is now replaced with a short
// hash instead of being embedded verbatim.
func TestIsolationTenants_LongDescriptiveSubtestName_StaysWithinMaxTenantIDLen(t *testing.T) {
	// Deliberately mirrors a real, plausible descriptive subtest name, not
	// a synthetic worst case: this is close to the exact subtest name whose
	// derived id independent testing measured at 84 characters against the
	// 64-character column every fixture in this package uses.
	t.Run("RepositoryBuiltFromSystemContextElevatedDB_WithoutAnyAdditionalScoping/postgres", func(t *testing.T) {
		rawLen := len(isolationTenantPrefix) + len(sanitizeForTenantID(t.Name())) + isolationTenantSuffixLen
		if rawLen <= maxTenantIDLen {
			t.Fatalf("test fixture error: t.Name() %q (unbounded derived length %d) does not actually overflow maxTenantIDLen (%d); the old, unbounded formula must overflow here for this test to mean anything", t.Name(), rawLen, maxTenantIDLen)
		}

		a, b := isolationTenants(t)
		if len(a) > maxTenantIDLen {
			t.Errorf("isolationTenants(%q) tenant A id = %q (len %d), want length <= maxTenantIDLen (%d); the unbounded formula would have produced length %d here", t.Name(), a, len(a), maxTenantIDLen, rawLen)
		}
		if len(b) > maxTenantIDLen {
			t.Errorf("isolationTenants(%q) tenant B id = %q (len %d), want length <= maxTenantIDLen (%d); the unbounded formula would have produced length %d here", t.Name(), b, len(b), maxTenantIDLen, rawLen)
		}
		if a == b {
			t.Errorf("isolationTenants(%q) returned identical ids for both tenants: %q", t.Name(), a)
		}
	})
}

// TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects is the
// full end-to-end confirmation: AssertIsolated itself, run under a long,
// descriptive subtest name against a real dbkit.Repository[sprocket], must
// succeed.
//
// SQLite only here -- see TestAssertIsolated_Sprocket's doc comment
// (assert_isolated_test.go) for why a plain _test.go file must not reach
// testutil.Dialects()'s postgres entry. PostgreSQL is if anything the more
// important dialect for this particular regression -- it is the one that
// actually enforces VARCHAR(64) and is what surfaced this bug in the first
// place -- so its leg is not skipped, merely relocated: it runs from
// tenancytest/integration_test/postgres_assert_isolated_tenant_id_length_test.go,
// behind //go:build integration. (The name kept saying "WorksAcrossDialects"
// across that split only because renaming it added unrelated churn; the two
// dialects' coverage together is exactly what it always was.)
func TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects(t *testing.T) {
	t.Run("ADeliberatelyLongAndDescriptiveSubtestNameLikeTheProjectConventionAsksFor", func(t *testing.T) {
		repo := newSprocketRepo(t, testutil.Dialects()[0].NewDB(t)) // sqlite: see doc comment above
		AssertIsolated(t, repo, newSprocket)
	})
}
