package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"gorm.io/gorm"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is the reference app's end-to-end proof that its notes module
// is a real consumer of go/compliance's retention mechanism (see
// internal/notes/retention_participant.go's own doc comment for what that
// discharges): notes' pkgcore.RetentionParticipant is registered on the
// kernel's Retention registrar by BuildServer, so the compliance module's
// three orchestrations reach real notes rows through the module it was
// built over.
//
// The suite deliberately drives the three services through the wired
// *compliance.Module BuildServer returns (its fourth result -- the only
// reach a test has into them, per BuildServer's own doc comment), never
// through the jobs handler that enqueues the sweep: SweepTenant runs
// synchronously, which is what lets a test create notes, age one past the
// retention window, and assert on the sweep's own result in one process.
//
// Notes are created through the real composed HTTP stack, exactly as the
// other flow tests in this package do. What no HTTP surface can do is
// soft-delete a note (notes exposes no delete endpoint yet), age a
// soft-deleted note's deleted_at past the sweep's 30-day default window
// (see go/compliance/retention.go's defaultRetentionWindow), or tell a
// physically deleted row from a merely soft-deleted one (the Repository's
// own reads are live-only by design). All three go through a second
// dbkit.Open connection to the same SQLite file -- the identical reach
// TestBuildServer_NoteCreate_PersistsAuditEvent already uses -- driving
// the very dbkit.Repository[Note].Delete (the mark-delete this participant
// is the compliance consumer OF) and, through dbkit.WithTenantSession so
// the tenant-scope plugin injects the tenant half of every WHERE clause,
// two test-only statements the product deliberately has no API for:
// backdating deleted_at and counting physical rows. Writing them against
// the test's own second connection, never the server's, is what keeps this
// suite out of the server's live connection pool.

// complianceOtherCreatorUserID is the second creator id these tests send as
// the X-Demo-User-Id header (DemoOrgUserHeader). DemoNotesSubjectResolver
// accepts any non-empty value there, and DemoNotesCreatorUserID is only the
// conventional one; naming a second value lets the erasure test prove that
// an Erase of one creator-subject leaves another creator-subject's notes
// untouched -- the cross-subject half of the non-erasability property,
// which needs two genuinely different creator ids on the same tenant.
const complianceOtherCreatorUserID = "user-creator-2"

// TestComplianceRetentionSweep_NotesParticipant_ReapsExpiredOnly is the
// sweep leg: a real soft-deleted acme note whose deleted_at was backdated
// 45 days (past the 30-day default retention window every sweep uses when
// no config service is wired -- see go/compliance/retention.go) is
// physically reaped by one RetentionService.SweepTenant call, while a live
// note of the same tenant and an equally-aged soft-deleted note of a
// different tenant both survive it -- the per-tenant sweep boundary proven
// over real notes rows, matching the erasure test's own cross-tenant
// non-erasability proof below.
func TestComplianceRetentionSweep_NotesParticipant_ReapsExpiredOnly(t *testing.T) {
	srv, cfg, complianceModule := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "compliance-sweep-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "compliance-sweep-globex")

	acmeExpiredID := createNoteAs(t, srv, acmeToken, "acme note past its retention window")
	acmeLiveID := createNoteAs(t, srv, acmeToken, "acme note still inside its retention window")
	globexExpiredID := createNoteAs(t, srv, globexToken, "globex note past ITS retention window")

	db := openSecondDB(t, cfg)
	past := time.Now().Add(-45 * 24 * time.Hour)
	softDeleteAndBackdate(t, db, "tenant-acme", acmeExpiredID, past)
	softDeleteAndBackdate(t, db, "tenant-globex", globexExpiredID, past)

	// SweepTenant accepts a bare context: it sets its own system Actor,
	// tenant and audited system context before any participant runs.
	result, err := complianceModule.Retention().SweepTenant(context.Background(), "tenant-acme")
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("SweepTenant errors = %v, want none", result.Errors)
	}
	if got := result.Reaped["notes.note"]; got != 1 {
		t.Fatalf("SweepTenant reaped notes.note = %d, want exactly 1", got)
	}

	// The reaped note is physically gone, not merely hidden...
	if got := physicalNoteCount(t, db, "tenant-acme", acmeExpiredID); got != 0 {
		t.Fatalf("expired acme note still physically present (%d row(s)) after the sweep", got)
	}
	// ...a live note of the swept tenant is untouched...
	if got := physicalNoteCount(t, db, "tenant-acme", acmeLiveID); got != 1 {
		t.Fatalf("live acme note physical rows = %d, want 1", got)
	}
	// ...and an equally-expired note of another tenant is: a sweep of
	// tenant-acme never crosses into tenant-globex's rows.
	if got := physicalNoteCount(t, db, "tenant-globex", globexExpiredID); got != 1 {
		t.Fatalf("expired globex note physical rows = %d, want 1 (sweep must not cross tenants)", got)
	}
}

