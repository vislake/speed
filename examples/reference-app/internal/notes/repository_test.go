package notes

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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

// notesHardDeletePurpose is the SystemPurpose the two hard-delete consumer
// tests grant themselves. The tests exercise the gate, not the grant's
// legitimacy: production code that actually erases rows sits at or above
// tenancy and enters the system context through tenancy.WithSystemContext's
// audited wrapper from a whitelisted module (admin, compliance, jobs, authn
// -- the erase orchestration itself being compliance-module (M4) work), so
// the raw pkgcore grant below stands in for that audited entry, on the same
// fixture-purpose convention dbkit's own hard_delete_test.go and tenancy's
// system-context tests follow one layer down.
const notesHardDeletePurpose pkgcore.SystemPurpose = "notes.test.hard_delete"

// hardDeleteSystemCtx returns a system-context copy of ctx (tenant and
// actor already set by the caller) that passes
// dbkit.Repository[Note].HardDelete's gate. RegisterSystemPurpose is
// idempotent and mutex-guarded, so the registration is a no-op from the
// second grant on. Which Actor the granted reason carries is not
// load-bearing: HardDelete never reads who holds the system context, only
// that one exists (see dbkit/hard_delete.go's doc comment on the gate's
// necessary-not-sufficient semantics).
func hardDeleteSystemCtx(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	pkgcore.RegisterSystemPurpose(notesHardDeletePurpose)
	elevated, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "notes-hard-delete-test",
		Purpose: notesHardDeletePurpose,
		Ticket:  "notes-repository-hard-delete-consumer-test",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	return elevated
}

// isHardDeleteRefused reports whether err is
// dbkit.ErrHardDeleteRequiresSystemContext, matched by Code rather than by
// identity (apperr.WithParam always derives a new *apperr.Error, so pointer
// identity is not stable across the decoration HardDelete applies).
func isHardDeleteRefused(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrHardDeleteRequiresSystemContext.Code
}

// isRecordNotFound reports whether err is dbkit.ErrRecordNotFound, matched
// by Code on the same convention isHardDeleteRefused follows.
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
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

// TestRepository_HardDelete_SoftDeletedNote_PhysicallyRemoved is the
// hard-delete half of dbkit's delete semantics'
// reference-app consumer proof (docs/internal/04-data-and-tenancy.md's
// delete-semantics section, §3; the mark-delete half is
// TestRepository_DeleteThenRestoreThenDelete_HiddenFromNormalQueriesThroughoutLifecycle
// above): dbkit.Repository[Note].HardDelete -- promoted unchanged from the
// embedded base, exactly like Delete and Restore, with no new code in this
// package's repository.go itself -- physically erases a soft-deleted note
// behind the system-context gate, service-level against notes' real,
// migrated Repository, exactly as a real caller would use it.
//
// The lifecycle: create, Delete (mark-delete -- the note is now hidden from
// ordinary reads but its row is still physically present), enter a system
// context over the tenant context, then HardDelete, which must remove the
// row outright -- proven through the raw *gorm.DB below (COUNT through
// db.Raw, which no GORM callback can hide or skip, reads 0), never merely
// mark it again. A second HardDelete then reports ErrRecordNotFound,
// proving the row is gone rather than hidden.
func TestRepository_HardDelete_SoftDeletedNote_PhysicallyRemoved(t *testing.T) {
	repo, db := newMigratedRepositoryWithDB(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	note := &Note{ID: uuid.NewString(), Text: "hard delete me"}
	if err := repo.Create(ctx, note); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.Delete(ctx, note.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := repo.FindByID(ctx, note.ID); err == nil {
		t.Error("FindByID() after Delete() succeeded, want the note hidden (mark-delete)")
	}

	// Enter the system context over the tenant context (see the
	// notesHardDeletePurpose comment for why the raw pkgcore grant stands in
	// for tenancy's audited WithSystemContext here).
	sysCtx := hardDeleteSystemCtx(t, ctx)
	if err := repo.HardDelete(sysCtx, note.ID); err != nil {
		t.Fatalf("HardDelete() error = %v", err)
	}

	// Ground truth: the physical row is gone -- read through the real
	// underlying *gorm.DB with db.Raw, which bypasses the query-only
	// soft-delete auto-scope plugin the same way it does in the lifecycle
	// test above.
	var rawCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM notes WHERE id = ?`, note.ID).Scan(&rawCount).Error; err != nil {
		t.Fatalf("raw notes count after HardDelete(): %v", err)
	}
	if rawCount != 0 {
		t.Fatalf("raw notes count after HardDelete() = %d, want 0 (the row must be physically gone, not merely marked)", rawCount)
	}

	if err := repo.HardDelete(sysCtx, note.ID); !isRecordNotFound(err) {
		t.Fatalf("second HardDelete() error = %v, want ErrRecordNotFound (the row is gone, not hidden)", err)
	}
}

// TestRepository_HardDelete_PlainTenantContext_Refused proves the
// system-context gate on notes' own Repository: without a system context,
// HardDelete refuses with dbkit's mechanism-level error (code
// dbkit.hard_delete_requires_system_context) before the database is touched
// -- the physical row must survive the refused call unchanged -- even
// though Note is SoftDeletable and its ordinary Delete is a mark-delete. A
// plain tenant context can always soft-delete; it can never erase.
func TestRepository_HardDelete_PlainTenantContext_Refused(t *testing.T) {
	repo, db := newMigratedRepositoryWithDB(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	note := &Note{ID: uuid.NewString(), Text: "keep me"}
	if err := repo.Create(ctx, note); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := repo.HardDelete(ctx, note.ID); !isHardDeleteRefused(err) {
		t.Fatalf("HardDelete() on a plain tenant-scoped context error = %v, want ErrHardDeleteRequiresSystemContext", err)
	}

	var rawCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM notes WHERE id = ?`, note.ID).Scan(&rawCount).Error; err != nil {
		t.Fatalf("raw notes count after the refused HardDelete(): %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("raw notes count after the refused HardDelete() = %d, want 1 (the gate must refuse before the database is touched)", rawCount)
	}
}
