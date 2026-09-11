package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// TestStop_NonBlockingWithInFlightJobAndCloseDrains pins the two-beat
// shutdown contract StandaloneQueue.Stop exists for: Stop returns while a
// Handle call is still in flight -- it only signals, it never waits for the
// drain -- and the Close that follows drains that in-flight Job to its own
// natural completion instead of truncating it.
func TestStop_NonBlockingWithInFlightJobAndCloseDrains(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(1))

	flood := &blockingHandler{startedCh: make(chan JobID, 1), releaseCh: make(chan struct{})}
	if err := q.RegisterHandler(flood); err != nil {
		t.Fatalf("RegisterHandler(flood) error = %v", err)
	}
	startQueue(t, q)

	id, err := q.Enqueue(context.Background(), Task{Type: "flood", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(flood) error = %v", err)
	}
	waitSignal(t, flood.startedCh, "the flood job to start")

	stopDone := make(chan struct{})
	go func() {
		q.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return while a Handle call was in flight; it must signal, never wait for the drain")
	}

	// The in-flight Job is still owned by the queue, not abandoned: releasing
	// the handler lets it complete, and Close (startQueue's cleanup, running
	// after this release) drains it to its terminal status rather than
	// cutting it short.
	close(flood.releaseCh)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q, ctx, id)
	if job.Status != StatusSucceeded {
		t.Errorf("the in-flight job's terminal status = %v, want %v: Close must drain it, not abandon it", job.Status, StatusSucceeded)
	}
}

// TestStop_IdempotentAndSafeWithoutStart pins Stop's lifecycle safety: a
// queue that never started tolerates Stop (there is nothing running to
// signal), a repeated Stop is a no-op, and the Close after Stop still
// returns cleanly -- the same "Close without a prior Start" contract Close
// already documents, reached through the new signal half.
func TestStop_IdempotentAndSafeWithoutStart(t *testing.T) {
	q := newTestQueue(t)

	q.Stop()
	q.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() after Stop() without a prior Start error = %v, want nil", err)
	}
}
