//go:build integration

// Package audit_test carries dbkit/audit's Docker-backed PostgreSQL
// integration tier -- the same "own physically separate integration_test/
// directory, guarded by the integration build tag" shape go/dbkit's own
// integration_test package uses (see that package's postgres_hard_delete_
// rls_test.go, whose startPostgresContainer/mustExec/openRole helpers this
// file's own equivalents are modeled on), so a plain `go test ./...` run
// never starts a container.
package audit_test

import (
	"context"
	"database/sql"
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeAuditModule feeds audit's own embedded migrations to
// dbkit.MigrationRegistry -- the identical, by-now-repeated-three-times
// (dbkit/audit/repository_test.go, go/compliance/module_test.go, here)
// minimal pkgcore.Module a test needs to drive MigrationRegistry.Apply;
// only Name and Migrations are ever read here.
type fakeAuditModule struct{}

func (fakeAuditModule) Name() string                     { return "audit" }
func (fakeAuditModule) DependsOn() []string              { return nil }
func (fakeAuditModule) Migrations() embed.FS             { return migrations.FS }
func (fakeAuditModule) Locales() embed.FS                { return embed.FS{} }
func (fakeAuditModule) OpenAPISpec() []byte              { return nil }
func (fakeAuditModule) Register(*pkgcore.ComponentRegistry) error { return nil }


// newMigratedPostgresAuditDB returns a *gorm.DB (via dbtest.NewPostgres,
// which itself calls t.Skip when no Docker daemon is reachable) with
// audit's real, versioned migrations -- both 0001_create_audit_events.sql
// and 0002_append_only_enforcement.sql -- applied from zero through the
// real dbkit.MigrationRegistry, never AutoMigrate.
func newMigratedPostgresAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewPostgres(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(fakeAuditModule{}); err != nil {
		t.Fatalf("register audit migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectPostgres); err != nil {
		t.Fatalf("apply audit migrations: %v", err)
	}
	return db
}

// rawSQLDB returns db's underlying *sql.DB, for issuing a raw statement
// that bypasses gorm (and, transitively, audit.Repository) entirely.
func rawSQLDB(t *testing.T, db *gorm.DB) *sql.DB {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}
	return sqlDB
}

// sampleEvent returns a minimal, valid *audit.AuditEvent for Insert,
// mirroring dbkit/audit's own repository_test.go sampleEvent (unexported
// to that package, hence this small duplicate).
func sampleEvent() *audit.AuditEvent {
	evt := &audit.AuditEvent{Action: "notes.note.create", TenantID: "tenant-a"}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	evt.SetResource(audit.Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"})
	evt.SetResult(audit.Result{Success: true})
	return evt
}

// TestAppendOnlyTrigger_Postgres_RejectsRawUpdateAndDelete is the
// PostgreSQL leg of the database-level append-only proof -- the SQLite leg
// lives in the parent package's append_only_test.go, needing no Docker.
//
// This is the whole point of a database-level (rather than merely
// application-level) guarantee: the test opens a raw *sql.DB from the SAME
// connection dbkit.Open/gorm hands back, and issues a raw UPDATE/DELETE
// directly against audit_events -- entirely bypassing audit.Repository,
// which has no Update or Delete method to bypass in the first place.
// A real PostgreSQL server, migrated from zero with the trigger
// migration applied, must refuse both statements regardless of which role
// the connection uses (dbtest.NewPostgres connects as the single
// superuser-ish role testcontainers provisions the whole database with --
// see 0002_append_only_enforcement.sql's own doc comment for why a
// REVOKE-based scheme could not make an equivalent claim against that same
// connection model).
func TestAppendOnlyTrigger_Postgres_RejectsRawUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	db := newMigratedPostgresAuditDB(t)
	repo := audit.NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil -- INSERT must be completely unaffected by the append-only trigger", err)
	}

	sqlDB := rawSQLDB(t, db)

	t.Run("RawUpdate_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `UPDATE audit_events SET action = $1 WHERE id = $2`, "tampered.action", evt.ID)
		if err == nil {
			t.Fatal("raw UPDATE against audit_events succeeded, want the append-only trigger to reject it")
		}

		got, getErr := repo.Get(ctx, evt.ID)
		if getErr != nil {
			t.Fatalf("Get() after the rejected UPDATE error = %v", getErr)
		}
		if got == nil || got.Action != evt.Action {
			t.Fatalf("Get() after the rejected UPDATE = %+v, want the row unchanged (Action = %q)", got, evt.Action)
		}
	})

	t.Run("RawDelete_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `DELETE FROM audit_events WHERE id = $1`, evt.ID)
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

// TestAppendOnlyTrigger_Postgres_RejectsTruncate proves the migration's
// statement-level TRUNCATE trigger, added alongside the pre-existing
// row-level UPDATE/DELETE pair: PostgreSQL never fires a row-level trigger
// for TRUNCATE, so without a dedicated FOR EACH STATEMENT trigger bound to
// the TRUNCATE event, a single `TRUNCATE audit_events;` would wipe every
// row with no error and no trigger ever invoked, even with the row-level
// UPDATE/DELETE triggers fully installed.
func TestAppendOnlyTrigger_Postgres_RejectsTruncate(t *testing.T) {
	ctx := context.Background()
	db := newMigratedPostgresAuditDB(t)
	repo := audit.NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil -- INSERT must be completely unaffected by the append-only trigger", err)
	}

	sqlDB := rawSQLDB(t, db)

	_, err := sqlDB.ExecContext(ctx, `TRUNCATE audit_events`)
	if err == nil {
		t.Fatal("raw TRUNCATE against audit_events succeeded, want the append-only statement-level trigger to reject it")
	}

	got, getErr := repo.Get(ctx, evt.ID)
	if getErr != nil {
		t.Fatalf("Get() after the rejected TRUNCATE error = %v", getErr)
	}
	if got == nil {
		t.Fatal("row is gone after the rejected TRUNCATE, want it still physically present")
	}
}

// TestAppendOnlyTrigger_Postgres_InsertStillWorks proves the trigger
// migration leaves ordinary Insert traffic against a real PostgreSQL
// server completely unaffected, driving several inserts through the same
// migrated connection.
func TestAppendOnlyTrigger_Postgres_InsertStillWorks(t *testing.T) {
	ctx := context.Background()
	repo := audit.NewRepository(newMigratedPostgresAuditDB(t))

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
