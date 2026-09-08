package audit

import (
	"context"
	"testing"
)

// TestAppendOnlyTrigger_SQLite_RejectsRawUpdateAndDelete is the SQLite leg
// of the database-level append-only proof: migrations/sqlite/0002_append_
// only_enforcement.sql's two BEFORE UPDATE/DELETE triggers must refuse a
// raw statement issued directly against audit_events, entirely bypassing
// Repository (which never had an Update or Delete method to bypass in the
// first place -- this test proves the guarantee holds even for a caller
// that skips Repository, and even Go application code, altogether: a raw
// *sql.DB obtained from the same *gorm.DB openAuditTestDB already applied
// both migration files to).
//
// The PostgreSQL leg of the identical proof lives in
// integration_test/postgres_append_only_test.go, since a real PostgreSQL
// server needs Docker via testcontainers -- this file needs none, matching
// the rest of this package's SQLite-first unit tier.
func TestAppendOnlyTrigger_SQLite_RejectsRawUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil -- INSERT must be completely unaffected by the append-only triggers", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}

	t.Run("RawUpdate_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `UPDATE audit_events SET action = ? WHERE id = ?`, "tampered.action", evt.ID)
		if err == nil {
			t.Fatal("raw UPDATE against audit_events succeeded, want the append-only trigger to reject it")
		}

		// The row itself must be completely unchanged -- not merely that
		// the statement returned an error, but that nothing was mutated
		// before the trigger aborted it.
		got, getErr := repo.Get(ctx, evt.ID)
		if getErr != nil {
			t.Fatalf("Get() after the rejected UPDATE error = %v", getErr)
		}
		if got == nil || got.Action != evt.Action {
			t.Fatalf("Get() after the rejected UPDATE = %+v, want the row unchanged (Action = %q)", got, evt.Action)
		}
	})

	t.Run("RawDelete_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `DELETE FROM audit_events WHERE id = ?`, evt.ID)
		if err == nil {
			t.Fatal("raw DELETE against audit_events succeeded, want the append-only trigger to reject it")
		}

		got, getErr := repo.Get(ctx, evt.ID)
		if getErr != nil {
			t.Fatalf("Get() after the rejected DELETE error = %v", getErr)
		}
		if got == nil {
			t.Fatal("row is gone after the rejected DELETE, want it still physically present")
		}
	})
}

// TestAppendOnlyTrigger_SQLite_RejectsInsertOrReplace proves the append-only
// triggers hold even against SQLite's legacy "INSERT OR REPLACE" upsert
// form, not only against a plain UPDATE/DELETE.
//
// SQLite only fires a table's DELETE triggers for the implicit
// conflict-row removal INSERT OR REPLACE performs when the connection's
// PRAGMA recursive_triggers is ON; it defaults OFF, so without the pragma
// an "INSERT OR REPLACE" against an existing id silently overwrites the
// row's columns with no error and no trigger firing at all -- reachable
// only via raw SQL that bypasses Repository (which has no Update or
// Delete method to begin with), but exactly the threat class the
// append-only triggers exist to stop. go/dbkit/dialect/sqlite's
// withRecursiveTriggers folds "_pragma=recursive_triggers(1)" into every
// SQLite DSN dbkit.Open opens; on a connection without it this test fails
// (the row silently becomes "tampered.action" with no error), on
// dbkit.Open's connections it passes.
func TestAppendOnlyTrigger_SQLite_RejectsInsertOrReplace(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}

	_, err = sqlDB.ExecContext(ctx,
		`INSERT OR REPLACE INTO audit_events (`+
			`id, actor_type, actor_id, actor_display_name, `+
			`on_behalf_of_type, on_behalf_of_id, on_behalf_of_display_name, `+
			`action, resource_type, resource_id, resource_display_name, `+
			`success, failure_reason, changes, tenant_id, ip, user_agent, trace_id, occurred_at`+
			`) SELECT `+
			`id, actor_type, actor_id, actor_display_name, `+
			`on_behalf_of_type, on_behalf_of_id, on_behalf_of_display_name, `+
			`'tampered.action', resource_type, resource_id, resource_display_name, `+
			`success, failure_reason, changes, tenant_id, ip, user_agent, trace_id, occurred_at `+
			`FROM audit_events WHERE id = ?`,
		evt.ID)
	if err == nil {
		t.Fatal("raw INSERT OR REPLACE against audit_events succeeded, want the append-only DELETE trigger to reject its implicit conflict-row removal")
	}

	got, getErr := repo.Get(ctx, evt.ID)
	if getErr != nil {
		t.Fatalf("Get() after the rejected INSERT OR REPLACE error = %v", getErr)
	}
	if got == nil || got.Action != evt.Action {
		t.Fatalf("Get() after the rejected INSERT OR REPLACE = %+v, want the row unchanged (Action = %q)", got, evt.Action)
	}
}

// TestAppendOnlyTrigger_SQLite_InsertStillWorks re-proves, as its own
// explicitly named test (rather than folding this into the test above
// alone), that a fresh Insert after the triggers are installed -- not just
// the one Insert the rejection test above happens to perform first -- is
// unaffected: the append-only enforcement must never make the table
// write-only-once or otherwise interfere with the one write path
// Repository is actually meant to support.
func TestAppendOnlyTrigger_SQLite_InsertStillWorks(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	for i := 0; i < 3; i++ {
		evt := sampleEvent()
		if err := repo.Insert(ctx, evt); err != nil {
			t.Fatalf("Insert() #%d error = %v, want nil", i, err)
		}
		got, err := repo.Get(ctx, evt.ID)
		if err != nil {
			t.Fatalf("Get() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Get() #%d = nil, want the just-inserted row back", i)
		}
	}
}
