package org

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Invitation is the one model that carries the blind-index column
// EmailIndexColumn names: Invitation.EmailIndex (invitation.go). Host
// indexers index an invitee's address into org_invitations, and a change to
// the name is exactly the drift this file exists to catch.
//
// Why this pin at all: dbkit.NewBlindIndexer's contract makes the column
// argument the blind-index column's exact SQL name, and it refuses an EMPTY
// name but has no guard for a non-empty wrong one -- the mis-copy this
// constant exists to make structurally impossible: the column name is a
// hand-typed, copyable literal in every indexer's construction, so a host
// wiring can silently mis-copy it into its own indexers. Nothing in this module's behaviour can
// fail on such a wrong name: its runtime consumes the indexer's computed
// Index values and Equal conditions, never the column name the indexer was
// built with, and the suite's own indexers are built over the literal too.
// The name is therefore pinned here instead, from the two sources that
// together define it: the model's own `column:` gorm tag (the authority
// dbkit's doc names) and the schema the module's real migration files
// produce. The constant is the only hand-written copy of the name left in
// host-facing org code.
//
// These tests fail when EmailIndexColumn itself drifts from the model's
// gorm tag or from the migrated column -- the drift that would otherwise
// re-create the dormant wrong-column indexer; a wrong literal a host
// duplicated on its own would not trip them.
func TestEmailIndexColumn_MatchesModelGormTag(t *testing.T) {
	if got := modelColumnName(t, Invitation{}, "EmailIndex"); got != EmailIndexColumn {
		t.Fatalf("Invitation.EmailIndex gorm `column:` tag = %q, want EmailIndexColumn (%q): a host indexer built over the constant would blind-index the wrong column", got, EmailIndexColumn)
	}
}

// TestEmailIndexColumn_IsTheMigratedColumn applies the module's real
// migration files from zero (newInvitationTestDB, invitation_test.go) and
// checks the migrated schema actually carries a column named
// EmailIndexColumn -- the direct pin of the constant against the real
// migration output, with no chain through the gorm tags.
func TestEmailIndexColumn_IsTheMigratedColumn(t *testing.T) {
	db := newInvitationTestDB(t)

	columnTypes, err := db.Migrator().ColumnTypes(&Invitation{})
	if err != nil {
		t.Fatalf("ColumnTypes(Invitation): %v", err)
	}
	columns := make([]string, 0, len(columnTypes))
	for _, ct := range columnTypes {
		columns = append(columns, ct.Name())
	}
	if !slices.Contains(columns, EmailIndexColumn) {
		t.Fatalf("migrated org_invitations table has no column %q; actual columns: %v", EmailIndexColumn, columns)
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