// TestComplianceRightToErasure_NotesParticipant_ErasesOnlyItsSubject is the
// erasure leg: one Erase of the creator-subject DemoNotesCreatorUserID in
// tenant-acme physically removes both that creator's live note and that
// creator's soft-deleted note (an erasure bypasses the retention window --
// the soft-deleted note here is brand-new, far younger than the sweep's
// 30-day cutoff), while a second creator-subject's note in the same tenant
// and every note of the other tenant survive -- the cross-subject and
// cross-tenant non-erasability halves re-proven over real notes rows.
// Re-running the same Erase then converges with (0, nil) instead of
// failing, the documented recovery path for a partial erasure.
func TestComplianceRightToErasure_NotesParticipant_ErasesOnlyItsSubject(t *testing.T) {
	srv, cfg, complianceModule := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "compliance-erase-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "compliance-erase-globex")

	creatorLiveID := createNoteAs(t, srv, acmeToken, "creator-one acme note, live")
	creatorDeletedID := createNoteAs(t, srv, acmeToken, "creator-one acme note, soft-deleted minutes ago")
	otherCreatorID := createNoteAsByCreator(t, srv, acmeToken, complianceOtherCreatorUserID, "creator-two acme note")
	globexID := createNoteAs(t, srv, globexToken, "creator-one globex note")

	db := openSecondDB(t, cfg)
	softDeleteAndBackdate(t, db, "tenant-acme", creatorDeletedID, time.Now())

	subject := pkgcore.SubjectRef{TenantID: "tenant-acme", SubjectID: app.DemoNotesCreatorUserID}
	// The ctx must carry the subject's own tenant: Erase erases within
	// the tenant ctx is scoped to -- the SubjectRef may only echo it back
	// (go/compliance's Erase tenant gate) -- and enters its audited system
	// context itself.
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	result, err := complianceModule.Erasure().Erase(ctx, subject, pkgcore.Actor{
		Type:        pkgcore.ActorTypeSystem,
		ID:          "compliance-flow-test",
		DisplayName: "Compliance flow test",
	})
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("Erase errors = %v, want none", result.Errors)
	}
	if got := result.Erased["notes.note"]; got != 2 {
		t.Fatalf("Erase erased notes.note = %d, want exactly 2 (the live and the soft-deleted creator-one notes)", got)
	}

	// The subject's own notes are physically gone -- the soft-deleted one
	// included, which no retention sweep would have touched for weeks...
	if got := physicalNoteCount(t, db, "tenant-acme", creatorLiveID); got != 0 {
		t.Fatalf("creator-one live note still physically present (%d row(s)) after Erase", got)
	}
	if got := physicalNoteCount(t, db, "tenant-acme", creatorDeletedID); got != 0 {
		t.Fatalf("creator-one soft-deleted note still physically present (%d row(s)) after Erase", got)
	}
	// ...another creator-subject's note in the same tenant survives...
	if got := physicalNoteCount(t, db, "tenant-acme", otherCreatorID); got != 1 {
		t.Fatalf("creator-two note physical rows = %d, want 1 (Erase must not cross subjects)", got)
	}
	// ...and the same creator id in another tenant survives too.
	if got := physicalNoteCount(t, db, "tenant-globex", globexID); got != 1 {
		t.Fatalf("globex note physical rows = %d, want 1 (Erase must not cross tenants)", got)
	}

	// A re-run of the same request converges to (0, nil): the participant
	// reports no rows for an already-fully-erased subject, the documented
	// retry semantics of Erase.
	again, err := complianceModule.Erasure().Erase(ctx, subject, pkgcore.Actor{
		Type:        pkgcore.ActorTypeSystem,
		ID:          "compliance-flow-test",
		DisplayName: "Compliance flow test",
	})
	if err != nil {
		t.Fatalf("second Erase: %v", err)
	}
	if again.HasErrors() {
		t.Fatalf("second Erase errors = %v, want none", again.Errors)
	}
	if got := again.Erased["notes.note"]; got != 0 {
		t.Fatalf("second Erase erased notes.note = %d, want 0 (an already-erased subject converges)", got)
	}
}

