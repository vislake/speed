package asynq_test

// Runnable documentation for the queue/asynq subpackage's public API,
// mirroring go/jobs/example_test.go's own convention (itself mirroring
// go/pkgcore/example_test.go's, go/dbkit/example_test.go's and
// go/tenancy/example_test.go's): every example here is compiled AND
// executed by `go test`, so an API change that invalidates the documented
// usage fails the build instead of only rotting in prose (root CLAUDE.md's
// Documentation section; this module's own AGENTS.md).

import (
	"context"
	"fmt"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/pkgcore"
)

// exampleGreeter is a jobs.Handler that greets whoever Task.Payload names,
// reporting progress once along the way. Duplicated from go/jobs's own
// example_test.go rather than shared: each package's Example* functions
// are compiled and run independently, per Go's own godoc convention, and
// this fixture is a handful of lines.
type exampleGreeter struct{}

func (exampleGreeter) Type() string { return "greet" }

func (exampleGreeter) Handle(_ context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
	progress(50, "composing greeting")
	name := string(job.Payload)
	return jobs.Result{Data: []byte("Hello, " + name + "!")}, nil
}

// waitForTerminal polls Get until id's Job reaches a terminal Status or
// deadline passes, returning whatever the last call observed either way.
func waitForTerminal(ctx context.Context, queue jobs.Queue, id jobs.JobID, deadline time.Time) (*jobs.Job, error) {
	for {
		job, err := queue.Get(ctx, id)
		if err != nil || job.Status.Terminal() || time.Now().After(deadline) {
			return job, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ExampleNewQueue shows the distributed deployment mode's shape of
// go/jobs's own Example -- register a Handler, Enqueue a Task under a
// tenant, poll Get until it completes -- against this package's Queue
// instead of jobs.StandaloneQueue. Every other line of jobs.Queue-facing
// code (jobs.Task, jobs.EnqueueOption, waitForTerminal) is identical to
// that Example's; only construction changes, exactly as jobs' own queue.go
// doc comment promises ("RegisterHandler, Start and Close... the
// distributed deployment mode's Redis/asynq-backed implementation is
// expected to need a different setup shape of its own").
//
// Deliberately has no "// Output:" comment, so go test compiles and
// type-checks this exactly like every other example here (catching a
// signature drift immediately, per root CLAUDE.md's "compiled and run by
// CI" documentation rule) WITHOUT executing it -- this package needs a
// real Redis, which the default, non-integration test tier this file
// belongs to must not require (see AGENTS.md's Testing section and
// integration_test/'s own package comment). The exact same shape,
// actually run end to end against a real Redis via testcontainers-go, is
// integration_test/enqueue_execute_test.go's
// TestRedisQueue_EnqueueExecuteRoundTrip.
func ExampleNewQueue() {
	ctx := context.Background()

	redisOpt := asynqlib.RedisClientOpt{Addr: "127.0.0.1:6379"}
	queue := asynq.NewQueue(redisOpt,
		asynq.WithConcurrency(8),
		asynq.WithTenantConcurrencyLimit(2),
	)
	if err := queue.RegisterHandler(exampleGreeter{}); err != nil {
		fmt.Println("register handler:", err)
		return
	}
	if err := queue.Start(ctx); err != nil {
		fmt.Println("start:", err)
		return
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = queue.Close(shutdownCtx)
	}()

	id, err := queue.Enqueue(ctx, jobs.Task{
		Type:     "greet",
		TenantID: pkgcore.TenantID("acme"),
		Payload:  []byte("speed"),
	})
	if err != nil {
		fmt.Println("enqueue:", err)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "acme")
	job, err := waitForTerminal(tenantCtx, queue, id, time.Now().Add(30*time.Second))
	if err != nil {
		fmt.Println("get:", err)
		return
	}

	fmt.Println("status:", job.Status)
	fmt.Println("result:", string(job.Result.Data))
}
