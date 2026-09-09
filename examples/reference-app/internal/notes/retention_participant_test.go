package notes

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// retentionTestTenant and retentionTestCreator are the fixed tenant and
// creator the retention-participant tests write under. Fixed values, not
// derived from test names: each test runs against a fresh per-test
// database whose only rows are the ones it writes (the same reasoning
// store_test.go's isoTenant constants document for the attestation
// package's own suite).
const (
	retentionTestTenant  = "retention-test-tenant"
	retentionTestCreator = "retention-test-creator"
)

// newRetentionRepo returns a Repository over a freshly migrated SQLite
// database (the same fixture repository_test.go's own tests use) plus the
// system-context-carrying tenant context compliance's orchestrators
// supply every participant callback with: HardDelete reads the system
// grant and the tenant both from ctx (retention_participant.go's own doc
// comment).
func newRetentionRepo(t *testing.T) *Repository {
	t.Helper()
	return newMigratedRepository(t)
}

// newNote builds a note of retentionTestCreator under ctx's tenant.
func newNote(ctx context.Context) *Note {
	return &Note{ID: uuid.NewString(), Text: "retention participant note", CreatorUserID: retentionTestCreator}
}

// retentionCtx returns a system-context-carrying copy of the
// retentionTestTenant context, exactly the ctx shape compliance's
// orchestrators hand every participant callback (see
// hardDeleteSystemCtx in repository_test.go for the fixture-purpose
// convention).
func retentionCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(retentionTestTenant))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: retentionTestCreator})
	return hardDeleteSystemCtx(t, ctx)
}

// backdateDeletion rewrites a soft-deleted row's deleted_at to the given
// time -- the fixture lever the sweep tests need, since a real Delete
// stamps "now" and the retention cutoff is a past moment. A raw UPDATE
// through the underlying *gorm.DB: raw statements are the one family the
// tenant and soft-delete plugins cannot intercept, the same convention
// repository_test.go's raw COUNT checks follow (fixture plumbing only,
// never a production write path).
func backdateDeletion(t *testing.T, repo *Repository, noteID string, deletedAt time.Time) {
	t.Helper()
	if err := repo.db.WithContext(context.Background()).
		Exec("UPDATE notes SET deleted_at = ? WHERE id = ?", deletedAt, noteID).Error; err != nil {
		t.Fatalf("backdate deletion of %q: %v", noteID, err)
	}
}

// TestRetentionParticipant_Sweep_HardDeletesSoftDeletedNotesPastTheCutoff
// drives the retention sweep callback over notes' real Repository: only
// the tenant's soft-deleted notes whose deletion happened at or before
// the cutoff are physically removed -- a live note and a more recently
// soft-deleted one survive -- the reaped count reports exactly the
// removed rows, and a re-run of the sweep over the same tenant converges
// on (0, nil) rather than failing or double-counting (the mechanism's
// documented idempotence contract).
func TestRetentionParticipant_Sweep_HardDeletesSoftDeletedNotesPastTheCutoff(t *testing.T) {
	repo := newRetentionRepo(t)
	ctx := retentionCtx(t)
	participant := NewRetentionParticipant(repo)
	if participant.Name != "notes.note" {
		t.Errorf("participant Name = %q, want %q", participant.Name, "notes.note")
	}

	live := newNote(ctx)
	if err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create(live): %v", err)
	}
	aged := newNote(ctx)
	if err := repo.Create(ctx, aged); err != nil {
		t.Fatalf("Create(aged): %v", err)
	}
	recent := newNote(ctx)
	if err := repo.Create(ctx, recent); err != nil {
		t.Fatalf("Create(recent): %v", err)
	}

	// aged and recent are soft-deleted; aged's deletion is then backdated
	// to before the cutoff the sweep runs with.
	if err := repo.Delete(ctx, aged.ID); err != nil {
		t.Fatalf("Delete(aged): %v", err)
	}
	if err := repo.Delete(ctx, recent.ID); err != nil {
		t.Fatalf("Delete(recent): %v", err)
	}
	cutoff := time.Now().Add(-time.Hour)
	backdateDeletion(t, repo, aged.ID, cutoff.Add(-24*time.Hour))

	reaped, err := participant.Sweep(ctx, pkgcore.TenantID(retentionTestTenant), cutoff)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("Sweep reaped %d rows, want 1 -- only the note deleted before the cutoff", reaped)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after Sweep: %v", err)
	}
	if len(list) != 1 || list[0].ID != live.ID {
		t.Fatalf("List after Sweep = %d notes (ids: %v), want only the live note -- the recently soft-deleted one stays hidden from ordinary reads", len(list), noteIDs(list))
	}
	// Ground truth: the recently deleted note is still physically present
	// (only its older sibling was reaped) -- the raw row count confirms
	// which row actually went.
	var rawCount int64
	if countErr := repo.db.WithContext(context.Background()).
		Raw(`SELECT COUNT(*) FROM notes`).Scan(&rawCount).Error; countErr != nil {
		t.Fatalf("raw notes count after Sweep: %v", countErr)
	}
	if rawCount != 2 {
		t.Fatalf("raw notes count after Sweep = %d, want 2 -- live + recently deleted, the aged note physically gone", rawCount)
	}

	// A re-run over the same tenant finds nothing left to reap: (0, nil),
	// never an error and never a double count.
	reaped, err = participant.Sweep(ctx, pkgcore.TenantID(retentionTestTenant), cutoff)
	if err != nil || reaped != 0 {
		t.Fatalf("second Sweep = (reaped %d, err %v), want (0, nil)", reaped, err)
	}
}