// TestComplianceExport_NotesParticipant_ExportsLiveNotesOnly is the
// data-portability leg: one Export of tenant-acme gathers the tenant's
// live notes -- including the creator attribution a subject needs to pick
// their own data out of the manifest -- and excludes the note this test
// soft-deleted minutes ago (an export carries what the tenant can see,
// never rows a soft-delete already hid, since Export runs with no system
// context). The manifest is stored and delivered through the real wiring:
// ObjectKey is non-empty (a go/sharing-backed delivery is minted against
// it), and the Delivery carries a share id, a one-time token and an
// expiry, proving the sharing-backed delivery wiring holds end to end.
func TestComplianceExport_NotesParticipant_ExportsLiveNotesOnly(t *testing.T) {
	srv, cfg, complianceModule := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "compliance-export-acme")

	liveID := createNoteAs(t, srv, acmeToken, "live note content a subject would export")
	deletedID := createNoteAs(t, srv, acmeToken, "soft-deleted note content, invisible to an export")

	db := openSecondDB(t, cfg)
	softDeleteAndBackdate(t, db, "tenant-acme", deletedID, time.Now())

	// Export reads every participant's rows through the tenant ctx carries
	// and refuses a ctx with no tenant at all (pkgcore.ErrNoTenant), so the
	// caller must rebuild the tenant ctx itself -- exactly what a real
	// job-handler-style caller does from its stored tenant id.
	result, err := complianceModule.Export().Export(
		pkgcore.WithTenant(context.Background(), "tenant-acme"), "tenant-acme")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Manifest.HasErrors() {
		t.Fatalf("Export manifest errors = %v, want none", result.Manifest.Errors)
	}
	if result.ObjectKey == "" {
		t.Fatal("Export ObjectKey is empty, want a stored-manifest key")
	}
	if result.Delivery.ShareID == "" || result.Delivery.Token == "" || result.Delivery.ExpiresAt.IsZero() {
		t.Fatalf("Export Delivery = %+v, want a minted share id, token and expiry", result.Delivery)
	}

	data, ok := result.Manifest.Participants["notes.note"]
	if !ok {
		t.Fatalf("Export manifest participants = %v, want a notes.note entry", result.Manifest.Participants)
	}
	exported, ok := data.([]notes.Note)
	if !ok {
		t.Fatalf("manifest notes.note value has type %T, want []notes.Note", data)
	}
	if len(exported) != 1 {
		t.Fatalf("exported notes = %+v, want exactly the one live note", exported)
	}
	got := exported[0]
	if got.ID != liveID || got.Text != "live note content a subject would export" {
		t.Fatalf("exported note = %+v, want the live note (id %s)", got, liveID)
	}
	if got.CreatorUserID != app.DemoNotesCreatorUserID {
		t.Fatalf("exported note CreatorUserID = %q, want %q", got.CreatorUserID, app.DemoNotesCreatorUserID)
	}
}

