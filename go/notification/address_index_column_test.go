package notification

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// addressIndexColumnTests are the two models that carry the blind-index
// address column AddressIndexColumn names. Host indexers index a contact's
// address into verified_contacts; platform_blacklist rows store the same
// shape of index under the same column name (blacklist.go), and a change to
// either half of the shared name is exactly the drift this file exists to
// catch.
//
// Why this pin at all: dbkit.NewBlindIndexer's contract makes the column
// argument the blind-index column's exact SQL name, and it refuses an EMPTY
// name but has no guard for a non-empty wrong one -- the reference app's
// original wiring (hand-typed "contact_email_index"/"contact_phone_index"
// for a column that is really address_index) is the failure that guard gap
// allowed. Nothing in this module's behaviour can fail on such a wrong name
// while Equal is never called (the contact service only ever uses Index),
// so the name is pinned here instead, from the two sources that together
// define it: the models' own `column:` gorm tags (the authority dbkit's
// doc names) and the schema the module's real migration files produce. The
// constant is the only hand-written copy left.
//
// A regression on the OLD strings cannot fail these tests (they never knew
// the old strings); the tests fail when AddressIndexColumn itself drifts
// from either model's gorm tag or from the migrated column -- the drift
// that would otherwise re-create the dormant wrong-column indexer.
func TestAddressIndexColumn_MatchesBothModelsGormTags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		field string
	}{
		{name: "VerifiedContact", model: VerifiedContact{}, field: "AddressIndex"},
		{name: "PlatformBlacklist", model: PlatformBlacklist{}, field: "AddressIndex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelColumnName(t, tc.model, tc.field); got != AddressIndexColumn {
				t.Fatalf("%s.%s gorm `column:` tag = %q, want AddressIndexColumn (%q): a host indexer built over the constant would blind-index the wrong column", tc.name, tc.field, got, AddressIndexColumn)
			}
		})
	}
}

// TestAddressIndexColumn_IsTheMigratedColumnOfBothTables applies the
// module's real migration files from zero (newTestDB, repository_test.go)
// and checks the migrated schema actually carries a column named
// AddressIndexColumn in both tables -- the direct pin of the constant
// against the real migration output, with no chain through the gorm tags.
func TestAddressIndexColumn_IsTheMigratedColumnOfBothTables(t *testing.T) {
	// ColumnTypes parses the model structurally, and VerifiedContact.Address
	// carries the encrypted-address serializer -- registered here the way
	// every test that touches a contact model does (registerContactSerializer,
	// contact_test.go), before the migrator can resolve the field.
	registerContactSerializer()
	db := newTestDB(t)

	for _, tc := range []struct {
		model any
		table string
	}{
		{model: &VerifiedContact{}, table: tableVerifiedContacts},
		{model: &PlatformBlacklist{}, table: tablePlatformBlacklist},
	} {
		t.Run(tc.table, func(t *testing.T) {
			columnTypes, err := db.Migrator().ColumnTypes(tc.model)
			if err != nil {
				t.Fatalf("ColumnTypes(%T): %v", tc.model, err)
			}
			columns := make([]string, 0, len(columnTypes))
			for _, ct := range columnTypes {
				columns = append(columns, ct.Name())
			}
			if !slices.Contains(columns, AddressIndexColumn) {
				t.Fatalf("migrated %s table has no column %q; actual columns: %v", tc.table, AddressIndexColumn, columns)
			}
		})
	}
}

// modelColumnName returns the value of one model field's `column:` gorm
// tag -- the authority dbkit.NewBlindIndexer's doc names for its column
// argument ("the value of the model field's `column:` gorm tag").
func modelColumnName(t *testing.T, model any, field string) string {
	t.Helper()
	structField, ok := reflect.TypeOf(model).FieldByName(field)
	if !ok {
		t.Fatalf("model %T has no field %s", model, field)
	}
	tag := structField.Tag.Get("gorm")
	for _, part := range strings.Split(tag, ";") {
		if name, found := strings.CutPrefix(part, "column:"); found {
			return name
		}
	}
	t.Fatalf("model %T field %s has no column: entry in its gorm tag (%q)", model, field, tag)
	return ""
}
