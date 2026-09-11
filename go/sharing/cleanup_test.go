package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

func TestService_Sweep_MarksExpiredAndExhaustedShares(t *testing.T) {
	svc, _ := newTestService(t, nil)
	// Rule 2's validation (service.go's resolveExpiry) refuses a share
	// whose expiry is already past at creation, so the expired share is
	// created with an expiry in the future of the CREATE clock and the
	// clock then moves past it before the sweep runs.
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepAt := createAt.Add(2 * time.Hour)
	svc.now = fixedClock(createAt)

	expiring := createAt.Add(time.Hour)
	expired, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create(expiring): %v", err)
	}

	one := 1
	exhausted, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create(exhausted): %v", err)
	}
	if _, accessErr := svc.Access(testCtx(), exhausted.Token, AccessParams{}); accessErr != nil {
		t.Fatalf("Access(exhausting the one view): %v", accessErr)
	}

	live, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create(live): %v", err)
	}

	svc.now = fixedClock(sweepAt)
	if sweepErr := svc.Sweep(testCtx()); sweepErr != nil {
		t.Fatalf("Sweep: %v", sweepErr)
	}

	got, err := svc.Get(testCtx(), expired.Share.ID)
	if err != nil {
		t.Fatalf("Get(expired): %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("expired share's RevokedAt is nil after Sweep, want set")
	}

	got, err = svc.Get(testCtx(), exhausted.Share.ID)
	if err != nil {
		t.Fatalf("Get(exhausted): %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("exhausted share's RevokedAt is nil after Sweep, want set")
	}

	got, err = svc.Get(testCtx(), live.Share.ID)
	if err != nil {
		t.Fatalf("Get(live): %v", err)
	}
	if got.RevokedAt != nil {
		t.Errorf("live share's RevokedAt = %v after Sweep, want nil -- Sweep must not touch a still-live share", got.RevokedAt)
	}
}

func TestService_Sweep_IsIdempotent(t *testing.T) {
	svc, _ := newTestService(t, nil)
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepAt := createAt.Add(2 * time.Hour)
	svc.now = fixedClock(createAt)
	expiring := createAt.Add(time.Hour)
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	svc.now = fixedClock(sweepAt)
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (first): %v", err)
	}
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (second, nothing left to reap): %v", err)
	}
}

// TestService_Sweep_RefundsInterruptedReservations pins the sweep's
// reservation arm: a view reservation that has OUTLIVED viewReservationTimeout
// -- presumed left behind by a serve that died without resolving it -- is
// refunded by the sweep, so a crashed serve's dead reservation never
// squats on a limited share's last view; a reservation still LIVE (the
// serve is presumably still streaming) is untouched by the pass.
func TestService_Sweep_RefundsInterruptedReservations(t *testing.T) {
	svc, _ := newTestService(t, nil)
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepAt := createAt.Add(viewReservationTimeout + 2*time.Minute)
	svc.now = fixedClock(createAt)

	one := 1
	interrupted, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r1", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create(interrupted): %v", err)
	}
	live, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r2", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create(live): %v", err)
	}

	// Take the two reservations at explicit instants: the interrupted
	// serve's at createAt (stale by sweep time), the live serve's just
	// before the sweep (still live at sweep time).
	interruptedShare, err := svc.Get(testCtx(), interrupted.Share.ID)
	if err != nil {
		t.Fatalf("Get(interrupted): %v", err)
	}
	liveShare, err := svc.Get(testCtx(), live.Share.ID)
	if err != nil {
		t.Fatalf("Get(live): %v", err)
	}
	if won, reserveErr := svc.shares.tryReserveView(testCtx(), interruptedShare, createAt); reserveErr != nil || !won {
		t.Fatalf("tryReserveView(interrupted): won=%v err=%v", won, reserveErr)
	}
	if won, reserveErr := svc.shares.tryReserveView(testCtx(), liveShare, sweepAt.Add(-time.Minute)); reserveErr != nil || !won {
		t.Fatalf("tryReserveView(live): won=%v err=%v", won, reserveErr)
	}

	svc.now = fixedClock(sweepAt)
	if sweepErr := svc.Sweep(testCtx()); sweepErr != nil {
		t.Fatalf("Sweep: %v", sweepErr)
	}

	got, err := svc.Get(testCtx(), interrupted.Share.ID)
	if err != nil {
		t.Fatalf("Get(interrupted, after sweep): %v", err)
	}
	if got.ViewsReserved != 0 || got.ViewsReservedAt != nil {
		t.Errorf("interrupted serve's reservation after Sweep = ViewsReserved %d / ViewsReservedAt %v, want 0 / nil -- the stale reservation must be refunded", got.ViewsReserved, got.ViewsReservedAt)
	}
	if got.RevokedAt != nil {
		t.Errorf("interrupted share's RevokedAt = %v after Sweep, want nil -- a stale reservation alone must not revoke a live share", got.RevokedAt)
	}

	got, err = svc.Get(testCtx(), live.Share.ID)
	if err != nil {
		t.Fatalf("Get(live, after sweep): %v", err)
	}
	if got.ViewsReserved != 1 {
		t.Errorf("live serve's ViewsReserved after Sweep = %d, want 1 -- a reservation still younger than the timeout must be untouched", got.ViewsReserved)
	}
}

