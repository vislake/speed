// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). A seam-contract
// driver of the module root's own built-ins is such a suite, and it must be
// black-box against package jobs: the driver imports go/jobs' queuetest
// support package, which itself imports go/jobs, and an internal test file
// (package jobs) importing a package that imports jobs back is an import
// cycle Go's toolchain refuses ("import cycle not allowed in test"). An
// external test package carries no such restriction.
package unittest

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
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
// home of the portable contract, run against both implementations.
func TestStandaloneQueue_ConformsToQueueContract(t *testing.T) {
	queuetest.AssertConforms(t, func() queuetest.Runnable {
		db := dbtest.NewSQLite(t)
		// Same pin as the in-package run of this suite (queuetest's own
		// assert_conforms_test.go): the contract under test is Enqueue
		// semantics, not SQLite's locking, and the concurrent-enqueue
		// subtest's racing statements against the queue's own background
		// writers would otherwise let a losing statement's bounded busy
		// budget expire on a loaded runner. See
		// testutil.PinSingleConnection.
		testutil.PinSingleConnection(t, db)
		return jobs.NewStandaloneQueue(db,
			jobs.WithPollInterval(15*time.Millisecond),
			jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		)
	})
}
