//go:build integration

// Package jobs_test holds go/jobs's integration tier: tests that exercise a
// real Redis instead of the standalone deployment mode's in-process
// SQLite-backed worker pool. It is physically separate from go/jobs's unit tests (all of which
// live in package jobs itself, one file per source file, per the backend
// coding standard's testing layout rule (§13)) and carries the
// "integration" build tag: a plain "go test ./..." never compiles or runs
// anything in this directory; it is invoked explicitly with
// "go test -tags=integration ./...". This mirrors go/dbkit/integration_test
// and go/tenancy/tenancytest/integration_test's identical convention --
// see dbkit's postgres_tenant_isolation_test.go for the pattern this file
// follows almost line for line, with a Redis container in place of
// Postgres.
//
// Every test here spins up its own disposable Redis container through
// go/pkgcore/redistest and requires a working Docker (or
// Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path, matching dbkit's own integration tier.
package jobs_test

import (
	"context"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore/redistest"
)

// startRedisContainer starts a disposable Redis 7 container and returns an
// asynqlib.RedisConnOpt connected to it, already terminated via t.Cleanup
// on test completion (pass or fail), so no container ever leaks past its
// owning test -- go/pkgcore/redistest's container lifecycle, wrapped in
// the one adaptation asynq needs.
func startRedisContainer(t *testing.T, ctx context.Context) asynqlib.RedisConnOpt {
	t.Helper()

	uri, err := redistest.Start(t, ctx).ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	connOpt, err := asynqlib.ParseRedisURI(uri)
	if err != nil {
		t.Fatalf("asynqlib.ParseRedisURI(%q): %v", uri, err)
	}
	return connOpt
}

// testAsynqDefaultOpts is the shared "short intervals for fast,
// deterministic tests" option list every queue this package starts uses --
// polling intervals and the tenant-throttle delay all turned down so tests
// observe outcomes in tens of milliseconds rather than asynq's own
// multi-second defaults (TaskCheckInterval defaults to 1s,
// DelayedTaskCheckInterval to 5s) -- the same convention go/jobs's own
// newTestQueue helper (queue_standalone_test.go, parent module) applies to
// StandaloneQueue's WithPollInterval/WithBackoff. Extracted so a test that
// must build its Queue from a container it started itself (to reach the
// same Redis with a raw client, e.g. marker-sabotage tests) starts from
// the identical tuning rather than a hand-maintained copy.
var testAsynqDefaultOpts = []asynq.Option{
	asynq.WithTaskCheckInterval(20 * time.Millisecond),
	asynq.WithDelayedTaskCheckInterval(50 * time.Millisecond),
	asynq.WithThrottleRetryDelay(20 * time.Millisecond),
	// asynqlib.DefaultRetryDelayFunc is tuned for real production
	// traffic (its first-retry delay alone is 15-44 SECONDS -- see
	// server.go's DefaultRetryDelayFunc: n=0 gives
	// 0 + 15 + rand.IntN(30)), the same reason StandaloneQueue's own tests
	// override WithBackoff instead of using DefaultBackoffBase/
	// DefaultBackoffMax. Every test in this package that exercises a
	// genuine retry needs this fast instead, or it would spend most
	// of its runtime asleep waiting on asynq's own default backoff.
	asynq.WithRetryDelayFunc(func(n int, err error, task *asynqlib.Task) time.Duration {
		return 20 * time.Millisecond
	}),
}

// newTestAsynqQueue returns an asynq.Queue (go/jobs/queue/asynq) connected
// to a fresh Redis container with testAsynqDefaultOpts applied (opts
// appended after, so a caller's override wins). Close is registered via
// t.Cleanup, bounded so a stuck test cannot hang forever.
func newTestAsynqQueue(t *testing.T, ctx context.Context, opts ...asynq.Option) *asynq.Queue {
	t.Helper()
	connOpt := startRedisContainer(t, ctx)

	q := asynq.NewQueue(connOpt, append(testAsynqDefaultOpts, opts...)...)

	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.Close(closeCtx); err != nil {
			t.Errorf("Queue.Close() error = %v", err)
		}
	})
	return q
}

// startTestAsynqQueue is newTestAsynqQueue plus Start, for the common case
// where a test has no need to Enqueue before Start the way StandaloneQueue's own
// TestPriorityOrdering does (asynq's own task-check-interval polling means
// there is no equivalent "before the dispatcher's first tick" race to avoid
// here).
func startTestAsynqQueue(t *testing.T, ctx context.Context, opts ...asynq.Option) *asynq.Queue {
	t.Helper()
	q := newTestAsynqQueue(t, ctx, opts...)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Queue.Start() error = %v", err)
	}
	return q
}

// waitForTerminal polls Get until id's Job reaches a terminal Status or
// deadline passes, failing the test on timeout -- the same shape as
// go/jobs's own queue_standalone_test.go waitTerminal helper (parent package,
// unexported, not importable from here), redefined for this package since
// go test helpers are never part of a package's importable API regardless
// of which package they live in.
func waitForTerminal(t *testing.T, ctx context.Context, q jobs.Queue, id jobs.JobID, timeout time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *jobs.Job
	for {
		job, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", id, err)
		}
		last = job
		if job.Status.Terminal() {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for job %q to reach a terminal status; last observed state = %+v", timeout, id, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pollUntil polls Get every 10ms until done reports true or timeout
// elapses, returning the last observed Job either way -- used by tests
// that need to observe a specific TRANSIENT state (e.g. StatusRunning with
// a particular progress value) rather than only a terminal one.
func pollUntil(t *testing.T, ctx context.Context, q jobs.Queue, id jobs.JobID, timeout time.Duration, done func(*jobs.Job) bool) *jobs.Job {
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
			t.Fatalf("timed out after %v waiting for job %q; last observed state = %+v", timeout, id, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
