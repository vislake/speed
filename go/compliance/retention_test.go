package compliance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// errFakeParticipant is the error a deliberately failing test participant
// returns, so assertions can tell it apart from any other error.
var errFakeParticipant = errors.New("testutil: fake participant refuses")

// newRetentionHarness returns a RetentionService wired directly over a
// hand-built pkgcore.Registry (Retention and AuditActions are both
// available on a Registry built with NewRegistry -- unlike ObjectStore,
// they do not require Kernel.Bootstrap, see pkgcore/registry.go) plus one
// registered testutil.FakeNote participant and its own migrated SQLite
// database, ready for seeding rows directly. Every test in this file uses
// the real defaultRetentionWindow (30 days) rather than overriding it, so
// seeded rows are backdated relative to it instead of the harness
// carrying a test-only window-override seam.
func newRetentionHarness(t *testing.T) (*RetentionService, *testutil.FakeRepository) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)

	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	if err := reg.Retention.Add(participant); err != nil {
		t.Fatalf("register fake participant: %v", err)
	}

	svc := newRetentionService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	return svc, repo
}

// wellPastDefaultWindow and withinDefaultWindow are two points in time on
// either side of defaultRetentionWindow (30 days), for seeding rows that
// are unambiguously expired or unambiguously fresh without a test needing
// a window-override seam.
func wellPastDefaultWindow() time.Time { return time.Now().Add(-40 * 24 * time.Hour) }
func withinDefaultWindow() time.Time   { return time.Now().Add(-1 * time.Hour) }

// seedFakeNote inserts one FakeNote for tenant, then soft-deletes it with
// DeletedAt backdated to deletedAt, so a sweep sees it as already past
// (or still within) its cutoff without the test needing to sleep.
func seedFakeNote(t *testing.T, repo *testutil.FakeRepository, tenant pkgcore.TenantID, id, subjectID string, deletedAt time.Time) {
	t.Helper()
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	note := testutil.FakeNote{ID: id, TenantID: string(tenant), SubjectID: subjectID, Content: "secret"}
	if err := repo.Create(ctx, &note); err != nil {
		t.Fatalf("seed FakeNote %q: %v", id, err)
	}
	if err := repo.Delete(ctx, id); err != nil {
		t.Fatalf("soft-delete FakeNote %q: %v", id, err)
	}
	if err := repo.DB().Exec(
		"UPDATE compliance_test_fake_notes SET deleted_at = ? WHERE id = ? AND tenant_id = ?",
		deletedAt, id, string(tenant),
	).Error; err != nil {
		t.Fatalf("backdate deleted_at for %q: %v", id, err)
	}
}

// fakeNoteExists reports whether a row with id still exists at all (live
// or soft-deleted) for tenant -- used to assert a HardDelete actually
// removed the row, not merely hid it from ordinary reads.
func fakeNoteExists(t *testing.T, repo *testutil.FakeRepository, tenant pkgcore.TenantID, id string) bool {
	t.Helper()
	var count int64
	if err := repo.DB().Table("compliance_test_fake_notes").Unscoped().
		Where("id = ? AND tenant_id = ?", id, string(tenant)).
		Count(&count).Error; err != nil {
		t.Fatalf("count FakeNote %q: %v", id, err)
	}
	return count > 0
}

// TestRetentionService_SweepTenant_ReapsExpiredAndSkipsFresh proves the
// cutoff itself: a soft-deleted row well past the retention window is
// hard-deleted, while one soft-deleted only moments ago survives the same
// pass.
func TestRetentionService_SweepTenant_ReapsExpiredAndSkipsFresh(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	tenant := pkgcore.TenantID("tenant-a")

	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())
	seedFakeNote(t, repo, tenant, "fresh-1", "subject-1", withinDefaultWindow())

	result, err := svc.SweepTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.TotalReaped() != 1 {
		t.Fatalf("TotalReaped() = %d, want 1", result.TotalReaped())
	}
	if fakeNoteExists(t, repo, tenant, "expired-1") {
		t.Error("expired-1 should have been hard-deleted")
	}
	if !fakeNoteExists(t, repo, tenant, "fresh-1") {
		t.Error("fresh-1 should still exist -- it has not passed the retention window yet")
	}
}

