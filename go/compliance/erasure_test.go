package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// newErasureServiceWith returns an ErasureService wired directly over a
// hand-built pkgcore.ComponentRegistry carrying exactly the given participants,
// plus a subscriber capturing every audit.RecordedEvent published on the
// bus (dbkit/audit.Emit's own EventRecorded), so a test can assert an
// erasure is actually audited without a real database-backed
// audit.Repository. Participants are whatever a test needs -- the shared
// testutil.FakeNote participant (newErasureHarness) or a deliberately
// custom set (the refusal and attribution tests below).
func newErasureServiceWith(t *testing.T, participants ...pkgcore.RetentionParticipant) (*ErasureService, *[]audit.RecordedEvent) {
	t.Helper()
	return newErasureServiceOn(t, pkgcore.NewMemoryEventBus(), participants...)
}

// newErasureServiceOn is newErasureServiceWith over an injected bus: the
// service is wired exactly the same way, but bus is the one the test
// provides, so a test can substitute a scripted bus whose Publish fails
// and drive the fail-closed branches (the system-context audit publish,
// the erasure's own audit emit) that a healthy memory bus can never reach.
func newErasureServiceOn(t *testing.T, bus pkgcore.EventBus, participants ...pkgcore.RetentionParticipant) (*ErasureService, *[]audit.RecordedEvent) {
	t.Helper()
	reg := componenttest.NewRegistry()
	reg.Put(bus)
	if err := reg.AuditActions.Add(AuditActionErasureRequest); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRightToErasure)

	captured := &[]audit.RecordedEvent{}
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			*captured = append(*captured, rec)
		}
		return nil
	})

	if err := reg.Retention.Add(participants...); err != nil {
		t.Fatalf("register participants: %v", err)
	}

	svc := newErasureService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	return svc, captured
}

// newErasureHarness returns an ErasureService wired directly over a
// hand-built pkgcore.ComponentRegistry (see newErasureServiceWith) plus one
// registered testutil.FakeNote participant and its own migrated SQLite
// database, ready for seeding rows directly.
func newErasureHarness(t *testing.T) (*ErasureService, *testutil.FakeRepository, *[]audit.RecordedEvent) {
	t.Helper()
	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	svc, captured := newErasureServiceWith(t, participant)
	return svc, repo, captured
}

// seedLiveFakeNote inserts one FakeNote for tenant, live (never soft-
// deleted) -- an erasure request bypasses the retention window entirely,
// so it must reach a live row too, unlike a sweep.
func seedLiveFakeNote(t *testing.T, repo *testutil.FakeRepository, tenant pkgcore.TenantID, id, subjectID string) {
	t.Helper()
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	note := testutil.FakeNote{ID: id, TenantID: string(tenant), SubjectID: subjectID, Content: "secret"}
	if err := repo.Create(ctx, &note); err != nil {
		t.Fatalf("seed FakeNote %q: %v", id, err)
	}
}

var testErasureActor = pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Test Admin"}

// TestErasureService_Erase_ErasesLiveRowsBypassingRetention proves the
// defining property of a right-to-erasure request: it reaches a live row,
// never soft-deleted, that a retention sweep would never have touched.
func TestErasureService_Erase_ErasesLiveRowsBypassingRetention(t *testing.T) {
	svc, repo, _ := newErasureHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Erase(pkgcore.WithTenant(context.Background(), tenant), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if result.TotalErased() != 1 {
		t.Fatalf("TotalErased() = %d, want 1", result.TotalErased())
	}
	if fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("note-1 should have been hard-deleted by the erasure request")
	}
}

// TestErasureService_Erase_CrossTenantNonErasure is a MANDATORY proof:
// erasing subject X in tenant A must never touch a same-named or
// same-shaped row in tenant B.
func TestErasureService_Erase_CrossTenantNonErasure(t *testing.T) {
	svc, repo, _ := newErasureHarness(t)
	subjectID := "subject-shared"
	seedLiveFakeNote(t, repo, "tenant-a", "note-a", subjectID)
	seedLiveFakeNote(t, repo, "tenant-b", "note-b", subjectID)

	result, err := svc.Erase(pkgcore.WithTenant(context.Background(), "tenant-a"), pkgcore.SubjectRef{TenantID: "tenant-a", SubjectID: subjectID}, testErasureActor)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if result.TotalErased() != 1 {
		t.Fatalf("TotalErased() = %d, want 1", result.TotalErased())
	}
	if fakeNoteExists(t, repo, "tenant-a", "note-a") {
		t.Error("tenant A's row for the erased subject should be gone")
	}
	if !fakeNoteExists(t, repo, "tenant-b", "note-b") {
		t.Error("tenant B's row for the SAME subject id must survive an erasure scoped to tenant A -- cross-tenant erasure is exactly what this test guards against")
	}
}

