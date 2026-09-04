package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
)

func TestService_Sweep_MarksExpiredAndExhaustedShares(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	past := now.Add(-time.Hour)
	expired, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &past})
	if err != nil {
		t.Fatalf("Create(expired): %v", err)
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
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)
	past := now.Add(-time.Hour)
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &past}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (first): %v", err)
	}
	if err := svc.Sweep(testCtx()); err != nil {
		t.Fatalf("Sweep (second, nothing left to reap): %v", err)
	}
}

func TestExpirySweepHandler_Type(t *testing.T) {
	h := expirySweepHandler{}
	if got := h.Type(); got != taskTypeExpirySweep {
		t.Errorf("Type() = %q, want %q", got, taskTypeExpirySweep)
	}
}

func TestExpirySweepHandler_Handle_RunsSweep(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)
	past := now.Add(-time.Hour)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &past})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := expirySweepHandler{svc: svc}
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
	h := expirySweepHandler{svc: svc}
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
	if err := m.EnqueueExpirySweep(testCtx()); err != nil {
		t.Fatalf("EnqueueExpirySweep: %v", err)
	}
	if fq.lastTask.Type != taskTypeExpirySweep {
		t.Errorf("Task.Type = %q, want %q", fq.lastTask.Type, taskTypeExpirySweep)
	}
	if fq.lastTask.TenantID != testTenant {
		t.Errorf("Task.TenantID = %q, want %q", fq.lastTask.TenantID, testTenant)
	}
	if fq.lastTask.IdempotencyKey != expirySweepIdempotencyKey(testTenant) {
		t.Errorf("Task.IdempotencyKey = %q, want %q", fq.lastTask.IdempotencyKey, expirySweepIdempotencyKey(testTenant))
	}
}