// TestRetentionService_SweepTenant_TenantIsolation is a MANDATORY proof:
// a sweep for tenant A never touches tenant B's rows, even when both have
// expired soft-deleted rows registered through the same fake participant.
func TestRetentionService_SweepTenant_TenantIsolation(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	tenantA := pkgcore.TenantID("tenant-a")
	tenantB := pkgcore.TenantID("tenant-b")

	seedFakeNote(t, repo, tenantA, "a-expired", "subject-a", wellPastDefaultWindow())
	seedFakeNote(t, repo, tenantB, "b-expired", "subject-b", wellPastDefaultWindow())

	result, err := svc.SweepTenant(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("SweepTenant(tenantA): %v", err)
	}
	if result.Tenant != tenantA {
		t.Errorf("SweepResult.Tenant = %q, want %q", result.Tenant, tenantA)
	}
	if fakeNoteExists(t, repo, tenantA, "a-expired") {
		t.Error("tenant A's expired row should have been reaped")
	}
	if !fakeNoteExists(t, repo, tenantB, "b-expired") {
		t.Error("tenant B's expired row must survive a sweep scoped to tenant A")
	}
}

// TestRetentionService_SweepTenant_Idempotent is a MANDATORY proof: running
// the same sweep twice neither errors nor double-counts -- the second pass
// finds nothing left to reap.
func TestRetentionService_SweepTenant_Idempotent(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	first, err := svc.SweepTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("first SweepTenant: %v", err)
	}
	if first.TotalReaped() != 1 {
		t.Fatalf("first pass reaped %d, want 1", first.TotalReaped())
	}

	second, err := svc.SweepTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("second SweepTenant: %v", err)
	}
	if second.TotalReaped() != 0 {
		t.Fatalf("second pass reaped %d, want 0 -- nothing left to reap", second.TotalReaped())
	}
	if second.HasErrors() {
		t.Errorf("second pass errors = %v, want none", second.Errors)
	}
}

// TestRetentionService_SweepTenant_ParticipantErrorIsPartialFailure proves
// a failing participant does not stop the pass and is reported both in
// the SweepResult and as ErrSweepPartialFailure.
func TestRetentionService_SweepTenant_ParticipantErrorIsPartialFailure(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	failing := pkgcore.RetentionParticipant{
		Name: "testutil.failing",
		Sweep: func(context.Context, pkgcore.TenantID, time.Time) (int, error) {
			return 0, errFakeParticipant
		},
	}
	if err := svc.retention.Add(failing); err != nil {
		t.Fatalf("register failing participant: %v", err)
	}

	result, err := svc.SweepTenant(context.Background(), tenant)
	if !hasCode(err, ErrSweepPartialFailure.Code) {
		t.Fatalf("SweepTenant error = %v, want %s", err, ErrSweepPartialFailure.Code)
	}
	if result.TotalReaped() != 1 {
		t.Errorf("TotalReaped() = %d, want 1 -- the healthy participant should still have run", result.TotalReaped())
	}
	if result.Errors["testutil.failing"] == nil {
		t.Errorf("Errors[%q] = nil, want errFakeParticipant", "testutil.failing")
	}
}

// TestRetentionService_SweepTenant_NoParticipants proves an empty registry
// is a clean, empty pass rather than an error.
func TestRetentionService_SweepTenant_NoParticipants(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)
	svc := newRetentionService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.TotalReaped() != 0 || result.HasErrors() {
		t.Errorf("SweepResult = %+v, want an empty clean pass", result)
	}
}

