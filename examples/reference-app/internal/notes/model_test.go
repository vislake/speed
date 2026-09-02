package notes

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestNote_GetTenantID_ReturnsEmbeddedTenantModelValue is a no-database
// sanity check that Note's promoted GetTenantID method (from embedding
// dbkit.TenantModel) returns the value held by that embedded TenantModel
// -- confirming the embedding itself is wired the way model.go's doc
// comment on Note says it is.
//
// It does NOT guard against the shadowed-TenantID-field footgun dbkit's
// own tenant_scope.go doc comment on TenantModel warns about (repeated on
// model.go's Note doc comment), despite an earlier version of this
// comment claiming it did. The struct literal below sets TenantModel's
// embedded TenantID directly, through a keyed "TenantModel:
// dbkit.TenantModel{TenantID: ...}" literal, so it never goes through
// GORM's schema/scan machinery -- exactly where that regression
// manifests, since GORM's schema parser resolves a same-named field
// redeclared on Note (the shadow) in place of the one promoted from
// TenantModel, per ordinary Go field-selector rules. Confirmed
// empirically: adding such a shadowing field to Note leaves this test
// passing, since GetTenantID and this test's literal still agree on
// TenantModel's own copy of the field -- the mismatch against what GORM
// actually populates on a real row never enters the picture.
//
// The regression is instead caught by TestRepository_AssertIsolated
// (repository_test.go), which drives Create and FindByID through a real
// migrated SQLite database via dbkit.Repository[Note] -- the path where
// GORM's field resolution actually runs -- and, more generally, by
// dbkit's own
// TestTenantModel_ShadowingPromotedFieldToAddPrimaryKey_BreaksFindByIDForTheOwningTenant
// (go/dbkit/tenant_scope_tenantmodel_test.go). Both fail under the
// shadowing regression; this test does not, and is not meant to.
func TestNote_GetTenantID_ReturnsEmbeddedTenantModelValue(t *testing.T) {
	n := Note{
		ID:          "note-1",
		TenantModel: dbkit.TenantModel{TenantID: "tenant-acme"},
		Text:        "hello",
	}

	if got, want := n.GetTenantID(), pkgcore.TenantID("tenant-acme"); got != want {
		t.Fatalf("GetTenantID() = %q, want %q", got, want)
	}
}

// TestNote_ImplementsTenantScoped is a runtime-checkable companion to
// model.go's compile-time `var _ dbkit.TenantScoped = Note{}` assertion --
// redundant with it today, but unlike that line, a test failure here shows
// up in `go test`'s own output instead of only a build error, which is
// easier for a future reader to spot in CI.
func TestNote_ImplementsTenantScoped(t *testing.T) {
	var _ dbkit.TenantScoped = Note{}
}
