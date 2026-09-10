package queuetest

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
)

// This file runs the package's own conformance driver (assert_conforms.go)
// from inside the package's own test binary, so the driver's statements are
// exercised where they ship. The module's unit gate measures each package
// only through its own test binary, and AssertConforms is a shipped
// contract check no code in this package ever invoked on its own: its two
// call sites live in the module unittest directory's and the integration
// tier's test binaries (both named queue_conformance_test.go -- go/jobs's
// unittest/ and its integration_test/), whose runs instrument their own
// packages, not queuetest. The factory below mirrors the standalone call
// site's -- unittest/queue_conformance_test.go's SQLite-backed
// StandaloneQueue with fast poll/backoff -- so this run doubles as the
// same-package acceptance pin the fault tier's own tests already play for
// assert_fails_closed.go (assert_fails_closed_test.go), against the real
// standalone implementation instead of the fault-tier fake, which cannot
// satisfy the healthy-path suite's idempotency and retry subtests by
// design.

// TestAssertConforms_StandaloneQueue runs the full conformance suite
// against a real StandaloneQueue -- the in-package run of the same suite
// the module's own unittest/queue_conformance_test.go drives from its
// black-box test package.
func TestAssertConforms_StandaloneQueue(t *testing.T) {
	AssertConforms(t, func() Runnable {
		db := dbtest.NewSQLite(t)
		// The pool is pinned to one connection so the suite measures the
		// Queue contract, never SQLite's multi-connection lock behaviour:
		// the concurrent-enqueue subtest drives ten racing Enqueues while
		// the queue's own dispatcher, heartbeat and workers write the same
		// file, and on a multi-connection pool a losing statement's bounded
		// busy budget can expire under that contention -- a lock error no
		// assertion here is about. See testutil.PinSingleConnection.
		testutil.PinSingleConnection(t, db)
		return jobs.NewStandaloneQueue(db,
			jobs.WithPollInterval(15*time.Millisecond),
			jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		)
	})
}