// TestRetentionService_SweepAllTenants_IteratesEveryListedTenant proves
// SweepAllTenants sweeps every tenant TenantLister returns, aggregating
// per-tenant results.
func TestRetentionService_SweepAllTenants_IteratesEveryListedTenant(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	seedFakeNote(t, repo, "tenant-a", "a-1", "s", wellPastDefaultWindow())
	seedFakeNote(t, repo, "tenant-b", "b-1", "s", wellPastDefaultWindow())
	svc.lister = testutil.FakeTenantLister{Tenants: []pkgcore.TenantID{"tenant-a", "tenant-b"}}

	results, err := svc.SweepAllTenants(context.Background())
	if err != nil {
		t.Fatalf("SweepAllTenants: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d tenants, want 2", len(results))
	}
	if results["tenant-a"].TotalReaped() != 1 || results["tenant-b"].TotalReaped() != 1 {
		t.Errorf("results = %+v, want one reaped row per tenant", results)
	}
}

// TestRetentionService_SweepAllTenants_NoListerIsAnError proves the
// documented refusal.
func TestRetentionService_SweepAllTenants_NoListerIsAnError(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	_, err := svc.SweepAllTenants(context.Background())
	if !hasCode(err, ErrTenantListerRequired.Code) {
		t.Fatalf("SweepAllTenants without a lister error = %v, want %s", err, ErrTenantListerRequired.Code)
	}
}

// TestRetentionService_EnqueueRetentionSweep_ShapesTheTask pins what the
// schedule point puts on the queue.
func TestRetentionService_EnqueueRetentionSweep_ShapesTheTask(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	queue := &recordingQueue{}
	svc.queue = queue
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("EnqueueRetentionSweep: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.Type != taskTypeRetentionSweep {
		t.Errorf("task.Type = %q, want %q", task.Type, taskTypeRetentionSweep)
	}
	if task.TenantID != "tenant-a" {
		t.Errorf("task.TenantID = %q, want %q", task.TenantID, "tenant-a")
	}
	if task.IdempotencyKey != retentionSweepIdempotencyKey("tenant-a") {
		t.Errorf("task.IdempotencyKey = %q, want %q", task.IdempotencyKey, retentionSweepIdempotencyKey("tenant-a"))
	}
}

// TestRetentionService_EnqueueRetentionSweep_NoTenantFails pins the
// no-guessing rule.
func TestRetentionService_EnqueueRetentionSweep_NoTenantFails(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	svc.queue = &recordingQueue{}
	if err := svc.EnqueueRetentionSweep(context.Background()); err == nil {
		t.Error("EnqueueRetentionSweep with no tenant in context = nil error, want one")
	}
}

// TestRetentionService_EnqueueRetentionSweep_NoQueueReturnsErrQueueRequired
// pins the no-queue answer.
func TestRetentionService_EnqueueRetentionSweep_NoQueueReturnsErrQueueRequired(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	if err := svc.EnqueueRetentionSweep(ctx); !hasCode(err, ErrQueueRequired.Code) {
		t.Errorf("EnqueueRetentionSweep with no queue error = %v, want %s", err, ErrQueueRequired.Code)
	}
}

// TestRetentionSweepHandler_RunsTheSweep proves the jobs.Handler wrapper.
func TestRetentionSweepHandler_RunsTheSweep(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	seedFakeNote(t, repo, "tenant-a", "expired-1", "s", wellPastDefaultWindow())

	h := retentionSweepHandler{svc: svc}
	job := &jobs.Job{Type: taskTypeRetentionSweep, TenantID: "tenant-a"}
	if _, err := h.Handle(context.Background(), job, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fakeNoteExists(t, repo, "tenant-a", "expired-1") {
		t.Error("expired-1 should have been reaped by the handler")
	}
}

// TestRetentionSweepHandler_RejectsAPayload pins the task-shape rule.
func TestRetentionSweepHandler_RejectsAPayload(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	h := retentionSweepHandler{svc: svc}
	job := &jobs.Job{Type: taskTypeRetentionSweep, TenantID: "tenant-a", Payload: []byte(`{"unexpected":true}`)}
	if _, err := h.Handle(context.Background(), job, nil); err == nil {
		t.Error("Handle with a non-empty payload = nil error, want one")
	}
}
