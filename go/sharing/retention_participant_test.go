package sharing

// retention_participant_test.go drives sharing's access-log retention
// participant (retention_participant.go): the pkgcore.RetentionParticipant
// the module registers so compliance's per-tenant retention sweep reaps
// access-log entries past the tenant's retention window, the same
// mechanism RetentionService already runs for every other participant
// (go/compliance). The sweep callback is invoked here with the context
// shape RetentionService.SweepTenant hands every participant -- tenant,
// structured actor and system context (compliance enters the audited
// wrapper itself; the unit tier uses pkgcore's primitive, which carries
// the same presence dbkit.HardDelete gates on).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testRetentionPurpose is the audited system purpose this file's sweep
// contexts enter, standing in for compliance's own
// SystemPurposeRetentionSweep (registered by compliance's Module.Register;
// the unit tier registers its own).
const testRetentionPurpose pkgcore.SystemPurpose = "sharing.test_retention_sweep"

// retentionSweepCtx returns the context shape RetentionService.SweepTenant
// hands every participant's Sweep callback: a tenant, a structured actor
// (the audit-capture attribution dbkit's HardDelete doc comment calls for
// when a db carries capture) and a system context naming the purpose.
func retentionSweepCtx(tenant pkgcore.TenantID) context.Context {
	pkgcore.RegisterSystemPurpose(testRetentionPurpose)
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: "system", ID: "test", DisplayName: "sharing retention test"})
	ctx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "sharing retention test",
		Purpose: testRetentionPurpose,
	})
	if err != nil {
		panic(err) // the purpose is registered above; unreachable
	}
	return ctx
}

// TestAccessLogRepository_ListOlderThan pins the retention sweep's
// candidate listing: entries whose OccurredAt is at or before cutoff,
// tenant-scoped like every read of the table -- another tenant's equally
// old entries must never appear, and a fresh entry must never.
func TestAccessLogRepository_ListOlderThan(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")
	cutoff := now.Add(-time.Hour)

	old := newAccessLogEntry("old-a", "share-1", now.Add(-2*time.Hour))
	if err := repo.Create(ctxA, old); err != nil {
		t.Fatalf("Create(old): %v", err)
	}
	atCutoff := newAccessLogEntry("at-cutoff-a", "share-1", cutoff)
	if err := repo.Create(ctxA, atCutoff); err != nil {
		t.Fatalf("Create(at-cutoff): %v", err)
	}
	fresh := newAccessLogEntry("fresh-a", "share-1", now)
	if err := repo.Create(ctxA, fresh); err != nil {
		t.Fatalf("Create(fresh): %v", err)
	}
	foreign := newAccessLogEntry("old-b", "share-2", now.Add(-2*time.Hour))
	if err := repo.Create(ctxB, foreign); err != nil {
		t.Fatalf("Create(foreign): %v", err)
	}

	got, err := repo.listOlderThan(ctxA, cutoff)
	if err != nil {
		t.Fatalf("listOlderThan: %v", err)
	}
	ids := map[string]bool{}
	for _, row := range got {
		ids[row.ID] = true
	}
	if !ids[old.ID] || !ids[atCutoff.ID] {
		t.Errorf("listOlderThan = %v, want both the pre-cutoff entry and the exactly-at-cutoff one", got)
	}
	if ids[fresh.ID] {
		t.Errorf("listOlderThan included the fresh entry %q, want entries at or before cutoff only", fresh.ID)
	}
	if ids[foreign.ID] {
		t.Errorf("listOlderThan included another tenant's entry %q, want tenant-scoped candidates", foreign.ID)
	}
}

