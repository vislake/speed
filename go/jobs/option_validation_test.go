package jobs

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the option-validation regressions for the root package's
// StandaloneQueue options: configuration values that would make the queue
// silently process nothing (or worse) must be refused at option time with a
// coded panic, matching pkgcore's own constructor-time-refusal convention
// (NewSMTPMailer, NewLocalObjectStore, ...), instead of being accepted and
// failing later -- or never. Named for the behaviour it verifies, per the
// backend coding standard's test-naming rule, since it spans every option
// function in standalone_queue.go.

// assertOptionPanics asserts that fn panics with a coded *apperr.Error
// carrying code -- the option-time refusal contract every validated
// With* option in this module shares.
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
// a healthy queue in every metric except the ones that never move. Fails
// on the pre-fix code, where the option is accepted silently.
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
