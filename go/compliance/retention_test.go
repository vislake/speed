package compliance

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"

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
	return newRetentionHarnessOn(t, pkgcore.NewMemoryEventBus())
}

// newRetentionHarnessOn is newRetentionHarness over an injected bus: the
// service is wired exactly the same way, but bus is the one the test
// provides, so a test can substitute a scripted bus whose Publish fails
// and drive the fail-closed branches (WithSystemContext's audit publish,
// the sweep's own audit emit) that a healthy memory bus can never reach.
func newRetentionHarnessOn(t *testing.T, bus pkgcore.EventBus) (*RetentionService, *testutil.FakeRepository) {
	t.Helper()
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

// scriptedBus is an EventBus that starts refusing Publish once its own
// publish count reaches failFrom: every publish at or after that one
// returns failErr without reaching the wrapped bus. It stands in for a
// broker-backed bus whose publish path genuinely fails (a closed
// connection, an append error), which the in-memory bus cannot produce --
// tenancy.WithSystemContext and dbkit/audit.Emit both fail closed on a
// Publish error, and those refusals are exactly the branches these tests
// drive. The count is publish invocations, not successful deliveries, so
// failFrom is deterministic however many subscribers an event has: a
// retention sweep publishes exactly twice (the system-context-entered
// event, then the sweep's own audit event), an erasure the same, an
// export once.
type scriptedBus struct {
	pkgcore.EventBus
	failFrom int
	failErr  error
	calls    int
}

func (b *scriptedBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.calls++
	if b.calls >= b.failFrom {
		return b.failErr
	}
	return b.EventBus.Publish(ctx, evt)
}

var _ pkgcore.EventBus = (*scriptedBus)(nil)

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
// or soft-deleted) for tenant; tests call it to assert a HardDelete
// actually removed the row, not merely hid it from ordinary reads.
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
		// NoopErase satisfies the registrar's mandatory-Erase rule; the
		// retention sweep under test never invokes it.
		Erase: testutil.NoopErase,
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

// TestRetentionService_SweepTenant_ParticipantPartialCountSurvivesError
// pins the sweep half of the count-on-error semantics the erasure side
// already records: a participant whose Sweep callback failed part-way
// through has already hard-deleted the rows it reports reaping, so that
// count must survive into SweepResult.Reaped -- TotalReaped and the audit
// event's Changes["reaped"] breakdown count rows that are genuinely and
// irreversibly gone even when the callback also errored, never silently
// dropping them from the record. A count recorded only on success would
// let a participant reporting (2, err) contribute 0 to TotalReaped and
// vanish from the audit trail's reaped map entirely.
func TestRetentionService_SweepTenant_ParticipantPartialCountSurvivesError(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)

	captured := &[]audit.RecordedEvent{}
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			*captured = append(*captured, rec)
		}
		return nil
	})

	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	partial := pkgcore.RetentionParticipant{
		Name: "testutil.partial",
		// NoopErase satisfies the registrar's mandatory-Erase rule; the
		// retention sweep under test never invokes it.
		Erase: testutil.NoopErase,
		Sweep: func(ctx context.Context, _ pkgcore.TenantID, _ time.Time) (int, error) {
			// A genuine part-way failure: hard-delete both expired rows
			// for real -- the sweep's own system context is what makes
			// repo.HardDelete legal here -- then fail before reporting
			// completion, the same (reaped, err) mid-loop shape
			// testutil.NewParticipant's own Sweep returns when one of
			// its HardDelete calls fails.
			for _, id := range []string{"expired-1", "expired-2"} {
				if err := repo.HardDelete(ctx, id); err != nil {
					return 0, err
				}
			}
			return 2, errFakeParticipant
		},
	}
	if err := reg.Retention.Add(partial); err != nil {
		t.Fatalf("register partial participant: %v", err)
	}
	svc := newRetentionService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions

	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())
	seedFakeNote(t, repo, tenant, "expired-2", "subject-2", wellPastDefaultWindow())

	result, err := svc.SweepTenant(context.Background(), tenant)
	if !hasCode(err, ErrSweepPartialFailure.Code) {
		t.Fatalf("SweepTenant error = %v, want %s", err, ErrSweepPartialFailure.Code)
	}
	if got := result.Reaped["testutil.partial"]; got != 2 {
		t.Errorf("Reaped[testutil.partial] = %d, want 2 -- the rows the failing participant reaped before its error must still be counted", got)
	}
	if result.Errors["testutil.partial"] == nil {
		t.Error("Errors[testutil.partial] is missing -- the failure must still be reported alongside the count")
	}
	if got := result.TotalReaped(); got != 2 {
		t.Errorf("TotalReaped() = %d, want 2", got)
	}
	for _, id := range []string{"expired-1", "expired-2"} {
		if fakeNoteExists(t, repo, tenant, id) {
			t.Errorf("%s should have been hard-deleted by the failing participant's sweep", id)
		}
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	changes := events[0].Changes
	if changes == nil {
		t.Fatal("audit event Changes = nil, want the reaped/errors breakdown")
	}
	reaped, ok := changes.After["reaped"].(map[string]int)
	if !ok {
		t.Fatalf("Changes.After[\"reaped\"] = %T, want map[string]int", changes.After["reaped"])
	}
	if got := reaped["testutil.partial"]; got != 2 {
		t.Errorf("audit Changes reaped[testutil.partial] = %d, want 2 -- the reaped breakdown must count rows actually hard-deleted even alongside the error", got)
	}
	if events[0].Result.Success {
		t.Error("audit Result.Success = true, want false -- the participant error must still mark the pass failed")
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
// schedule point puts on the queue: one task of the retention-sweep type
// for the tenant in context, with no payload and with the window-scoped
// idempotency key that collapses one retentionSweepWindowSize window's
// concurrent enqueues into one job -- the enqueue's key naming the window
// (retentionSweepWindowStart) its clock places it in.
func TestRetentionService_EnqueueRetentionSweep_ShapesTheTask(t *testing.T) {
	svc, _ := newRetentionHarness(t)
	now := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
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
	want := retentionSweepIdempotencyKey("tenant-a", retentionSweepWindowStart(now))
	if task.IdempotencyKey != want {
		t.Errorf("task.IdempotencyKey = %q, want %q (the enqueue's own window, not a tenant-only key)", task.IdempotencyKey, want)
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

// TestRetentionService_SweepTenant_ChangesRecordClassificationNeverErrorText
// pins the audit-trail content rule for the sweep path: a participant
// whose Sweep callback failed must appear in the sweep audit event's
// Changes as a classification (the participant's name keyed to
// participantErrorMarker) -- never with the callback's error text
// verbatim. The changes column is the one place on the audit table from
// which nothing can ever be removed (dbkit/audit/emit.go's Diff content
// contract: "Anything written here is effectively permanent"), and a
// sweep-path error can carry internal row or storage details no audit
// reader was ever promised. The error text's homes are the returned
// SweepResult.Errors (asserted below to still carry the raw error -- the
// in-process home) and the structured log at the failure site, behind
// go/observability's redaction layer -- never the permanent audit record.
// emitSweepAudit must never write err.Error() verbatim into
// Changes["errors"]; a verbatim write fails the assertions below.
func TestRetentionService_SweepTenant_ChangesRecordClassificationNeverErrorText(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)

	captured := &[]audit.RecordedEvent{}
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			*captured = append(*captured, rec)
		}
		return nil
	})

	// The failure text names an internal object key -- the class of
	// platform-internal content the permanent record must never carry.
	carving := errors.New("hard delete compliance/exports/tenant-a/x.json failed: object store timeout")
	failing := pkgcore.RetentionParticipant{
		Name: "testutil.carving_sweep",
		// NoopErase satisfies the registrar's mandatory-Erase rule; the
		// retention sweep under test never invokes it.
		Erase: testutil.NoopErase,
		Sweep: func(context.Context, pkgcore.TenantID, time.Time) (int, error) {
			return 0, carving
		},
	}
	if err := reg.Retention.Add(failing); err != nil {
		t.Fatalf("register failing participant: %v", err)
	}
	svc := newRetentionService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if !hasCode(err, ErrSweepPartialFailure.Code) {
		t.Fatalf("SweepTenant error = %v, want %s", err, ErrSweepPartialFailure.Code)
	}
	// The raw error must still reach this call's own caller -- the
	// in-process home that makes the classification in the audit record a
	// lossless trade for everyone entitled to the text.
	if !errors.Is(result.Errors["testutil.carving_sweep"], carving) {
		t.Errorf("SweepResult.Errors[%q] = %v, want the raw error preserved for the caller", "testutil.carving_sweep", result.Errors["testutil.carving_sweep"])
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	changes := events[0].Changes
	if changes == nil {
		t.Fatal("audit event Changes = nil, want the reaped/errors breakdown")
	}
	errs, ok := changes.After["errors"].(map[string]string)
	if !ok {
		t.Fatalf("Changes.After[\"errors\"] = %T, want map[string]string -- the classification map, never the raw errors", changes.After["errors"])
	}
	if got := errs["testutil.carving_sweep"]; got != participantErrorMarker {
		t.Errorf("Changes errors[%q] = %q, want the classification marker %q", "testutil.carving_sweep", got, participantErrorMarker)
	}
	raw, marshalErr := json.Marshal(changes)
	if marshalErr != nil {
		t.Fatalf("json.Marshal(Changes) error = %v", marshalErr)
	}
	if strings.Contains(string(raw), carving.Error()) {
		t.Errorf("the participant error text %q was carved into the audit trail Changes: %s", carving.Error(), raw)
	}
	if events[0].Result.Success {
		t.Error("audit Result.Success = true, want false -- the participant error must still mark the pass failed")
	}
}

// configModuleStub feeds go/config's own embedded migrations to
// dbkit.MigrationRegistry, mirroring module_test.go's fakeAuditModule for
// dbkit/audit's (a module's own migrations are not exported through the
// module type, so a test that wants the configs table migrated applies the
// migrations directly) -- only Name and Migrations are ever read by
// MigrationRegistry.Apply here.
type configModuleStub struct{}

func (configModuleStub) Name() string                     { return "config" }
func (configModuleStub) DependsOn() []string              { return nil }
func (configModuleStub) Migrations() embed.FS             { return configmigrations.FS }
func (configModuleStub) Locales() embed.FS                { return embed.FS{} }
func (configModuleStub) OpenAPISpec() []byte              { return nil }
func (configModuleStub) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = configModuleStub{}

// newRetentionConfigService returns a live *config.Service over a freshly
// migrated configs table, attached the way a host attaches one: a real
// config.Module registered on a real pkgcore.Registry, its schema frozen
// by Attach. withComplianceItems controls whether compliance's own
// Register ran on that registry first -- the schema then carries
// ConfigDefaultRetentionWindow (the shape of every real host, which
// bootstraps the module) or does not (the shape of a config service whose
// schema was frozen before compliance ever registered, which is the one
// live way RetentionWindow's config read can error). A test wires the
// returned service onto a RetentionService with svc.cfg = <it>, exactly
// what Module.WithConfigService does.
func newRetentionConfigService(t *testing.T, withComplianceItems bool) *config.Service {
	t.Helper()
	db := dbtest.NewSQLite(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(configModuleStub{}); err != nil {
		t.Fatalf("register config migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply config migrations: %v", err)
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	cfgModule := config.NewModule(db)
	if err := cfgModule.Register(reg); err != nil {
		t.Fatalf("config.Module.Register: %v", err)
	}
	if withComplianceItems {
		m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
		if err := m.Register(reg); err != nil {
			t.Fatalf("compliance.Module.Register: %v", err)
		}
	}
	svc, err := cfgModule.Attach(reg)
	if err != nil {
		t.Fatalf("config.Module.Attach: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestRetentionService_SweepTenant_ConfigWiredOverrideDrivesTheCutoff
// proves the reason WithConfigService exists: with a *config.Service wired
// and a tenant-level override of ConfigDefaultRetentionWindow set, the
// sweep's cutoff follows the override, not the module's own 30-day
// default. A soft-deleted row inside the override window but far outside
// the default window is reaped only because the configured value shrank
// the window -- without the wiring the same rows would survive every sweep
// until the operator's override took effect.
func TestRetentionService_SweepTenant_ConfigWiredOverrideDrivesTheCutoff(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	svc.cfg = newRetentionConfigService(t, true)

	tenant := pkgcore.TenantID("tenant-a")
	override := 5 * 24 * time.Hour
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	if err := svc.cfg.Set(ctx, config.ScopeTenant, ConfigDefaultRetentionWindow,
		config.Value{Data: override}, config.Actor("compliance-test-admin")); err != nil {
		t.Fatalf("set tenant retention window override: %v", err)
	}

	window, err := svc.RetentionWindow(ctx, tenant)
	if err != nil {
		t.Fatalf("RetentionWindow: %v", err)
	}
	if window != override {
		t.Fatalf("RetentionWindow = %v, want the tenant override %v", window, override)
	}

	seedFakeNote(t, repo, tenant, "past-override", "subject-1", time.Now().Add(-10*24*time.Hour))
	seedFakeNote(t, repo, tenant, "within-override", "subject-1", time.Now().Add(-24*time.Hour))

	result, err := svc.SweepTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.TotalReaped() != 1 {
		t.Fatalf("TotalReaped() = %d, want exactly 1 -- only the row past the 5-day override window", result.TotalReaped())
	}
	if fakeNoteExists(t, repo, tenant, "past-override") {
		t.Error("the row soft-deleted 10 days ago should have been reaped under the 5-day override")
	}
	if !fakeNoteExists(t, repo, tenant, "within-override") {
		t.Error("the row soft-deleted a day ago must survive -- it is inside the override window")
	}
}

// TestRetentionService_SweepTenant_ConfigReadErrorFailsClosedBeforeAnyParticipant
// proves the sweep never guesses a window when its wired config service
// cannot answer: RetentionWindow's read of ConfigDefaultRetentionWindow
// errors when the service's frozen schema predates compliance's own
// registration (the key is not declared), and SweepTenant propagates that
// error before any participant runs -- a mis-wired or stale config service
// must fail the sweep loudly rather than silently sweeping under an
// unvalidated default.
func TestRetentionService_SweepTenant_ConfigReadErrorFailsClosedBeforeAnyParticipant(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	svc.cfg = newRetentionConfigService(t, false)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	if _, err := svc.RetentionWindow(ctx, tenant); err == nil {
		t.Error("RetentionWindow over a schema without ConfigDefaultRetentionWindow = nil error, want one")
	}

	result, err := svc.SweepTenant(context.Background(), tenant)
	if err == nil {
		t.Fatal("SweepTenant = nil error, want the config read error propagated")
	}
	if result.Tenant != "" || !result.Cutoff.IsZero() || result.Reaped != nil || result.Errors != nil {
		t.Errorf("SweepTenant result = %+v, want the zero result -- no participant may run when the window cannot be resolved", result)
	}
	if !fakeNoteExists(t, repo, tenant, "expired-1") {
		t.Error("the expired row must survive: the sweep failed before any participant ran")
	}
}

// TestRetentionService_SweepTenant_SystemContextAuditPublishFailure_FailsClosed
// proves the system-context grant fails closed when its own audit record
// cannot be published: tenancy.WithSystemContext publishes
// EventSystemContextEntered and refuses the elevation on a publish error,
// so SweepTenant must return that error and call no participant -- an
// elevated sweep with no audit trail is exactly the gap the audited
// wrapper exists to close.
func TestRetentionService_SweepTenant_SystemContextAuditPublishFailure_FailsClosed(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 1, failErr: errors.New("scripted bus refuses")}
	svc, repo := newRetentionHarnessOn(t, bus)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	result, err := svc.SweepTenant(context.Background(), tenant)
	if !hasCode(err, tenancy.ErrAuditPublishFailed.Code) {
		t.Fatalf("SweepTenant error = %v, want %s", err, tenancy.ErrAuditPublishFailed.Code)
	}
	if result.Tenant != "" || !result.Cutoff.IsZero() || result.Reaped != nil || result.Errors != nil {
		t.Errorf("SweepTenant result = %+v, want the zero result", result)
	}
	if !fakeNoteExists(t, repo, tenant, "expired-1") {
		t.Error("the expired row must survive: the sweep was refused before any participant ran")
	}
}

// TestRetentionService_SweepTenant_SweepAuditRecordFailure_SurfacesWithResult
// proves the audit-record failure contract of the sweep path: the pass
// itself ran (rows are genuinely and irreversibly gone) but the one audit
// event recording it could not be published, so SweepTenant returns the
// completed SweepResult alongside ErrAuditRecordFailed -- the operator is
// told the sweep happened AND that its record is missing, never one at the
// expense of the other.
func TestRetentionService_SweepTenant_SweepAuditRecordFailure_SurfacesWithResult(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 2, failErr: errors.New("scripted bus refuses")}
	svc, repo := newRetentionHarnessOn(t, bus)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	result, err := svc.SweepTenant(context.Background(), tenant)
	if !hasCode(err, ErrAuditRecordFailed.Code) {
		t.Fatalf("SweepTenant error = %v, want %s", err, ErrAuditRecordFailed.Code)
	}
	if result.TotalReaped() != 1 {
		t.Errorf("TotalReaped() = %d, want 1 -- the sweep ran before its audit publish failed", result.TotalReaped())
	}
	if fakeNoteExists(t, repo, tenant, "expired-1") {
		t.Error("expired-1 should have been reaped: the participant ran before the audit publish failed")
	}
}

// TestRetentionService_SweepAllTenants_ListerFailureFailsClosed proves the
// whole-tenant sweep answers a failing lister with that error and no
// results -- it never sweeps a guessed subset when the enumeration itself
// failed.
func TestRetentionService_SweepAllTenants_ListerFailureFailsClosed(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	svc.lister = testutil.FakeTenantLister{Err: errors.New("tenant directory unavailable")}
	seedFakeNote(t, repo, "tenant-a", "expired-1", "s", wellPastDefaultWindow())

	results, err := svc.SweepAllTenants(context.Background())
	if err == nil {
		t.Fatal("SweepAllTenants with a failing lister = nil error, want the lister error")
	}
	if results != nil {
		t.Errorf("results = %v, want nil -- nothing was swept", results)
	}
	if !fakeNoteExists(t, repo, "tenant-a", "expired-1") {
		t.Error("no tenant may be swept when the lister itself failed")
	}
}

// TestRetentionService_SweepAllTenants_OneTenantsFailureDoesNotBlockOthers
// proves the per-tenant isolation SweepAllTenants exists to deliver at the
// whole-call level: tenant-a's sweep completes and reaps its rows even
// though tenant-b's own sweep fails its system-context audit publish, and
// the aggregate error names exactly the tenant that failed -- a caller
// checking only the top-level error still learns something needs
// attention, while the results map still holds tenant-a's completed
// outcome.
func TestRetentionService_SweepAllTenants_OneTenantsFailureDoesNotBlockOthers(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 3, failErr: errors.New("scripted bus refuses")}
	svc, repo := newRetentionHarnessOn(t, bus)
	seedFakeNote(t, repo, "tenant-a", "a-expired", "s", wellPastDefaultWindow())
	seedFakeNote(t, repo, "tenant-b", "b-expired", "s", wellPastDefaultWindow())
	svc.lister = testutil.FakeTenantLister{Tenants: []pkgcore.TenantID{"tenant-a", "tenant-b"}}

	results, err := svc.SweepAllTenants(context.Background())
	if err == nil {
		t.Fatal("SweepAllTenants = nil error, want the aggregate error naming tenant-b")
	}
	if !strings.Contains(err.Error(), "tenant-b") {
		t.Errorf("aggregate error = %q, want it to name tenant-b", err)
	}
	if strings.Contains(err.Error(), "tenant-a") {
		t.Errorf("aggregate error = %q, must not name tenant-a -- its sweep succeeded", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d tenants, want 2 -- every listed tenant has an entry", len(results))
	}
	if results["tenant-a"].TotalReaped() != 1 {
		t.Errorf("results[tenant-a] = %+v, want its own 1 reaped row", results["tenant-a"])
	}
	if fakeNoteExists(t, repo, "tenant-a", "a-expired") {
		t.Error("tenant-a's expired row should have been reaped despite tenant-b's failure")
	}
	if !fakeNoteExists(t, repo, "tenant-b", "b-expired") {
		t.Error("tenant-b's expired row must survive: its sweep never ran")
	}
}

// TestRetentionSweepHandler_TypeAndFailedSweep pins the two remaining
// halves of the jobs.Handler wrapper: Type names the exact task type the
// handler was claimed under (Register claims taskTypeRetentionSweep, and
// the queue dispatches by Type, so a drift between the two would silently
// strand every enqueued sweep), and a failed sweep is returned as the
// Handle error rather than swallowed -- a worker reports it for retry or
// dead-lettering instead of logging success.
func TestRetentionSweepHandler_TypeAndFailedSweep(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 1, failErr: errors.New("scripted bus refuses")}
	svc, repo := newRetentionHarnessOn(t, bus)
	seedFakeNote(t, repo, "tenant-a", "expired-1", "s", wellPastDefaultWindow())

	h := retentionSweepHandler{svc: svc}
	if h.Type() != taskTypeRetentionSweep {
		t.Errorf("Type() = %q, want %q", h.Type(), taskTypeRetentionSweep)
	}

	job := &jobs.Job{Type: taskTypeRetentionSweep, TenantID: "tenant-a"}
	result, err := h.Handle(context.Background(), job, nil)
	if err == nil {
		t.Fatal("Handle over a failed sweep = nil error, want the sweep's error propagated")
	}
	if result.Data != nil {
		t.Errorf("Handle result = %+v, want the zero result", result)
	}
	if !fakeNoteExists(t, repo, "tenant-a", "expired-1") {
		t.Error("the expired row must survive: the failed sweep never reached the participant")
	}
}
