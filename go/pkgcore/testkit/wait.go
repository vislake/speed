package testkit

import (
	"testing"
	"time"
)

// DefaultConvergence is the deadline Eventually allows a condition: the
// budget every wait on an asynchronously delivered effect here shares, sized
// for the slowest of them (a remote event bus's reader goroutine waking on
// its own schedule) so a local in-process delivery converges well inside it
// too.
const DefaultConvergence = 10 * time.Second

// pollInterval is how often the pollers re-check their condition. It only
// sets the detection granularity -- how soon after an effect lands the poll
// notices -- never whether the effect is observed before the deadline.
const pollInterval = 10 * time.Millisecond

// Eventually polls cond until it reports true or DefaultConvergence passes,
// failing the test in the latter case with what named. It is the bounded
// loop every assertion on the far side of asynchronous delivery waits
// through -- a remote bus reader, a queue worker, a handler goroutine --
// rather than assuming the delivery landed with the publish.
func Eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	EventuallyWithin(t, DefaultConvergence, what, cond)
}

// EventuallyWithin is Eventually with an explicit timeout, for the waits
// whose budget differs from DefaultConvergence.
func EventuallyWithin(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	if !waitFor(timeout, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitFor polls cond until it reports true or the budget elapses. It exists
// as the error-free core of the pollers above so their timeout rejection is
// directly testable rather than only reachable by failing a real test.
func waitFor(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// runReceiveBudget bounds WaitForRun's receive: a sweep handler run lands on
// its channel as soon as the queue hands the job over, so the window is
// generous for a queue-backed delivery and a genuine miss fails loudly
// rather than hanging.
const runReceiveBudget = 20 * time.Second

// WaitForRun receives one value from runs and returns it, failing the test
// after runReceiveBudget when nothing arrives. runs is the buffered channel
// a job handler (or any asynchronous producer) fills once per run; what
// names the run for the failure message.
func WaitForRun[T any](t *testing.T, runs <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-runs:
		return v
	case <-time.After(runReceiveBudget):
		t.Fatalf("%s: no run within %s", what, runReceiveBudget)
		var zero T
		return zero
	}
}
