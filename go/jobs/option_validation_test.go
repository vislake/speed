package jobs

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the option-validation regressions for the root package's
// StandaloneQueue CONSTRUCTION options (standalone_queue.go). It pins one
// rule, applied uniformly to all five With* options in that file: an
// invalid value is refused at option time with a coded panic -- matching
// pkgcore's own constructor-time-refusal convention (NewSMTPMailer,
// NewLocalObjectStore, ...) -- never accepted and silently reinterpreted,
// and never left to fail after Start has already reported success. What
// counts as invalid is per-option and stated on each With* function's own
// doc comment, with the reason the value is unhonourable: worker counts
// and the per-tenant concurrency limit below 1 (a queue that silently
// processes nothing), and durations at or below zero (a zero poll interval
// would panic a background ticker only AFTER Start had succeeded; a zero
// or negative timeout or backoff cannot be honoured literally and would
// silently collapse onto the default, or into an immediate retry burst).
// The completeness claim is deliberate: every one of the five options in
// standalone_queue.go has a regression here, so the suite really does
// cover each option's invalid-value behaviour, and a new constructor
// option cannot be added to that file without landing its refusal test
// beside it. Named for the behaviour it verifies. (The per-Enqueue options
// in queue.go are a
// separate layer with their own individually documented rules --
// WithMaxRetries clamps, WithTimeout falls back -- and are not what this
// file pins.)

// assertOptionPanics asserts that fn panics with a coded *apperr.Error
// carrying code -- the option-time refusal contract every With*
// construction option in standalone_queue.go shares.
func assertOptionPanics(t *testing.T, code string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s: expected a coded panic %q, got none", t.Name(), code)
		}
		e, ok := r.(*apperr.Error)
		if !ok {
			t.Fatalf("%s: panic value = %T(%v), want a coded *apperr.Error %q", t.Name(), r, r, code)
		}
		if e.Code != code {
			t.Fatalf("%s: panic code = %q, want %q", t.Name(), e.Code, code)
		}
	}()
	fn()
}

// TestWithWorkerCount_ZeroOrNegative_Refused pins the WithWorkerCount(0)
// refusal: a queue with no workers would claim every eligible Job into
// StatusRunning and execute none of them -- a silent no-op that looks like
// a healthy queue in every metric except the ones that never move.
func TestWithWorkerCount_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithWorkerCount(0) })
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithWorkerCount(-4) })
}

// TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused pins the
// WithTenantConcurrencyLimit(0) refusal: a limit of zero would refuse
// every tenant admission forever. Same fail-before shape as the worker
// count test.
func TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(0) })
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(-1) })
}

// TestWithPollInterval_ZeroOrNegative_Refused pins the WithPollInterval(0)
// refusal against the crash shape construction-time validation exists to
// prevent: accepted silently, a zero poll interval would let Start report
// success and then the dispatcher goroutine's time.NewTicker would panic
// and kill the whole process.
func TestWithPollInterval_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.poll_interval_zero", func() { WithPollInterval(0) })
	assertOptionPanics(t, "jobs.poll_interval_zero", func() { WithPollInterval(-5 * time.Millisecond) })
}

// TestWithJobTimeout_ZeroOrNegative_Refused pins the WithJobTimeout(0)
// refusal: a non-positive timeout is this package's "not set" marker, so a
// zero or negative configured default could never be honoured literally --
// it would silently leave every Job on DefaultTimeout, indistinguishable in
// operation from an option that was never passed.
func TestWithJobTimeout_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(0) })
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(-1 * time.Second) })
}

// TestWithBackoff_ZeroOrNegative_Refused pins the WithBackoff zero-or-
// negative refusal on each bound: backoffDelay would return a zero or
// negative delay from such a configuration, collapsing the exponential
// spread into an immediate retry burst at the poll cadence until retries
// are exhausted -- the opposite of what the option exists to configure.
func TestWithBackoff_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.backoff_base_zero", func() { WithBackoff(0, DefaultBackoffMax) })
	assertOptionPanics(t, "jobs.backoff_base_zero", func() { WithBackoff(-1*time.Second, DefaultBackoffMax) })
	assertOptionPanics(t, "jobs.backoff_max_zero", func() { WithBackoff(DefaultBackoffBase, 0) })
	assertOptionPanics(t, "jobs.backoff_max_zero", func() { WithBackoff(DefaultBackoffBase, -1*time.Second) })
}
