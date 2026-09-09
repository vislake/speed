package queuetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
)

// This file extends the fault tier's teeth tests
// (assert_fails_closed_rejects_swallowing_queues_test.go) from the one
// defect the tier exists to detect -- swallowing the cancellation-state
// read failure -- to the tier's intermediate-state refusals: every error
// answer a check returns when the queue under test misbehaves at exactly
// one point of the checked scenario (a Cancel that cannot land, a
// sabotage that cannot be applied, a repair that loses the
// cancellation, ...). Each row drives the check against a
// faultTierFake with exactly that one operation defect and requires the
// check to reject it; each row first runs the same check against the
// defect-free twin, so the rejection below is the defect's own work --
// the identical discipline the swallowing tests state.
//
// defectTierQueue is the defect injector: the embedded faultTierFake
// provides the working machinery, and each overridden method consults
// the behavior map to fail or no-op exactly one operation.
type defectTierQueue struct {
	*faultTierFake
	behavior map[string]string // op -> "err" (the op reports a failure) or a no-op/lying mode spelled per op below
	repaired bool
}

func newDefectTierQueue(behavior map[string]string) *defectTierQueue {
	return &defectTierQueue{faultTierFake: newFaultTierFake(false), behavior: behavior}
}

func (q *defectTierQueue) op(op string) string { return q.behavior[op] }

// RegisterHandler implements queuetest.Runnable.
func (q *defectTierQueue) RegisterHandler(h jobs.Handler) error {
	if q.op("register") == "err" {
		return errors.New("defect: RegisterHandler refused")
	}
	return q.faultTierFake.RegisterHandler(h)
}

// Start implements queuetest.Runnable.
func (q *defectTierQueue) Start(ctx context.Context) error {
	if q.op("start") == "err" {
		return errors.New("defect: Start refused")
	}
	return q.faultTierFake.Start(ctx)
}

// Enqueue implements jobs.Queue.
func (q *defectTierQueue) Enqueue(ctx context.Context, task jobs.Task, opts ...jobs.EnqueueOption) (jobs.JobID, error) {
	if q.op("enqueue") == "err" {
		return "", errors.New("defect: Enqueue refused")
	}
	return q.faultTierFake.Enqueue(ctx, task, opts...)
}

// Cancel implements jobs.Queue.
func (q *defectTierQueue) Cancel(ctx context.Context, id jobs.JobID) error {
	switch q.op("cancel") {
	case "err":
		return errors.New("defect: Cancel refused")
	case "noop":
		// The silent defect: Cancel succeeds but records nothing, so the
		// healthy read still answers the Job's natural status.
		return nil
	}
	return q.faultTierFake.Cancel(ctx, id)
}

// Get implements jobs.Queue.
func (q *defectTierQueue) Get(ctx context.Context, id jobs.JobID) (*jobs.Job, error) {
	switch q.op("get") {
	case "err":
		return nil, errors.New("defect: Get refused")
	case "deadletter-as-success":
		// The lying-read defect: a genuinely dead-lettered Job reads back as
		// succeeded, the natural-state shape the DeadLetterJobs check's own
		// scenario distinguishes from the DeadLetter status it demands.
		job, err := q.faultTierFake.Get(ctx, id)
		if err == nil && job.Status == jobs.StatusDeadLetter {
			copy := *job
			copy.Status = jobs.StatusSucceeded
			return &copy, nil
		}
		return job, err
	}
	if q.op("get-after-repair") == "err" && q.repaired {
		return nil, errors.New("defect: Get refused after repair")
	}
	return q.faultTierFake.Get(ctx, id)
}

// Sabotage implements queuetest.CancellationStateFault.
func (q *defectTierQueue) Sabotage(ctx context.Context, id jobs.JobID) error {
	if q.op("sabotage") == "err" {
		return errors.New("defect: Sabotage refused")
	}
	return q.faultTierFake.Sabotage(ctx, id)
}

