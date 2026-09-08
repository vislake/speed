// This file lives in package jobs_test -- the external test package,
// distinct from standalone_queue_test.go's internal package jobs -- because
// it must import go/jobs/queuetest, which itself imports go/jobs: an
// internal test file (package jobs) importing a package that imports jobs
// back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"), while an external test file compiles as a separate
// package and carries no such restriction. This is the mechanical exception
// the testing-layout rule names for exactly this
// situation (package x vs. package x_test cases cannot share a file), the
// same one go/pkgcore/kv_conformance_test.go's own doc comment documents
// for kvstoretest.
package jobs_test

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queuetest"
)

// TestStandaloneQueue_ConformsToQueueContract proves StandaloneQueue
// satisfies the shared jobs.Queue contract queuetest.AssertConforms checks
// -- the standalone deployment mode's half of the proof that both
// implementations (this one and go/jobs/queue/asynq's, proven by
// go/jobs/integration_test's own TestAsynqQueue_ConformsToQueueContract)
// agree on Enqueue/Get/Cancel/idempotency/retry/dead-letter semantics.
// The five contract subtests (enqueue_get_happy_path,
// get_tenant_isolation, cancel_tenant_isolation_and_idempotency,
// retry_succeeds_after_transient_failures,
// dead_letter_exhausts_retries_and_invokes_failure_hook) are the single
// home of the portable contract, covering what was once hand-maintained
// separately for each implementation.
func TestStandaloneQueue_ConformsToQueueContract(t *testing.T) {
	queuetest.AssertConforms(t, func() queuetest.Runnable {
		db := dbtest.NewSQLite(t)
		return jobs.NewStandaloneQueue(db,
			jobs.WithPollInterval(15*time.Millisecond),
			jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		)
	})
}