// TestErasureService_Erase_IsAudited is a MANDATORY proof: a successful
// erasure request publishes exactly one AuditActionErasureRequest event,
// naming the subject and carrying the per-participant breakdown.
func TestErasureService_Erase_IsAudited(t *testing.T) {
	svc, repo, captured := newErasureHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	if _, err := svc.Erase(pkgcore.WithTenant(context.Background(), tenant), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want exactly 1", len(events))
	}
	evt := events[0]
	if evt.Action != AuditActionErasureRequest {
		t.Errorf("Action = %q, want %q", evt.Action, AuditActionErasureRequest)
	}
	if evt.Resource.Type != "compliance.subject" || evt.Resource.ID != "subject-1" {
		t.Errorf("Resource = %+v, want type=compliance.subject id=subject-1", evt.Resource)
	}
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true for a clean erasure")
	}
	if evt.Actor.ID != testErasureActor.ID {
		t.Errorf("Actor.ID = %q, want %q -- the requester must be attributed", evt.Actor.ID, testErasureActor.ID)
	}
}

// TestErasureService_Erase_EmptySubjectRefIsRefused pins the input
// validation.
func TestErasureService_Erase_EmptySubjectRefIsRefused(t *testing.T) {
	svc, _, _ := newErasureHarness(t)
	tests := []pkgcore.SubjectRef{
		{TenantID: "", SubjectID: "s"},
		{TenantID: "t", SubjectID: ""},
		{},
	}
	for _, subject := range tests {
		_, err := svc.Erase(context.Background(), subject, testErasureActor)
		if !apperr.HasCode(err, ErrEmptySubjectRef.Code) {
			t.Errorf("Erase(%+v) error = %v, want %s", subject, err, ErrEmptySubjectRef.Code)
		}
	}
}

// TestErasureService_Erase_ParticipantErrorIsPartialFailureAndRetryConverges
// proves the documented partial-failure and retry-converges behavior: a
// failing participant does not stop the healthy one, is reported as
// ErrErasurePartialFailure, and a second Erase call for the same subject
// finishes the job without re-erasing (or re-auditing as a duplicate) what
// already succeeded.
func TestErasureService_Erase_ParticipantErrorIsPartialFailureAndRetryConverges(t *testing.T) {
	svc, repo, captured := newErasureHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	attempt := 0
	flaky := pkgcore.RetentionParticipant{
		// NoopSweep satisfies the registrar's mandatory-Sweep rule; the
		// erasure service under test never invokes it.
		Name:  "testutil.flaky",
		Sweep: testutil.NoopSweep,
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) {
			attempt++
			if attempt == 1 {
				return 0, errFakeParticipant
			}
			return 0, nil // nothing of its own to erase, but healthy on retry
		},
	}
	if err := svc.retention.Add(flaky); err != nil {
		t.Fatalf("register flaky participant: %v", err)
	}

	subject := pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}
	ctx := pkgcore.WithTenant(context.Background(), tenant)

	first, err := svc.Erase(ctx, subject, testErasureActor)
	if !apperr.HasCode(err, ErrErasurePartialFailure.Code) {
		t.Fatalf("first Erase error = %v, want %s", err, ErrErasurePartialFailure.Code)
	}
	if first.Erased["testutil.fake_note"] != 1 {
		t.Errorf("first attempt: healthy participant erased %d, want 1", first.Erased["testutil.fake_note"])
	}
	if fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("the healthy participant should have erased note-1 on the first attempt")
	}

	second, err := svc.Erase(ctx, subject, testErasureActor)
	if err != nil {
		t.Fatalf("second Erase: %v", err)
	}
	if second.Erased["testutil.fake_note"] != 0 {
		t.Errorf("second attempt: healthy participant erased %d, want 0 -- nothing left", second.Erased["testutil.fake_note"])
	}
	if second.HasErrors() {
		t.Errorf("second attempt errors = %v, want none -- the flaky participant recovered", second.Errors)
	}

	if len(*captured) != 2 {
		t.Errorf("captured audit events = %d, want 2 -- one per Erase call, no dedup and no missing record", len(*captured))
	}
}