// openSecondDB opens a second dbkit.Open connection to the same SQLite
// file the server under test writes (cfg.SQLitePath) and registers its
// close with the test -- the reach TestBuildServer_NoteCreate_PersistsAuditEvent
// uses, which BuildServer needs neither its *gorm.DB nor any module's
// service for a test to inspect its storage. The second connection is
// deliberately not migrated: the server's own boot already applied every
// migration, and this handle only reads and performs test-only writes.
func openSecondDB(t *testing.T, cfg app.ServerConfig) *gorm.DB {
	t.Helper()

	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})
	return db
}

// softDeleteAndBackdate performs a real soft delete of the note with the
// given id -- through notes' own dbkit.Repository[Note].Delete over the
// second connection, the very mark-delete path this suite's participant is
// the compliance consumer of, with the tenant-scope plugin filtering the
// row -- then overwrites its deleted_at with backdatedAt. The overwrite is
// test-only plumbing: no product API sets deleted_at to an arbitrary past
// time, and the retention sweep's own cutoff comparison is what a test
// must reach to prove it (the sweep reaps rows whose deleted_at is at or
// before its cutoff, so aging a row 45 days back puts it past the 30-day
// default window; leaving it at time.Now() keeps it young). The UPDATE
// runs inside dbkit.WithTenantSession so the plugin injects the tenant
// half of its WHERE clause, exactly as the product's own tenant-session
// queries do; the id WHERE clause is the test's own.
func softDeleteAndBackdate(t *testing.T, db *gorm.DB, tenant pkgcore.TenantID, noteID string, backdatedAt time.Time) {
	t.Helper()

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	repo := notes.NewRepository(db)
	if err := repo.Delete(ctx, noteID); err != nil {
		t.Fatalf("soft-delete note %s in %s: %v", noteID, tenant, err)
	}
	err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Model(&notes.Note{}).Where("id = ?", noteID).Update("deleted_at", &backdatedAt).Error
	})
	if err != nil {
		t.Fatalf("backdate note %s in %s to %v: %v", noteID, tenant, backdatedAt, err)
	}
}

// physicalNoteCount counts the notes rows with the given id for tenant,
// soft-deleted or not -- an Unscoped count, since the live-only reads the
// Repository exposes cannot distinguish a soft-deleted row from a
// physically gone one, which is exactly the distinction every assertion in
// this suite is about. The tenant half of the WHERE clause is injected by
// the tenant-scope plugin inside dbkit.WithTenantSession.
func physicalNoteCount(t *testing.T, db *gorm.DB, tenant pkgcore.TenantID, noteID string) int64 {
	t.Helper()

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	var count int64
	err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Unscoped().Model(&notes.Note{}).Where("id = ?", noteID).Count(&count).Error
	})
	if err != nil {
		t.Fatalf("count note %s in %s: %v", noteID, tenant, err)
	}
	return count
}

// createNoteAsByCreator is createNoteAs with the note's creator made
// explicit: it POSTs a note with the given text, authenticated as token,
// carrying X-Demo-User-Id (DemoOrgUserHeader) equal to creatorID instead
// of the DemoNotesCreatorUserID default -- the header DemoNotesSubjectResolver
// reads to attribute a creator (internal/app/server.go), which is what lets an erasure
// test target one real creator-subject while another creator's notes stay
// untouched. The acting user for the rbac gate is DemoOwnerUserID exactly
// as in createNoteAs.
func createNoteAsByCreator(t *testing.T, srv *httptest.Server, token, creatorID, text string) string {
	t.Helper()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/notes", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(app.DemoUserHeader, app.DemoOwnerUserID)
	req.Header.Set(app.DemoOrgUserHeader, creatorID)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/notes: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/notes status = %d, want %d; body = %s",
			resp.StatusCode, http.StatusCreated, respBody)
	}

	var created testNote
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create-note response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create-note response carried no id")
	}
	return created.ID
}
