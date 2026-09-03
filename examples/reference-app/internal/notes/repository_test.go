package notes

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newMigratedRepository returns a Repository backed by a fresh, per-test
// SQLite database already migrated with this module's own real
// Migrations() -- not a hand-written schema shortcut -- so a test failure
// here can never be explained away as "the test fixture's schema diverged
// from the real migration files".
func newMigratedRepository(t *testing.T) *Repository {
	t.Helper()
	repo, _ := newMigratedRepositoryWithDB(t)
	return repo
}

// newMigratedRepositoryWithDB is newMigratedRepository, additionally
// returning the underlying *gorm.DB it built Repository on top of, for a
// test that needs a raw, plugin-bypassing check of what actually landed in
// the database (Repository itself, package-private outside dbkit, exposes
// no such accessor).
func newMigratedRepositoryWithDB(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()

	db := dbtest.NewSQLite(t)

	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(NewModule(db)); err != nil {
		t.Fatalf("register notes module for migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply notes migrations: %v", err)
	}

	return NewRepository(db), db
}

// TestRepository_AssertIsolated runs the mandatory tenant-isolation suite
// (root CLAUDE.md's multi-tenant isolation rule; backend coding standard
// §3.3/§13; go/dbkit/AGENTS.md's "Known limitations" section, which named
// this exact obligation before tenancytest existed to fulfill it) against
// notes' real dbkit.Repository[Note] usage -- this is that suite's first
// real caller anywhere in the repository. Note is tenant data, not
// identity or platform data (docs/internal/04-data-and-tenancy.md's
// data-domain table), so AssertIsolated is the correct half of the
// AssertIsolated/AssertNotTenantScoped pair, never both.
func TestRepository_AssertIsolated(t *testing.T) {
	repo := newMigratedRepository(t)

	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Note {
		return &Note{
			ID:          uuid.NewString(),
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			Text:        "isolation test note",
		}
	})
}

// TestRepository_DeleteThenRestoreThenDelete_HiddenFromNormalQueriesThroughoutLifecycle
// is dbkit's mark-delete round's reference-app consumer proof
// (docs/internal/04-data-and-tenancy.md's delete-semantics section): Note implements
// dbkit.SoftDeletable (model.go), and this drives the full lifecycle
// straight through notes' real, migrated Repository -- promoted unchanged
// from dbkit.Repository[Note], with no new code in repository.go itself --
// exactly as a real caller would use it: create, see it in normal queries,
// delete (mark-delete), confirm it is hidden from FindByID/List but the raw
// row is still present, restore, confirm it reappears, delete again,
// confirm it is hidden again.
//
// This is a service-level proof, not an HTTP one: notes exposes no
// delete/restore endpoint in its OpenAPI fragment yet (a good, named
// follow-up scope -- see this package's model.go doc comment on
// DeletedAt/DeletedBy), so this test exercises Repository directly rather
// than going through handler.go.
func TestRepository_DeleteThenRestoreThenDelete_HiddenFromNormalQueriesThroughoutLifecycle(t *testing.T) {
	repo, db := newMigratedRepositoryWithDB(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	note := &Note{ID: uuid.NewString(), Text: "lifecycle note"}
	if err := repo.Create(ctx, note); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if list, err := repo.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("List() after Create = (%+v, %v), want exactly one note", list, err)
	}

	if err := repo.Delete(ctx, note.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := repo.FindByID(ctx, note.ID); err == nil {
		t.Error("FindByID() after Delete() succeeded, want the note hidden")
	}
	if list, err := repo.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List() after Delete() = (%+v, %v), want empty", list, err)
	}

	// The raw row itself must still be present (mark-delete, not physical
	// delete) -- confirmed through the real underlying *gorm.DB, bypassing
	// the soft-delete auto-scope plugin the same way a raw SELECT would.
	var rawCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM notes WHERE id = ?`, note.ID).Scan(&rawCount).Error; err != nil {
		t.Fatalf("raw notes count after Delete(): %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("raw notes count after Delete() = %d, want 1 (mark-delete must leave the row physically present)", rawCount)
	}

	if err := repo.Restore(ctx, note.ID); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got, err := repo.FindByID(ctx, note.ID); err != nil || got.ID != note.ID {
		t.Fatalf("FindByID() after Restore() = (%+v, %v), want the note visible again", got, err)
	}
	if list, err := repo.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("List() after Restore() = (%+v, %v), want exactly one note", list, err)
	}

	if err := repo.Delete(ctx, note.ID); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}
	if _, err := repo.FindByID(ctx, note.ID); err == nil {
		t.Error("FindByID() after the second Delete() succeeded, want the note hidden again")
	}
}