// erasureRecorder returns a RetentionParticipant whose Erase callback
// merely records that it was called (and the Actor it was called under)
// without touching any table -- the deterministic fake that proves an
// Erase gate refused the request before any participant ran.
func erasureRecorder(calls *int, observed *[]pkgcore.Actor) pkgcore.RetentionParticipant {
	return pkgcore.RetentionParticipant{
		// NoopSweep satisfies the registrar's mandatory-Sweep rule; the
		// erasure service under test never invokes it.
		Name:  "testutil.recorder",
		Sweep: testutil.NoopSweep,
		Erase: func(ctx context.Context, _ pkgcore.SubjectRef) (int, error) {
			*calls++
			if observed != nil {
				if a, ok := pkgcore.ActorFromContext(ctx); ok {
					*observed = append(*observed, a)
				}
			}
			return 0, nil
		},
	}
}

// TestErasureService_Erase_NoTenantContext_Refused pins the fail-closed
// tenant gate: Erase is irreversible, so a ctx carrying no tenant must be
// refused with pkgcore.ErrNoTenant before any participant runs -- a bare
// context must never become a license to erase rows in a tenant the
// SubjectRef merely names. A gate that tolerated a tenantless ctx would
// let this call hard-delete tenant-a's note from a background context.
func TestErasureService_Erase_NoTenantContext_Refused(t *testing.T) {
	svc, repo, captured := newErasureHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	_, err := svc.Erase(context.Background(), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("Erase error = %v, want pkgcore.ErrNoTenant", err)
	}
	if !fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("note-1 must survive a refused erasure")
	}
	if len(*captured) != 0 {
		t.Errorf("captured audit events = %d, want 0 -- a refused erasure is never audited as a request", len(*captured))
	}
}

// TestErasureService_Erase_TenantMismatch_RefusedBeforeAnyParticipant is
// the regression test for the erasure tenant gate: Erase must refuse a
// SubjectRef whose tenant differs from the ctx tenant, before any
// participant's Erase callback runs. Erase is the one operation in this
// module that is irreversible, so a caller-supplied tenant that is absent,
// empty or different from the ctx tenant is refused with the coded error
// (ErrErasureTenantMismatch) -- never a license to hard-delete another
// tenant's rows with a compliant audit record to show for it. Without the
// gate, an unconditional re-scope to the subject's tenant would erase
// tenant-b's rows while the caller's ctx says tenant-a.
func TestErasureService_Erase_TenantMismatch_RefusedBeforeAnyParticipant(t *testing.T) {
	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	seedLiveFakeNote(t, repo, "tenant-b", "note-b", "subject-shared")

	calls := 0
	svc, captured := newErasureServiceWith(t, erasureRecorder(&calls, nil))

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	_, err := svc.Erase(ctx, pkgcore.SubjectRef{TenantID: "tenant-b", SubjectID: "subject-shared"}, testErasureActor)
	if !apperr.HasCode(err, ErrErasureTenantMismatch.Code) {
		t.Fatalf("Erase error = %v, want %s", err, ErrErasureTenantMismatch.Code)
	}
	if calls != 0 {
		t.Errorf("participant Erase calls = %d, want 0 -- the mismatch must be refused before any participant runs", calls)
	}
	if !fakeNoteExists(t, repo, "tenant-b", "note-b") {
		t.Error("tenant-b's row must survive an erasure refused for a tenant-a ctx -- cross-tenant destruction is exactly what this gate prevents")
	}
	if len(*captured) != 0 {
		t.Errorf("captured audit events = %d, want 0 -- a refused erasure is never audited as a request", len(*captured))
	}
}