// TestService_Sweep_PublishesShareRevokedPerReapedShare pins the EventShareRevoked
// contract module.go's constant declares -- "owner-initiated or
// sweep-initiated alike": a sweep that marks a share must announce that
// revocation on the bus exactly as Service.Revoke does, one event per
// share this pass actually transitioned. A second sweep of the same rows
// (idempotence) publishes nothing more.
func TestService_Sweep_PublishesShareRevokedPerReapedShare(t *testing.T) {
	svc, bus := newTestService(t, nil)
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepAt := createAt.Add(2 * time.Hour)
	svc.now = fixedClock(createAt)

	expiring := createAt.Add(time.Hour)
	first, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create(first expiring): %v", err)
	}
	second, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create(second expiring): %v", err)
	}
	live, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create(live): %v", err)
	}

	revoked := map[string]int{}
	bus.Subscribe(EventShareRevoked, func(_ context.Context, evt pkgcore.Event) error {
		if p, ok := evt.Payload.(ShareRevokedPayload); ok {
			revoked[p.ShareID]++
		}
		return nil
	})

	svc.now = fixedClock(sweepAt)
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (first): %v", err)
	}
	if revoked[first.Share.ID] != 1 || revoked[second.Share.ID] != 1 {
		t.Errorf("EventShareRevoked count after first sweep = %v, want exactly one per reaped share (%q and %q)", revoked, first.Share.ID, second.Share.ID)
	}
	if revoked[live.Share.ID] != 0 {
		t.Errorf("EventShareRevoked fired for the still-live share -- the sweep must not announce a revocation it did not perform")
	}

	// Idempotence: the second sweep reaps nothing, so it publishes nothing.
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (second): %v", err)
	}
	if len(revoked) != 2 || revoked[first.Share.ID] != 1 || revoked[second.Share.ID] != 1 {
		t.Errorf("EventShareRevoked count after second sweep = %v, want unchanged (one each for the two reaped shares, none for the live one)", revoked)
	}
}

func TestExpirySweepHandler_Handle_RunsSweep(t *testing.T) {
	svc, _ := newTestService(t, nil)
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepAt := createAt.Add(2 * time.Hour)
	svc.now = fixedClock(createAt)
	expiring := createAt.Add(time.Hour)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := jobs.NewEmptyPayloadHandler(taskTypeExpirySweep, svc.Sweep)
	svc.now = fixedClock(sweepAt)
	job := &jobs.Job{TenantID: testTenant}
	if _, handleErr := h.Handle(testCtx(), job, nil); handleErr != nil {
		t.Fatalf("Handle: %v", handleErr)
	}

	got, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("RevokedAt is nil after Handle, want set")
	}
}

func TestExpirySweepHandler_Handle_RefusesNonEmptyPayload(t *testing.T) {
	svc, _ := newTestService(t, nil)
	h := jobs.NewEmptyPayloadHandler(taskTypeExpirySweep, svc.Sweep)
	job := &jobs.Job{TenantID: testTenant, Payload: []byte("{}")}
	if _, err := h.Handle(testCtx(), job, nil); err == nil {
		t.Errorf("Handle with a non-empty payload succeeded, want an error")
	}
}

func TestModule_EnqueueExpirySweep_RequiresQueue(t *testing.T) {
	m := NewModule(newTestDB(t))
	err := m.EnqueueExpirySweep(testCtx())
	if !errors.Is(err, ErrQueueRequiredForSweep) {
		t.Errorf("EnqueueExpirySweep (no queue) error = %v, want ErrQueueRequiredForSweep", err)
	}
}

// recordingQueue is a minimal jobs.Queue double recording the last Enqueue
// call, enough to prove Module.EnqueueExpirySweep builds the right
// jobs.Task.
type recordingQueue struct {
	lastTask jobs.Task
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.lastTask = task
	return "job-1", nil
}

func (q *recordingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }

func (q *recordingQueue) Cancel(context.Context, jobs.JobID) error { return nil }

var _ jobs.Queue = (*recordingQueue)(nil)

func TestModule_EnqueueExpirySweep_BuildsTheExpectedTask(t *testing.T) {
	fq := &recordingQueue{}
	m := NewModule(newTestDB(t), WithQueue(fq))
	// Pin the module service's clock -- the seam EnqueueExpirySweep reads
	// the enqueue's window from -- so the expected key is deterministic.
	now := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	m.svc.now = func() time.Time { return now }
	if err := m.EnqueueExpirySweep(testCtx()); err != nil {
		t.Fatalf("EnqueueExpirySweep: %v", err)
	}
	if fq.lastTask.Type != taskTypeExpirySweep {
		t.Errorf("Task.Type = %q, want %q", fq.lastTask.Type, taskTypeExpirySweep)
	}
	if fq.lastTask.TenantID != testTenant {
		t.Errorf("Task.TenantID = %q, want %q", fq.lastTask.TenantID, testTenant)
	}
	if fq.lastTask.IdempotencyKey != "sharing.sweep:tenant-a:2026-09-07T10:00:00Z" {
		t.Errorf("Task.IdempotencyKey = %q, want %q (the enqueue's own window, not a tenant-only key)", fq.lastTask.IdempotencyKey, "sharing.sweep:tenant-a:2026-09-07T10:00:00Z")
	}
}
