//go:build integration

package jobs_test

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/jobs/queuetest"
)

// TestAsynqQueue_ConformsToQueueContract proves go/jobs/queue/asynq's Queue
// satisfies the shared jobs.Queue contract queuetest.AssertConforms checks
// -- the distributed deployment mode's half of the proof that both
// implementations (this one, run here against a real Redis container, and
// StandaloneQueue's, proven by go/jobs's own
// TestStandaloneQueue_ConformsToQueueContract) agree on Enqueue/Get/
// Cancel/idempotency/retry/dead-letter semantics, replacing what used to
// be four independently hand-maintained tests in this package
// (TestRedisQueue_EnqueueExecuteRoundTrip, TestRedisQueue_Idempotency,
// TestRedisQueue_RetryOnFailure, TestRedisQueue_DeadLetterAndFailureHook,
// plus TestRedisQueue_Cancel_PendingJobNeverRuns's tenant-isolation-and-
// idempotency assertions) that duplicated, assertion for assertion, what
// go/jobs's own standalone_queue_test.go already proved for StandaloneQueue.
//
// Every subtest spins up its own disposable Redis container via
// newTestAsynqQueue (redis_container_test.go), matching every other test in
// this package's own per-test-container convention -- this file adds no
// new infrastructure pattern.
func TestAsynqQueue_ConformsToQueueContract(t *testing.T) {
	ctx := context.Background()
	queuetest.AssertConforms(t, func() queuetest.Runnable {
		return newTestAsynqQueue(t, ctx)
	})
}
