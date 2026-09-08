//go:build integration

package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vislake/speed/go/dbkit/audit"
)

// TestRepository_Insert_Postgres_CutsOverWideDisplayNameToColumnBound is
// the PostgreSQL leg of the write-boundary enforcement repository.go's
// Insert applies to audit_events' descriptive columns (see
// fitEventToColumns' doc comment in repository.go and model.go's
// column-bounds constants for the full account). It is the fail-before /
// pass-after regression for the enforcement itself: an audit event whose
// resource_display_name carries a legal value longer
// than the target column's width -- the concrete case is an integration
// webhook URL (url VARCHAR(2048), go/integration/webhook_model.go) fed
// verbatim into resource_display_name (VARCHAR(255)) by
// go/integration/webhook_service.go's emitWebhookAudit -- is stored
// verbatim by SQLite (whose VARCHAR bounds are unenforced) and REFUSED by
// a real PostgreSQL server with SQLSTATE 22001 ("value too long for type
// character varying(255)"), failing the whole INSERT. On the real
// PostgreSQL side of this file the refusal is a genuine 22001, so this
// test FAILS on a codebase that relies on the database to truncate and
// PASSES on one whose Insert cuts the field in Go first -- which is the
// point of a dual-dialect regression -- while the SQLite-only unit tier
// pins the same Go-side cut from the other direction (repository_test.go's
// TestRepository_Insert_CutsOverWideDescriptiveFieldsToTheirColumnBounds,
// which cannot see a database-level refusal because SQLite never
// enforces one).
func TestRepository_Insert_Postgres_CutsOverWideDisplayNameToColumnBound(t *testing.T) {
	ctx := context.Background()
	db := newMigratedPostgresAuditDB(t)
	repo := audit.NewRepository(db)

	// A legal webhook URL: 300 characters, comfortably inside the
	// source column's own VARCHAR(2048) budget and far past the audit
	// column's VARCHAR(255).
	longURL := "https://hooks.example.com/integration/wh-" + strings.Repeat("a", 300)
	if utf8.RuneCountInString(longURL) <= 255 {
		t.Fatalf("premise broken: test URL is only %d runes, want > 255", utf8.RuneCountInString(longURL))
	}

	evt := sampleEvent()
	evt.SetResource(audit.Resource{Type: "integration.webhook_subscription", ID: "wh-1", DisplayName: longURL})
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert(over-wide resource_display_name) error = %v, want nil -- before the fix a real PostgreSQL server refuses this with 22001 and the whole record is lost", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() = nil, want the inserted event")
	}
	runes := []rune(longURL)
	if got.ResourceDisplayName != string(runes[:255]) {
		t.Errorf("stored resource_display_name is %d runes of the original, want the original cut at 255 runes (stored: %q)",
			utf8.RuneCountInString(got.ResourceDisplayName), got.ResourceDisplayName)
	}
}

// TestRepository_Insert_Postgres_RefusesOverWideIdentifierField pins the
// identifier/vocabulary half of the same enforcement against a real
// PostgreSQL server: an over-wide resource_id is refused with
// ErrEventFieldTooLong BEFORE the write, so the outcome is identical to
// the SQLite one -- no row, a named error -- instead of a PostgreSQL-only
// 22001 arriving after a statement was already attempted. A refused
// insert must leave no row behind.
func TestRepository_Insert_Postgres_RefusesOverWideIdentifierField(t *testing.T) {
	ctx := context.Background()
	db := newMigratedPostgresAuditDB(t)
	repo := audit.NewRepository(db)

	evt := sampleEvent()
	evt.SetResource(audit.Resource{Type: "note", ID: strings.Repeat("n", 300), DisplayName: "x"})
	err := repo.Insert(ctx, evt)
	if err == nil {
		t.Fatal("Insert(over-wide resource_id) error = nil, want ErrEventFieldTooLong")
	}
	if !errors.Is(err, audit.ErrEventFieldTooLong) {
		t.Fatalf("Insert(over-wide resource_id) error = %v, want it to wrap ErrEventFieldTooLong", err)
	}
	rows, listErr := repo.ListByTenant(ctx, "tenant-a")
	if listErr != nil {
		t.Fatalf("ListByTenant() error = %v", listErr)
	}
	if len(rows) != 0 {
		t.Errorf("ListByTenant() returned %d rows after the refusal, want 0", len(rows))
	}
}
