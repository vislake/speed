package pki

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// recordingQueue is a jobs.Queue double that records every Task it was
// asked to Enqueue, used only to prove EnqueueExpiryScan shapes the task
// correctly -- it never actually runs a worker.
type recordingQueue struct {
	tasks []jobs.Task
	err   error
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	if q.err != nil {
		return "", q.err
	}
	q.tasks = append(q.tasks, task)
	return jobs.JobID("job-1"), nil
}
func (*recordingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (*recordingQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = (*recordingQueue)(nil)

// TestService_EnqueueExpiryScan_NoQueueWired proves a Service built without
// WithQueue (module.go) reports a plain error rather than panicking --
// scheduling rotation is optional, per Service.queue's own doc comment.
func TestService_EnqueueExpiryScan_NoQueueWired(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnqueueExpiryScan(context.Background()); err == nil {
		t.Fatalf("EnqueueExpiryScan with no queue wired succeeded, want an error")
	}
}

// TestService_EnqueueExpiryScan_ShapesTheTask proves the task Enqueue
// receives: the fixed taskTypeExpiryScan type, the fixed
// platformScanTenantID (pki_signing_keys is platform data with no real
// tenant to put here -- see platformScanTenantID's own doc comment), and no
// payload.
func TestService_EnqueueExpiryScan_ShapesTheTask(t *testing.T) {
	svc := newTestService(t)
	queue := &recordingQueue{}
	svc.attachQueue(queue)

	if err := svc.EnqueueExpiryScan(context.Background()); err != nil {
		t.Fatalf("EnqueueExpiryScan: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("Enqueue was called %d times, want exactly 1", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.Type != taskTypeExpiryScan {
		t.Errorf("task.Type = %q, want %q", task.Type, taskTypeExpiryScan)
	}
	if task.TenantID != platformScanTenantID {
		t.Errorf("task.TenantID = %q, want %q", task.TenantID, platformScanTenantID)
	}
	if len(task.Payload) != 0 {
		t.Errorf("task.Payload = %q, want empty", task.Payload)
	}
}

// TestService_EnqueueExpiryScan_PropagatesAQueueFailure proves a queue
// error is surfaced rather than swallowed.
func TestService_EnqueueExpiryScan_PropagatesAQueueFailure(t *testing.T) {
	svc := newTestService(t)
	wantErr := errors.New("queue is down")
	svc.attachQueue(&recordingQueue{err: wantErr})

	if err := svc.EnqueueExpiryScan(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("EnqueueExpiryScan error = %v, want %v", err, wantErr)
	}
}

// --- expiryScanHandler --------------------------------------------------------

func TestExpiryScanHandler_Type(t *testing.T) {
	h := expiryScanHandler{}
	if got := h.Type(); got != taskTypeExpiryScan {
		t.Errorf("Type() = %q, want %q", got, taskTypeExpiryScan)
	}
}

// TestExpiryScanHandler_Handle_RunsScanExpiry proves Handle actually drives
// the state machine: a pending key past its propagation window is promoted
// when the handler runs.
func TestExpiryScanHandler_Handle_RunsScanExpiry(t *testing.T) {
	svc := newTestService(t)
	svc.propagationWindow = time.Nanosecond // any elapsed wall-clock time is "due"
	ctx := context.Background()

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	h := expiryScanHandler{svc: svc}
	result, err := h.Handle(ctx, &jobs.Job{TenantID: platformScanTenantID}, nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(result.Data) != 0 {
		t.Errorf("Handle returned Data %q, want empty", result.Data)
	}

	got, err := svc.signingKeys.FindByID(ctx, "kid-pending")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != SigningKeyStatusActive {
		t.Errorf("kid-pending status after Handle = %q, want %q", got.Status, SigningKeyStatusActive)
	}
}

// TestExpiryScanHandler_Handle_RejectsANonEmptyPayload proves the task-shape
// guard: a scan takes its inputs from the rows and the clock, never a
// payload, and a non-empty one is a shape violation the retry policy can
// never fix by re-running.
func TestExpiryScanHandler_Handle_RejectsANonEmptyPayload(t *testing.T) {
	svc := newTestService(t)
	h := expiryScanHandler{svc: svc}
	_, err := h.Handle(context.Background(), &jobs.Job{Payload: []byte("{}")}, nil)
	if err == nil {
		t.Fatalf("Handle with a non-empty payload succeeded, want an error")
	}
}

// compile-time reminder that platformScanTenantID really is a
// pkgcore.TenantID, the type jobs.Task.TenantID requires.
var _ pkgcore.TenantID = platformScanTenantID
