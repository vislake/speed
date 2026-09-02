package jobs_test

// Runnable documentation for the jobs public API, mirroring
// go/pkgcore/example_test.go's, go/dbkit/example_test.go's and
// go/tenancy/example_test.go's identical convention: every example here is
// compiled AND executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of only rotting in prose (root
// CLAUDE.md's Documentation section; this package's own AGENTS.md).
//
// Both examples poll Queue.Get in a tight loop bounded by a short deadline
// rather than sleeping a fixed duration: DemoQueue's dispatch/execute cycle
// is asynchronous by design (see AGENTS.md), so a fixed sleep would either
// make this file's own test run needlessly slow or be a source of
// flakiness under load -- polling is both fast and deterministic here
// because the registered Handlers do no real work.

import (
	"context"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// exampleGreeter is a jobs.Handler that greets whoever Task.Payload names,
// reporting progress once along the way.
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

// Example demonstrates the whole Queue contract this package pins:
// register a Handler, Enqueue a Task under a tenant, and poll Get until
// the resulting Job completes. See examples/reference-app (once a later
// milestone wires jobs into it) for the same shape used against a real
// business handler.
func Example() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:jobs_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	queue := jobs.NewDemoQueue(db, jobs.WithPollInterval(5*time.Millisecond))
	err = queue.RegisterHandler(exampleGreeter{})
	if err != nil {
		fmt.Println("register handler:", err)
		return
	}
	err = queue.Start(ctx)
	if err != nil {
		fmt.Println("start:", err)
		return
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
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
	job, err := waitForTerminal(tenantCtx, queue, id, time.Now().Add(2*time.Second))
	if err != nil {
		fmt.Println("get:", err)
		return
	}

	fmt.Println("status:", job.Status)
	fmt.Println("attempts:", job.Attempts)
	fmt.Println("result:", string(job.Result.Data))

	// Output:
	// status: succeeded
	// attempts: 1
	// result: Hello, speed!
}

// ExampleNewHandlerFunc shows the lightweight way to satisfy Handler for a
// simple case: adapt a plain function instead of declaring a named type
// (compare exampleGreeter above, which Example uses instead precisely to
// also show the named-type shape a real Handler typically takes).
func ExampleNewHandlerFunc() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:jobs_example_handlerfunc?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	queue := jobs.NewDemoQueue(db, jobs.WithPollInterval(5*time.Millisecond))
	echo := jobs.NewHandlerFunc("echo", func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{Data: job.Payload}, nil
	})
	err = queue.RegisterHandler(echo)
	if err != nil {
		fmt.Println("register handler:", err)
		return
	}
	err = queue.Start(ctx)
	if err != nil {
		fmt.Println("start:", err)
		return
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = queue.Close(shutdownCtx)
	}()

	id, err := queue.Enqueue(ctx, jobs.Task{
		Type:     "echo",
		TenantID: pkgcore.TenantID("acme"),
		Payload:  []byte("ping"),
	})
	if err != nil {
		fmt.Println("enqueue:", err)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "acme")
	job, err := waitForTerminal(tenantCtx, queue, id, time.Now().Add(2*time.Second))
	if err != nil {
		fmt.Println("get:", err)
		return
	}

	fmt.Println("result:", string(job.Result.Data))

	// Output:
	// result: ping
}