// TestRetentionParticipant_Sweep_OnlySeesTheCallingTenantsRows pins the
// sweep's tenant boundary at the participant layer: another tenant's
// soft-deleted notes past the cutoff are invisible to this tenant's
// sweep -- the list the sweep reaps from is the tenant-scoped
// listSoftDeletedBefore, never a cross-tenant scan.
func TestRetentionParticipant_Sweep_OnlySeesTheCallingTenantsRows(t *testing.T) {
	repo := newRetentionRepo(t)
	ctx := retentionCtx(t)
	participant := NewRetentionParticipant(repo)

	otherTenant := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("retention-test-other"))
	other := &Note{ID: uuid.NewString(), Text: "another tenant's aged note", CreatorUserID: "someone-else"}
	if err := repo.Create(otherTenant, other); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}
	if err := repo.Delete(otherTenant, other.ID); err != nil {
		t.Fatalf("Delete(other tenant): %v", err)
	}
	cutoff := time.Now().Add(-time.Hour)
	backdateDeletion(t, repo, other.ID, cutoff.Add(-24*time.Hour))

	reaped, err := participant.Sweep(ctx, pkgcore.TenantID(retentionTestTenant), cutoff)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("Sweep reaped %d rows, want 0 -- another tenant's aged note must be invisible", reaped)
	}
}

// TestRetentionParticipant_Erase_RemovesEveryNoteOneCreatorOwns drives
// the right-to-erasure callback: every note of the subject's creator --
// live and soft-deleted alike -- is physically removed from the calling
// tenant, the erased count reports exactly those rows, and a creator
// with no notes on file answers (0, nil) rather than an error (the
// contract that lets a re-run of an erasure already applied elsewhere
// converge).
func TestRetentionParticipant_Erase_RemovesEveryNoteOneCreatorOwns(t *testing.T) {
	repo := newRetentionRepo(t)
	ctx := retentionCtx(t)
	participant := NewRetentionParticipant(repo)
	subject := pkgcore.SubjectRef{TenantID: pkgcore.TenantID(retentionTestTenant), SubjectID: retentionTestCreator}

	live := newNote(ctx)
	if err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create(live): %v", err)
	}
	deleted := newNote(ctx)
	if err := repo.Create(ctx, deleted); err != nil {
		t.Fatalf("Create(deleted): %v", err)
	}
	if err := repo.Delete(ctx, deleted.ID); err != nil {
		t.Fatalf("Delete(deleted): %v", err)
	}

	erased, err := participant.Erase(ctx, subject)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if erased != 2 {
		t.Fatalf("Erase removed %d notes, want 2 -- the live and the soft-deleted one", erased)
	}

	var rawCount int64
	if countErr := repo.db.WithContext(context.Background()).
		Raw(`SELECT COUNT(*) FROM notes`).Scan(&rawCount).Error; countErr != nil {
		t.Fatalf("raw notes count after Erase: %v", countErr)
	}
	if rawCount != 0 {
		t.Fatalf("raw notes count after Erase = %d, want 0 -- the rows must be physically gone, not marked", rawCount)
	}

	// A creator with nothing left on file erases to (0, nil).
	erased, err = participant.Erase(ctx, subject)
	if err != nil || erased != 0 {
		t.Fatalf("second Erase = (erased %d, err %v), want (0, nil)", erased, err)
	}
}

