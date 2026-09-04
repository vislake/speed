package compliance

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// newErasureHarness returns an ErasureService wired directly over a
// hand-built pkgcore.Registry, plus a subscriber capturing every
// audit.RecordedEvent published on the bus (dbkit/audit.Emit's own
// EventRecorded), so a test can assert an erasure is actually audited
// without a real database-backed audit.Repository.
func newErasureHarness(t *testing.T) (*ErasureService, *testutil.FakeRepository, *[]audit.RecordedEvent) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
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

	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	if err := reg.Retention.Add(participant); err != nil {
		t.Fatalf("register fake participant: %v", err)
	}

	svc := newErasureService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
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

	result, err := svc.Erase(context.Background(), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
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

	result, err := svc.Erase(context.Background(), pkgcore.SubjectRef{TenantID: "tenant-a", SubjectID: subjectID}, testErasureActor)
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

	if _, err := svc.Erase(context.Background(), pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor); err != nil {
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
		if !hasCode(err, ErrEmptySubjectRef.Code) {
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
		Name: "testutil.flaky",
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

	first, err := svc.Erase(context.Background(), subject, testErasureActor)
	if !hasCode(err, ErrErasurePartialFailure.Code) {
		t.Fatalf("first Erase error = %v, want %s", err, ErrErasurePartialFailure.Code)
	}
	if first.Erased["testutil.fake_note"] != 1 {
		t.Errorf("first attempt: healthy participant erased %d, want 1", first.Erased["testutil.fake_note"])
	}
	if fakeNoteExists(t, repo, tenant, "note-1") {
		t.Error("the healthy participant should have erased note-1 on the first attempt")
	}

	second, err := svc.Erase(context.Background(), subject, testErasureActor)
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