// Repair implements queuetest.CancellationStateFault.
func (q *defectTierQueue) Repair(ctx context.Context, id jobs.JobID) error {
	if q.op("repair") == "err" {
		return errors.New("defect: Repair refused")
	}
	if err := q.faultTierFake.Repair(ctx, id); err != nil {
		return err
	}
	q.repaired = true
	switch q.op("repair") {
	case "forgets-cancel":
		// The repair clears the fault but also loses the cancellation the
		// check recorded before the sabotage.
		q.mu.Lock()
		delete(q.cancelled, id)
		q.mu.Unlock()
	case "forgets-dead":
		// The repair clears the fault but also loses the dead-lettered Job
		// itself, so the post-repair listing no longer finds it.
		q.mu.Lock()
		delete(q.records, id)
		q.mu.Unlock()
	}
	return nil
}

// DeadLetterJobs implements queuetest.Runnable.
func (q *defectTierQueue) DeadLetterJobs(ctx context.Context) ([]*jobs.Job, error) {
	if q.op("list-after-repair") == "err" && q.repaired {
		return nil, errors.New("defect: DeadLetterJobs refused after repair")
	}
	return q.faultTierFake.DeadLetterJobs(ctx)
}

// TestFaultTierChecks_RejectEachDefectiveIntermediateState is the
// matrix: for each defect, the fail-closed twin passes the check and the
// defective queue is rejected with the check's own error -- pinning that
// every refusal branch of both checks is a live, reachable judgment and
// not dead scaffolding.
func TestFaultTierChecks_RejectEachDefectiveIntermediateState(t *testing.T) {
	getDefects := []struct {
		name     string
		behavior map[string]string
	}{
		{name: "RegisterHandler refused", behavior: map[string]string{"register": "err"}},
		{name: "Start refused", behavior: map[string]string{"start": "err"}},
		{name: "Enqueue refused", behavior: map[string]string{"enqueue": "err"}},
		{name: "Cancel refused", behavior: map[string]string{"cancel": "err"}},
		{name: "Get refused", behavior: map[string]string{"get": "err"}},
		{name: "Cancel silently records nothing", behavior: map[string]string{"cancel": "noop"}},
		{name: "Sabotage refused", behavior: map[string]string{"sabotage": "err"}},
		{name: "Repair refused", behavior: map[string]string{"repair": "err"}},
		{name: "Get refused after repair", behavior: map[string]string{"get-after-repair": "err"}},
		{name: "Repair loses the cancellation", behavior: map[string]string{"repair": "forgets-cancel"}},
	}
	for _, tc := range getDefects {
		t.Run("get_check_"+tc.name, func(t *testing.T) {
			if err := checkGetFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
				t.Fatalf("the Get check rejected the defect-free twin: %v", err)
			}
			q := newDefectTierQueue(tc.behavior)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = q.Close(ctx)
			})
			if err := checkGetFailsClosedOnUnreadableCancellationState(q); err == nil {
				t.Errorf("the Get check accepted a queue whose %s defect is exactly what the tier must refuse", tc.name)
			}
		})
	}

	deadLetterDefects := []struct {
		name     string
		behavior map[string]string
	}{
		{name: "RegisterHandler refused", behavior: map[string]string{"register": "err"}},
		{name: "Start refused", behavior: map[string]string{"start": "err"}},
		{name: "Enqueue refused", behavior: map[string]string{"enqueue": "err"}},
		{name: "dead-lettered Job reads back as succeeded", behavior: map[string]string{"get": "deadletter-as-success"}},
		{name: "Sabotage refused", behavior: map[string]string{"sabotage": "err"}},
		{name: "Repair refused", behavior: map[string]string{"repair": "err"}},
		{name: "DeadLetterJobs refused after repair", behavior: map[string]string{"list-after-repair": "err"}},
		{name: "Repair loses the dead-lettered Job", behavior: map[string]string{"repair": "forgets-dead"}},
	}
	for _, tc := range deadLetterDefects {
		t.Run("dead_letter_check_"+tc.name, func(t *testing.T) {
			if err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
				t.Fatalf("the DeadLetterJobs check rejected the defect-free twin: %v", err)
			}
			q := newDefectTierQueue(tc.behavior)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = q.Close(ctx)
			})
			if err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(q); err == nil {
				t.Errorf("the DeadLetterJobs check accepted a queue whose %s defect is exactly what the tier must refuse", tc.name)
			}
		})
	}
}
