package queuetest

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
)

// This file runs the package's own conformance driver (assert_conforms.go)
// from inside the package's own test binary, so the driver's statements are
// exercised where they ship. The module's unit gate measures each package
// only through its own test binary, and AssertConforms is a shipped
// contract check no code in this package ever invoked on its own: its two
// call sites live in the jobs root package's and integration tier's test
// binaries (queue_conformance_test.go there), whose runs instrument their
// own packages, not queuetest. The factory below mirrors the standalone
// call site's (jobs' own queue_conformance_test.go) -- SQLite-backed
// StandaloneQueue with fast poll/backoff -- so this run doubles as the
// same-package acceptance pin the fault tier's own tests already play for
// assert_fails_closed.go (assert_fails_closed_test.go), against the real
// standalone implementation instead of the fault-tier fake, which cannot
// satisfy the healthy-path suite's idempotency and retry subtests by
// design.

// TestAssertConforms_StandaloneQueue runs the full conformance suite
// against a real StandaloneQueue -- the in-package run of the same suite
// the jobs root package's own queue_conformance_test.go drives from its
// external test binary.
func TestAssertConforms_StandaloneQueue(t *testing.T) {
	AssertConforms(t, func() Runnable {
		db := dbtest.NewSQLite(t)
		return jobs.NewStandaloneQueue(db,
			jobs.WithPollInterval(15*time.Millisecond),
			jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		)
	})
}