// TestRetentionParticipant_Erase_OnlySeesTheCallingTenantsCreatorNotes
// pins the erasure's two boundaries at once: another tenant's notes by
// the same creator id are invisible to this tenant's erasure, and within
// the tenant only the named creator's notes are targeted -- another
// user's notes survive an erasure of this subject.
func TestRetentionParticipant_Erase_OnlySeesTheCallingTenantsCreatorNotes(t *testing.T) {
	repo := newRetentionRepo(t)
	ctx := retentionCtx(t)
	participant := NewRetentionParticipant(repo)
	subject := pkgcore.SubjectRef{TenantID: pkgcore.TenantID(retentionTestTenant), SubjectID: retentionTestCreator}

	// The same creator id in another tenant.
	otherTenant := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("retention-test-other"))
	other := &Note{ID: uuid.NewString(), Text: "other tenant, same creator id", CreatorUserID: retentionTestCreator}
	if err := repo.Create(otherTenant, other); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	// Another user's note inside the calling tenant.
	colleague := &Note{ID: uuid.NewString(), Text: "a colleague's note", CreatorUserID: "retention-test-colleague"}
	if err := repo.Create(ctx, colleague); err != nil {
		t.Fatalf("Create(colleague): %v", err)
	}

	erased, err := participant.Erase(ctx, subject)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if erased != 0 {
		t.Fatalf("Erase removed %d notes, want 0 -- the subject owns no note in this tenant", erased)
	}
	var rawCount int64
	if err := repo.db.WithContext(context.Background()).
		Raw(`SELECT COUNT(*) FROM notes`).Scan(&rawCount).Error; err != nil {
		t.Fatalf("raw notes count after Erase: %v", err)
	}
	if rawCount != 2 {
		t.Fatalf("raw notes count after Erase = %d, want 2 -- neither the other tenant's nor the colleague's note may be erased", rawCount)
	}
}

// TestRetentionParticipant_Export_ReturnsTheTenantsLiveNotes drives the
// data-portability callback: the export is the tenant's LIVE notes only
// -- a soft-deleted note is hidden from the manifest -- and it is
// tenant-scoped, never reaching another tenant's rows.
func TestRetentionParticipant_Export_ReturnsTheTenantsLiveNotes(t *testing.T) {
	repo := newRetentionRepo(t)
	// Export runs with no system context (the participant's own doc
	// comment): a plain tenant context is the faithful ctx shape here.
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(retentionTestTenant))
	participant := NewRetentionParticipant(repo)

	kept := newNote(ctx)
	if err := repo.Create(ctx, kept); err != nil {
		t.Fatalf("Create(kept): %v", err)
	}
	deleted := newNote(ctx)
	if err := repo.Create(ctx, deleted); err != nil {
		t.Fatalf("Create(deleted): %v", err)
	}
	if err := repo.Delete(ctx, deleted.ID); err != nil {
		t.Fatalf("Delete(deleted): %v", err)
	}

	exported, err := participant.Export(ctx, pkgcore.TenantID(retentionTestTenant))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	notes, ok := exported.([]Note)
	if !ok {
		t.Fatalf("Export returned %T, want []Note", exported)
	}
	if len(notes) != 1 || notes[0].ID != kept.ID {
		t.Fatalf("Export returned %d notes (ids: %v), want exactly the live note %q", len(notes), noteIDs(notes), kept.ID)
	}

	// Another tenant's rows stay out of this tenant's export.
	otherTenant := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("retention-test-other"))
	other := &Note{ID: uuid.NewString(), Text: "another tenant's note", CreatorUserID: retentionTestCreator}
	if createErr := repo.Create(otherTenant, other); createErr != nil {
		t.Fatalf("Create(other tenant): %v", createErr)
	}
	exported, err = participant.Export(ctx, pkgcore.TenantID(retentionTestTenant))
	if err != nil {
		t.Fatalf("Export after the other tenant's create: %v", err)
	}
	notes = exported.([]Note)
	if len(notes) != 1 || notes[0].ID != kept.ID {
		t.Fatalf("Export after the other tenant's create returned %d notes, want only this tenant's live note", len(notes))
	}
}

