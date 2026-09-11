package unittest

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
)

// The four helpers below mirror package jobs' own test helpers
// newTestQueue/startQueue/pollJob/waitTerminal (defined in the module
// root's queue_standalone_test.go, which stays in the source package for
// the in-package suites that use them). The black-box suites in this
// directory cannot call those unexported helpers, and the helpers cannot
// move to go/jobs/internal/testutil either: that package is imported by
// package jobs' own in-package tests (the metric helpers), so a testutil
// -> jobs import edge would close the very import cycle Go refuses for
// those tests. This support file is therefore the unittest side's mirror,
// kept in step with the in-package helpers' defaults and semantics.
//
// One deliberate difference: the in-package newTestQueue eagerly ensures
// the jobs schema at construction time, a step that reaches the unexported
// ensureJobsSchema and therefore cannot be reproduced here. This
// constructor defers schema creation to the queue's own Start, exactly as
// every StandaloneQueue consumer sees it. Every suite in this directory
// Starts the queue before its first Enqueue, so the two behave identically
// for them.

// newTestQueue returns a StandaloneQueue backed by a private, per-test
// temp-file SQLite database, not yet started: callers finish every
// construction step they need and then call startQueue. Poll interval and
// backoff are both set short so tests observe outcomes quickly; every
// value remains overridable via opts.
func newTestQueue(t *testing.T, opts ...jobs.Option) *jobs.StandaloneQueue {
	t.Helper()
	db := dbtest.NewSQLite(t)
	defaults := []jobs.Option{
		jobs.WithPollInterval(15 * time.Millisecond),
		jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
	}
	return jobs.NewStandaloneQueue(db, append(defaults, opts...)...)
}

// startQueue starts q and registers a bounded Close via t.Cleanup.
func startQueue(t *testing.T, q *jobs.StandaloneQueue) {
	t.Helper()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
}

// pollJob polls Get until done reports true or timeout elapses.
func pollJob(t *testing.T, q *jobs.StandaloneQueue, ctx context.Context, id jobs.JobID, timeout time.Duration, done func(*jobs.Job) bool) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *jobs.Job
	for {
		job, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", id, err)
		}
		last = job
		if done(job) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for job %q; last state = %+v", timeout, id, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitTerminal polls until id reaches a terminal Status.
func waitTerminal(t *testing.T, q *jobs.StandaloneQueue, ctx context.Context, id jobs.JobID) *jobs.Job {
	t.Helper()
	return pollJob(t, q, ctx, id, signalWaitTimeout, func(j *jobs.Job) bool { return j.Status.Terminal() })
}

// signalWaitTimeout bounds waitSignal: how long a channel-ready event
// may take under a slow scheduler before the test gives up. It mirrors
// the in-package helper's own constant (queue_standalone_test.go), with
// the same rationale: waitSignal is event-driven, so the cap is paid
// only when the event never arrives, and the generous ceiling is what
// keeps a starved scheduler from turning a delayed start into a test
// failure.
const signalWaitTimeout = 5 * time.Second

// waitSignal waits until ch delivers a value and returns it, failing
// the test with what on timeout. A closed channel counts as delivered
// (the receive returns the zero value immediately), so close-based
// signals and send-based ones share this one wait. Mirrors the
// in-package helper of the same name.
func waitSignal[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(signalWaitTimeout):
		t.Fatalf("timed out after %v waiting for %s", signalWaitTimeout, what)
		return *new(T)
	}
}
