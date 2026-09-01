package dbkit

// This file is a deliberate exception to putting every tenant_scope.go test
// in tenant_scope_test.go: that file is already well past the
// 600–800-line split guideline (backend coding standard §13), so
// TenantModel's own tests — a self-contained group with no dependency on
// that file's fixtures — live here instead, keeping the "tenant_scope"
// target prefix the convention requires for a split-out file.
//
// TenantModel itself had zero behavioral test coverage before this file:
// it appeared only in its own declaration and a bare
// "var _ TenantScoped = TenantModel{}" compile-time assertion in
// tenant_scope.go. This file exercises it three ways: GetTenantID in
// isolation, embedded in a fixture and driven through Repository[T]
// (Create, FindByID, and the isolation/override guarantees those methods
// promise), and — because reading TenantModel's own doc comment and
// AGENTS.md's caveat about it raised a real question worth answering
// rather than just repeating — an empirical proof of exactly what goes
// wrong if a caller tries to "fix" TenantModel's missing primaryKey tag by
// shadowing its field instead of declaring TenantID directly.

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// TestTenantModel_GetTenantID_ReturnsTenantIDField is TenantModel's
// no-database unit test, mirroring internal/testutil's identically-shaped
// TestWidget_GetTenantID_ReturnsTenantIDField.
func TestTenantModel_GetTenantID_ReturnsTenantIDField(t *testing.T) {
	tests := []struct {
		name string
		m    TenantModel
		want pkgcore.TenantID
	}{
		{name: "non-empty tenant", m: TenantModel{TenantID: "tenant-a"}, want: pkgcore.TenantID("tenant-a")},
		{name: "empty tenant", m: TenantModel{TenantID: ""}, want: pkgcore.TenantID("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.GetTenantID(); got != tt.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// tenantModelFixture embeds TenantModel the way its doc comment intends:
// TenantModel supplies the promoted GetTenantID method and the "tenant_id"
// column; the fixture declares its own "ID" field directly, since
// Repository[T] additionally requires an exported "ID" string field by
// that exact name (repository.go's own documented convention), which no
// embeddable base can supply generically — every model's own primary key
// shape differs. ID alone is this table's primary key: see
// TestTenantModel_DoesNotProvideCompositePrimaryKey below for why that,
// not tenant_id+id, is what embedding TenantModel actually gets you.
type tenantModelFixture struct {
	TenantModel
	ID   string `gorm:"primaryKey;size:26"`
	Name string `gorm:"size:255;not null"`
}

// TableName pins tenantModelFixture's table name explicitly, matching the
// raw CREATE TABLE in createTenantModelFixtureTable independent of GORM's
// pluralization rules.
func (tenantModelFixture) TableName() string { return "tenant_model_fixtures" }

var _ TenantScoped = tenantModelFixture{}

// createTenantModelFixtureTable adds tenantModelFixture's table via a
// plain Exec, mirroring tenant_scope_test.go's createNotesTable /
// createPlatformFlagsTable for the same "no AutoMigrate even in a
// throwaway test fixture" reason. tenant_id is NOT part of the primary key
// here — see the type's own doc comment.
func createTenantModelFixtureTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE tenant_model_fixtures (
		id        VARCHAR(26)  NOT NULL PRIMARY KEY,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create tenant_model_fixtures table: %v", err)
	}
}

// TestTenantModel_EmbeddedInFixtureUsedThroughRepository_CreateAndFindByIDRoundTrip
// is TenantModel's main proof: embedded in a small fixture type and driven
// through Repository[T] — Create then FindByID, plus the cross-tenant
// denial Repository promises — exactly the "embeddable base" bar
// TenantModel's own doc comment sets. Every assertion here only passes if
// Repository's reflection-based machinery (setTenantID, and its own
// GetTenantID re-check in FindByID) correctly reaches a field PROMOTED
// from an embedded struct, not merely a directly-declared one — the one
// property none of dbkit's other fixtures (Widget, note, platformFlag)
// exercise, since none of them embed TenantModel.
func TestTenantModel_EmbeddedInFixtureUsedThroughRepository_CreateAndFindByIDRoundTrip(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	createTenantModelFixtureTable(t, db)
	repo := NewRepository[tenantModelFixture](db)

	created := &tenantModelFixture{ID: "fixture-1", Name: "gadget"}
	if err := repo.Create(ctxFor(tenantA), created); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Create must have populated the promoted TenantID field through
	// reflection (setTenantID's FieldByName("TenantID")), exactly as it
	// would a directly-declared one.
	if created.GetTenantID() != tenantA {
		t.Errorf("after Create(), GetTenantID() = %q, want %q", created.GetTenantID(), tenantA)
	}

	got, err := repo.FindByID(ctxFor(tenantA), created.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.Name != "gadget" || got.GetTenantID() != tenantA {
		t.Errorf("FindByID() = %+v (tenant %q), want Name=gadget tenant=%s", *got, got.GetTenantID(), tenantA)
	}

	// Cross-tenant lookup must still fail closed: FindByID's own
	// defense-in-depth re-check (repository.go: "if m.GetTenantID() !=
	// tenant") must correctly read the field promoted from the embedded
	// TenantModel, exactly as it would a directly-declared one, for this to
	// deny tenant B here.
	if _, err := repo.FindByID(ctxFor(tenantB), created.ID); !isRecordNotFound(err) {
		t.Errorf("FindByID() from a different tenant error = %v, want ErrRecordNotFound", err)
	}
}

// TestTenantModel_EmbeddedInFixture_CreateOverwritesCallerSuppliedTenantID
// mirrors TestTenantScopeBeforeCreate_ForcesContextTenant_OverridesStructField
// one layer up, at Repository[T] instead of the plugin: Create must force
// the promoted TenantID field to ctx's tenant regardless of what the
// caller populated the embedded TenantModel with, the same guarantee
// repository.go's own doc comment makes for a directly-declared field.
func TestTenantModel_EmbeddedInFixture_CreateOverwritesCallerSuppliedTenantID(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	createTenantModelFixtureTable(t, db)
	repo := NewRepository[tenantModelFixture](db)

	m := &tenantModelFixture{
		TenantModel: TenantModel{TenantID: "attacker-supplied-tenant"},
		ID:          "fixture-2",
		Name:        "gadget",
	}
	if err := repo.Create(ctxFor(tenantA), m); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if m.GetTenantID() != tenantA {
		t.Errorf("after Create(), GetTenantID() = %q, want %q (Create must overwrite the promoted TenantID field, not trust the caller's)", m.GetTenantID(), tenantA)
	}
}

// TestTenantModel_DoesNotProvideCompositePrimaryKey turns AGENTS.md's
// documented caveat ("do not assume TenantModel alone gives you the
// standard's recommended (tenant_id, id) composite primary key — its tag
// does not include primaryKey") into an executable, regression-proof fact
// instead of only a claim in prose: two different tenants creating a row
// with the same id must collide, because tenant_model_fixtures' only
// primary-key column is "id" — TenantModel's own tag never makes tenant_id
// part of it. Contrast with internal/testutil's
// TestNewTestSQLite_SameIDAcrossTenants_BothRowsPersist, which proves the
// opposite for Widget, which declares TenantID directly with its own
// primaryKey tag instead of embedding TenantModel — exactly the
// alternative this test's failure points a reader toward.
func TestTenantModel_DoesNotProvideCompositePrimaryKey(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	createTenantModelFixtureTable(t, db)
	repo := NewRepository[tenantModelFixture](db)

	const sharedID = "fixture-shared-id"
	if err := repo.Create(ctxFor(tenantA), &tenantModelFixture{ID: sharedID, Name: "a-gadget"}); err != nil {
		t.Fatalf("Create() for tenant A error = %v", err)
	}

	if err := repo.Create(ctxFor(tenantB), &tenantModelFixture{ID: sharedID, Name: "b-gadget"}); err == nil {
		t.Fatal("Create() for tenant B with the same id = nil error, want a duplicate-key error — if this ever starts succeeding, tenant_model_fixtures gained a composite (tenant_id, id) key it does not otherwise have, and this test (and TenantModel's own doc comment) need updating together")
	}
}

// tenantModelShadowedFixture demonstrates the exact footgun TenantModel's
// doc comment warns against: shadowing the field promoted from an embedded
// TenantModel, from the embedding struct, in an attempt to add
// "primaryKey" on top of what TenantModel's own tag lacks.
type tenantModelShadowedFixture struct {
	TenantModel
	TenantID string `gorm:"column:tenant_id;primaryKey;size:26;not null"` // shadows TenantModel's own field
	ID       string `gorm:"primaryKey;size:26"`
	Name     string `gorm:"size:255;not null"`
}

func (tenantModelShadowedFixture) TableName() string { return "tenant_model_shadowed_fixtures" }

var _ TenantScoped = tenantModelShadowedFixture{}

func createTenantModelShadowedFixtureTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE tenant_model_shadowed_fixtures (
		id        VARCHAR(26)  NOT NULL,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create tenant_model_shadowed_fixtures table: %v", err)
	}
}

// TestTenantModel_ShadowingPromotedFieldToAddPrimaryKey_BreaksFindByIDForTheOwningTenant
// is a warning shot, not an endorsement of the pattern it exercises: it
// proves, empirically, exactly what TenantModel's doc comment warns
// against, rather than leaving it as an unverified claim.
//
// A struct field declared directly on tenantModelShadowedFixture takes
// precedence over one merely promoted from an embedded type one level
// deeper (ordinary Go field-selector shadowing, confirmed here against
// this exact gorm version): reflect's FieldByName resolves "TenantID" to
// the shallow, outer field, and so does GORM's own schema parser when
// deciding what to write and scan. So Repository[T].Create's
// reflection-based setTenantID correctly forces ctx's tenant onto the
// OUTER field — the row's real "tenant_id" column ends up correct — but
// GetTenantID is a method PROMOTED from TenantModel, so its body always
// reads TenantModel's OWN embedded copy of TenantID, which GORM never
// populates once the outer field shadows it. The result: FindByID's own
// defense-in-depth re-check ("if m.GetTenantID() != tenant" in
// repository.go) always sees an empty tenant on the row it just correctly
// found by SQL, and denies it — even to the tenant that legitimately owns
// it. Shadowing does not just fail to add a primary key; it silently
// breaks TenantScoped for the very code path TenantModel exists to
// support. The correct fix is repository.go's own documented convention:
// declare TenantID directly, with no TenantModel embedding at all (as
// internal/testutil.Widget does) — see TestTenantModel_DoesNotProvideCompositePrimaryKey's
// doc comment above.
func TestTenantModel_ShadowingPromotedFieldToAddPrimaryKey_BreaksFindByIDForTheOwningTenant(t *testing.T) {
	db := testutil.NewTestSQLite(t)
	createTenantModelShadowedFixtureTable(t, db)
	repo := NewRepository[tenantModelShadowedFixture](db)

	m := &tenantModelShadowedFixture{ID: "shadow-1", Name: "gadget"}
	if err := repo.Create(ctxFor(tenantA), m); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// The outer, shadowing field is what reflection and GORM both actually
	// write to, so the row's real tenant_id column is correct — this test
	// demonstrates a GetTenantID/isolation bug, not a data-corruption one.
	if m.TenantID != string(tenantA) {
		t.Fatalf("after Create(), m.TenantID = %q, want %q — the outer field itself must round-trip correctly for this test to demonstrate anything", m.TenantID, tenantA)
	}

	if _, err := repo.FindByID(ctxFor(tenantA), m.ID); !isRecordNotFound(err) {
		t.Errorf("FindByID() by the legitimate owning tenant error = %v, want ErrRecordNotFound — if this starts succeeding, GORM's field-shadowing/population behavior changed versus what this test (and TenantModel's doc comment) assume, and both need re-checking together", err)
	}
}