// noteIDs collects a manifest's note ids for assertion output.
func noteIDs(notes []Note) []string {
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.ID)
	}
	return ids
}

// TestHardDeleteSaysGone_PinsTheGoneClassification exercises the
// classification helper's full decision table directly: a nil error is
// never "gone" (it is the success answer, and reaped++ must run), an
// error carrying dbkit's record-not-found code IS "gone" (matched by
// Code, never identity -- the decorated form dbkit's HardDelete
// surfaces, the reason the helper exists at all), and any other error is
// a genuine failure, not "gone".
func TestHardDeleteSaysGone_PinsTheGoneClassification(t *testing.T) {
	if hardDeleteSaysGone(nil) {
		t.Error("hardDeleteSaysGone(nil) = true, want false -- nil is the success answer")
	}
	notFound := dbkit.ErrRecordNotFound.WithParam("id", "some-note")
	if !hardDeleteSaysGone(notFound) {
		t.Error("hardDeleteSaysGone(record-not-found, decorated) = false, want true -- the code match must survive WithParam decoration")
	}
	if hardDeleteSaysGone(context.DeadlineExceeded) {
		t.Error("hardDeleteSaysGone(unrelated error) = true, want false -- only the record-not-found code means gone")
	}
}

// TestRetentionParticipant_FailsClosedOnAnIncompleteContext pins the
// destructive callbacks' dependence on the context compliance's
// orchestrators contract to supply: without the system-context grant,
// Sweep and Erase refuse with the HardDelete gate's error -- the
// soft-deleted rows staying physically present, never silently counted
// as reaped -- and without a tenant in ctx the candidate list itself
// fails closed. A participant must never erase because its caller forgot
// to elevate.
func TestRetentionParticipant_FailsClosedOnAnIncompleteContext(t *testing.T) {
	repo := newRetentionRepo(t)
	participant := NewRetentionParticipant(repo)

	// One soft-deleted, past-cutoff note exists, so the sweep has work to
	// do the moment its context is complete enough.
	fullCtx := retentionCtx(t)
	note := newNote(fullCtx)
	if err := repo.Create(fullCtx, note); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(fullCtx, note.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	cutoff := time.Now().Add(-time.Hour)
	backdateDeletion(t, repo, note.ID, cutoff.Add(-24*time.Hour))

	tenantOnly := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(retentionTestTenant))
	if reaped, err := participant.Sweep(tenantOnly, pkgcore.TenantID(retentionTestTenant), cutoff); err == nil {
		t.Fatalf("Sweep without a system context = (reaped %d, nil), want the HardDelete gate's refusal", reaped)
	}
	subject := pkgcore.SubjectRef{TenantID: pkgcore.TenantID(retentionTestTenant), SubjectID: retentionTestCreator}
	if erased, err := participant.Erase(tenantOnly, subject); err == nil {
		t.Fatalf("Erase without a system context = (erased %d, nil), want the HardDelete gate's refusal", erased)
	}
	if _, err := participant.Sweep(context.Background(), pkgcore.TenantID(retentionTestTenant), cutoff); err == nil {
		t.Fatal("Sweep with no tenant in ctx succeeded, want the fail-closed tenant refusal")
	}
	if _, err := participant.Erase(context.Background(), subject); err == nil {
		t.Fatal("Erase with no tenant in ctx succeeded, want the fail-closed tenant refusal")
	}

	// Ground truth: the refused calls removed nothing.
	var rawCount int64
	if err := repo.db.WithContext(context.Background()).
		Raw(`SELECT COUNT(*) FROM notes`).Scan(&rawCount).Error; err != nil {
		t.Fatalf("raw notes count: %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("raw notes count after the refused sweeps/erasures = %d, want 1 -- nothing may be removed by an incomplete context", rawCount)
	}
}