// TestErasureService_Erase_EmptyActorFallsBackToSystemActor pins the
// attribution fallback: a requestedBy Actor with an empty ID (the zero
// Actor included) must be replaced by the fallback system actor BEFORE
// the Actor is placed on ctx -- not only inside the system reason -- so
// the audit row and every participant capture carry the fallback id.
// Placing the empty-ID Actor on ctx with the fallback reserved for
// SystemReason.Actor alone would leave audit rows and participant
// captures attributed to an Actor with an empty id.
func TestErasureService_Erase_EmptyActorFallsBackToSystemActor(t *testing.T) {
	tenant := pkgcore.TenantID("tenant-a")
	calls := 0
	var observed []pkgcore.Actor
	svc, captured := newErasureServiceWith(t, erasureRecorder(&calls, &observed))

	_, err := svc.Erase(pkgcore.WithTenant(context.Background(), tenant), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, pkgcore.Actor{})
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if calls != 1 {
		t.Fatalf("participant Erase calls = %d, want 1", calls)
	}
	if len(observed) != 1 {
		t.Fatalf("participant-observed actors = %d, want 1", len(observed))
	}
	if observed[0].ID != erasureFallbackActorID || observed[0].Type != pkgcore.ActorTypeSystem {
		t.Errorf("participant observed Actor = %+v, want type=system id=%q -- the fallback must reach the Actor on ctx", observed[0], erasureFallbackActorID)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	if events[0].Actor.ID != erasureFallbackActorID || events[0].Actor.Type != pkgcore.ActorTypeSystem {
		t.Errorf("audit event Actor = %+v, want type=system id=%q", events[0].Actor, erasureFallbackActorID)
	}
}

// TestErasureService_Erase_ParticipantPartialCountSurvivesError pins the
// count semantics of a partial failure: a participant whose Erase callback
// failed part-way through has already hard-deleted the rows it reports
// erasing, so that count must survive into ErasureResult.Erased --
// TotalErased and the audit event's Changes["erased"] breakdown count rows
// that are genuinely gone even when the callback also errored, never
// silently dropping them from the record of an irreversible operation. A
// count recorded only on success would let a participant reporting (2,
// err) contribute 0 to TotalErased and vanish from the audit trail's
// erased map entirely.
func TestErasureService_Erase_ParticipantPartialCountSurvivesError(t *testing.T) {
	partial := pkgcore.RetentionParticipant{
		// NoopSweep satisfies the registrar's mandatory-Sweep rule; the
		// erasure service under test never invokes it.
		Name:  "testutil.partial",
		Sweep: testutil.NoopSweep,
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) {
			return 2, errFakeParticipant
		},
	}
	svc, captured := newErasureServiceWith(t, partial)
	tenant := pkgcore.TenantID("tenant-a")

	result, err := svc.Erase(pkgcore.WithTenant(context.Background(), tenant), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if !apperr.HasCode(err, ErrErasurePartialFailure.Code) {
		t.Fatalf("Erase error = %v, want %s", err, ErrErasurePartialFailure.Code)
	}
	if got := result.Erased["testutil.partial"]; got != 2 {
		t.Errorf("Erased[testutil.partial] = %d, want 2 -- the rows the failing participant reported erasing before its error must still be counted", got)
	}
	if result.Errors["testutil.partial"] == nil {
		t.Error("Errors[testutil.partial] is missing -- the failure must still be reported alongside the count")
	}
	if got := result.TotalErased(); got != 2 {
		t.Errorf("TotalErased() = %d, want 2", got)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	changes := events[0].Changes
	if changes == nil {
		t.Fatal("audit event Changes = nil, want the erased/errors breakdown")
	}
	erased, ok := changes.After["erased"].(map[string]int)
	if !ok {
		t.Fatalf("Changes.After[\"erased\"] = %T, want map[string]int", changes.After["erased"])
	}
	if got := erased["testutil.partial"]; got != 2 {
		t.Errorf("audit Changes erased[testutil.partial] = %d, want 2 -- the erased breakdown must count rows actually hard-deleted even alongside the error", got)
	}
	if events[0].Result.Success {
		t.Error("audit Result.Success = true, want false -- the participant error must still mark the request failed")
	}
}

// TestErasureService_Erase_ChangesRecordClassificationNeverErrorText pins
// the audit-trail content rule for the erasure path: a participant whose
// Erase callback failed must appear in the audit event's Changes as a
// classification (the participant's name, marked failed) -- never with the
// callback's error text verbatim. An erasure-path error can carry the
// erased subject's own identifier (a repository error naming the rows it
// could not delete, a storage error quoting the request), and the changes
// column is the one place on the audit table from which nothing can ever
// be removed: carving the erased subject's identifier into the very table
// erasure must not touch would defeat the erasure's whole purpose. The
// error text's home is the structured log (behind the redaction layer) and
// the returned ErasureResult.Errors -- never the permanent audit record.
func TestErasureService_Erase_ChangesRecordClassificationNeverErrorText(t *testing.T) {
	// The subject id appears in the failing participant's error text --
	// the carving shape this test guards against.
	carving := errors.New("erasing subject-1 rows failed: connection refused")
	partial := pkgcore.RetentionParticipant{
		// NoopSweep satisfies the registrar's mandatory-Sweep rule; the
		// erasure service under test never invokes it.
		Name:  "testutil.carving",
		Sweep: testutil.NoopSweep,
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) {
			return 0, carving
		},
	}
	svc, captured := newErasureServiceWith(t, partial)
	tenant := pkgcore.TenantID("tenant-a")

	_, err := svc.Erase(pkgcore.WithTenant(context.Background(), tenant), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if !apperr.HasCode(err, ErrErasurePartialFailure.Code) {
		t.Fatalf("Erase error = %v, want %s", err, ErrErasurePartialFailure.Code)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	changes := events[0].Changes
	if changes == nil {
		t.Fatal("audit event Changes = nil, want the erased/errors breakdown")
	}
	errs, ok := changes.After["errors"].(map[string]string)
	if !ok {
		t.Fatalf("Changes.After[\"errors\"] = %T, want map[string]string -- the classification map, never the raw errors", changes.After["errors"])
	}
	if got := errs["testutil.carving"]; got != erasureAuditErrorMarker {
		t.Errorf("Changes errors[testutil.carving] = %q, want the classification marker %q", got, erasureAuditErrorMarker)
	}
	raw, marshalErr := json.Marshal(changes)
	if marshalErr != nil {
		t.Fatalf("json.Marshal(Changes) error = %v", marshalErr)
	}
	if strings.Contains(string(raw), carving.Error()) {
		t.Errorf("the participant error text %q was carved into the audit trail Changes: %s", carving.Error(), raw)
	}
	if strings.Contains(string(raw), "subject-1") {
		t.Errorf("the erased subject's identifier leaked into the audit trail Changes through the participant error: %s", raw)
	}
}

// TestErasureService_Erase_SystemContextAuditPublishFailure_FailsClosed
// proves the erasure path carries the same fail-closed grant SweepTenant
// does: when the system-context audit record itself cannot be published,
// Erase refuses before any participant runs -- an elevated erasure with no
// audit trail would be an irreversible, unattributed deletion, exactly the
// gap the audited wrapper exists to close.
func TestErasureService_Erase_SystemContextAuditPublishFailure_FailsClosed(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 1, failErr: errors.New("scripted bus refuses")}
	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	svc, _ := newErasureServiceOn(t, bus, participant)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	result, err := svc.Erase(ctx, pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if !apperr.HasCode(err, tenancy.ErrAuditPublishFailed.Code) {
		t.Fatalf("Erase error = %v, want %s", err, tenancy.ErrAuditPublishFailed.Code)
	}
	if result.Subject != (pkgcore.SubjectRef{}) || result.Erased != nil || result.Errors != nil {
		t.Errorf("Erase result = %+v, want the zero result -- no participant ran", result)
	}
	if !fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("the note must survive: the erasure was refused before any participant ran")
	}
}

// TestErasureService_Erase_AuditRecordFailure_SurfacesWithResult proves
// the audit-record failure contract of the erasure path: the erasure
// itself ran (rows are genuinely and irreversibly gone) but the one audit
// event recording it could not be published, so Erase returns the
// completed ErasureResult alongside ErrAuditRecordFailed -- the operator
// is told the erasure happened AND that its record is missing, never one
// at the expense of the other.
func TestErasureService_Erase_AuditRecordFailure_SurfacesWithResult(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 2, failErr: errors.New("scripted bus refuses")}
	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	svc, _ := newErasureServiceOn(t, bus, participant)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	result, err := svc.Erase(ctx, pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if !apperr.HasCode(err, ErrAuditRecordFailed.Code) {
		t.Fatalf("Erase error = %v, want %s", err, ErrAuditRecordFailed.Code)
	}
	if result.Erased["testutil.fake_note"] != 1 {
		t.Errorf("Erased = %v, want the participant's 1 erased row -- the erasure ran before its audit publish failed", result.Erased)
	}
	if fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("note-1 should have been erased: the participant ran before the audit publish failed")
	}
}