// TestAccessLogRetentionParticipant_Sweep_ReapsOnlyPastCutoffEntries drives
// the participant's Sweep callback the way RetentionService runs it: under
// a system context for one tenant, entries whose OccurredAt is at or before
// cutoff are hard-deleted (counted in the result), younger entries and
// another tenant's entries survive, and a re-run over the same tenant finds
// nothing left to reap and reports 0 -- the retry-convergence contract every
// participant's Sweep keeps.
func TestAccessLogRetentionParticipant_Sweep_ReapsOnlyPastCutoffEntries(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")
	cutoff := now.Add(-time.Hour)

	oldA := newAccessLogEntry("old-a", "share-1", now.Add(-2*time.Hour))
	if err := repo.Create(ctxA, oldA); err != nil {
		t.Fatalf("Create(oldA): %v", err)
	}
	freshA := newAccessLogEntry("fresh-a", "share-1", now)
	if err := repo.Create(ctxA, freshA); err != nil {
		t.Fatalf("Create(freshA): %v", err)
	}
	oldB := newAccessLogEntry("old-b", "share-2", now.Add(-2*time.Hour))
	if err := repo.Create(ctxB, oldB); err != nil {
		t.Fatalf("Create(oldB): %v", err)
	}

	participant := NewAccessLogRetentionParticipant(repo)
	reaped, err := participant.Sweep(retentionSweepCtx("tenant-a"), "tenant-a", cutoff)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("Sweep reaped %d entries, want the one past-cutoff entry", reaped)
	}

	// The reaped row is physically gone from its own tenant's table.
	byID, err := repo.FindByID(ctxA, oldA.ID)
	if err == nil || byID != nil {
		t.Errorf("the reaped entry still reads back (row %+v, err %v), want it gone", byID, err)
	}
	// The fresh entry and the other tenant's old entry survive.
	if _, findErr := repo.FindByID(ctxA, freshA.ID); findErr != nil {
		t.Errorf("the fresh entry was reaped: %v", findErr)
	}
	if _, findErr := repo.FindByID(ctxB, oldB.ID); findErr != nil {
		t.Errorf("another tenant's old entry was reaped: %v", findErr)
	}

	// A re-run converges: nothing left past the cutoff reports 0, not an
	// error -- the retry-convergence contract the mechanism documents.
	again, err := participant.Sweep(retentionSweepCtx("tenant-a"), "tenant-a", cutoff)
	if err != nil {
		t.Fatalf("Sweep re-run: %v", err)
	}
	if again != 0 {
		t.Errorf("Sweep re-run reaped %d, want 0 -- already-reaped rows must not be double-counted", again)
	}
}

// TestAccessLogRetentionParticipant_Sweep_SurvivesTheStatusesOfOtherTables
// pins the participant's narrowness: the sweep touches access-log rows
// only, never the shares they were recorded against -- a share whose log
// entries were reaped keeps serving and stays listed (sharing_shares is
// not this participant's table).
func TestAccessLogRetentionParticipant_Sweep_SurvivesTheStatusesOfOtherTables(t *testing.T) {
	shares := NewShareRepository(newTestDB(t))
	logs := NewAccessLogRepository(shares.db)
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	cutoff := now.Add(-time.Hour)

	share := newTestShare("share-1", now)
	if err := shares.Create(ctxA, share); err != nil {
		t.Fatalf("Create(share): %v", err)
	}
	old := newAccessLogEntry("old-a", share.ID, now.Add(-2*time.Hour))
	if err := logs.Create(ctxA, old); err != nil {
		t.Fatalf("Create(old log entry): %v", err)
	}

	participant := NewAccessLogRetentionParticipant(logs)
	if _, err := participant.Sweep(retentionSweepCtx("tenant-a"), "tenant-a", cutoff); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := shares.FindByID(ctxA, share.ID); err != nil {
		t.Errorf("the sweep reaped the share itself (err %v), want access-log rows only", err)
	}
}

// TestAccessLogRetentionParticipant_EraseDeclaresNothingSubjectShaped pins
// the participant's erasure answer: no sharing row carries a subject
// attribution (shares store no creator, log entries no viewer user id), so
// the Erase callback states the nothing-to-erase fact explicitly with
// (0, nil) -- the pkgcore.RetentionParticipant contract's own answer for a
// participant with nothing subject-shaped, the same shape go/compliance's
// export-manifests participant declares for its tenant-wide bundles.
func TestAccessLogRetentionParticipant_EraseDeclaresNothingSubjectShaped(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	participant := NewAccessLogRetentionParticipant(repo)

	erased, err := participant.Erase(retentionSweepCtx("tenant-a"), pkgcore.SubjectRef{
		TenantID:  "tenant-a",
		SubjectID: "user-1",
	})
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if erased != 0 {
		t.Errorf("Erase erased %d rows, want the explicit nothing-to-erase (0, nil)", erased)
	}
}

// TestAccessLogRetentionParticipant_AssertIsolated runs the tenant-data
// isolation suite over the participant's own table -- the retention sweep
// must never be able to reap across tenants, so the candidate listing's
// isolation is the property this suite pins at the repository level (the
// rows this test writes carry no tenant context of their own beyond the
// suite's).
func TestAccessLogRetentionParticipant_AssertIsolated(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	tenancytest.AssertIsolated(t, repo.Repository, func(_ pkgcore.TenantID) *AccessLogEntry {
		return newAccessLogEntry(uuid.NewString(), "share-1", now)
	})
}

// newAccessLogEntry returns one access-log row for the retention tests with
// an explicit OccurredAt (autoCreateTime only fills a zero value, so the
// age a sweep judges is asserted against a real difference, never against
// however fast two inserts land on the test's own clock).
func newAccessLogEntry(id, shareID string, at time.Time) *AccessLogEntry {
	return &AccessLogEntry{
		ID:         id,
		ShareID:    shareID,
		OccurredAt: at,
		Outcome:    AccessOutcomeGranted,
	}
}
