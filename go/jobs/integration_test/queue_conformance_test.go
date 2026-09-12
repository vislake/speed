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
// Cancel/idempotency/retry/dead-letter semantics -- the suite's subtests
// are the single maintained proof of those semantics; this package keeps
// no hand-maintained duplicates of them.
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
