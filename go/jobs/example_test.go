package jobs_test

// Runnable documentation for the jobs public API, mirroring
// go/pkgcore/example_test.go's, go/dbkit/example_test.go's and
// go/tenancy/example_test.go's identical convention: every example here is
// compiled AND executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of only rotting in prose.
//
// Both examples poll Queue.Get in a tight loop bounded by a short deadline
// rather than sleeping a fixed duration: StandaloneQueue's dispatch/execute
// cycle is asynchronous by design, so a fixed sleep would either
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
// the resulting Job completes. See examples/reference-app's own wiring
// for the same shape used against a real business handler.
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

	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))
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

	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))
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

// ExampleWithEventBus shows the terminal signal end to end: a queue
// configured with an EventBus publishes one jobs.job.terminal event per
// terminal transition, so a subscriber learns a Job ended without polling
// it — here a subscription on the shared in-memory bus, reduced to the one
// payload field the printout uses. The subscriber clause the event's own
// doc comment spells out (idempotency, the row as the truth, a
// reconciliation net where completeness matters) is what a real subscriber
// is written against; asynq.Queue (WithEventBus on that implementation)
// publishes the same payload at its terminal points, with the
// publish-before-archive ordering the event's doc comment records.
func ExampleWithEventBus() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:jobs_example_terminal?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	bus := pkgcore.NewMemoryEventBus()
	terminal := make(chan jobs.JobTerminalEvent, 1)
	bus.Subscribe(jobs.EventJobTerminal, func(_ context.Context, evt pkgcore.Event) error {
		payload := evt.Payload.(jobs.JobTerminalEvent)
		select {
		case terminal <- payload:
		default:
		}
		return nil
	})

	queue := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(5*time.Millisecond),
		jobs.WithEventBus(bus),
	)
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

	_, err = queue.Enqueue(ctx, jobs.Task{
		Type:     "greet",
		TenantID: pkgcore.TenantID("acme"),
		Payload:  []byte("speed"),
	})
	if err != nil {
		fmt.Println("enqueue:", err)
		return
	}

	select {
	case payload := <-terminal:
		fmt.Println("job_type:", payload.JobType)
		fmt.Println("status:", payload.Status)
	case <-time.After(2 * time.Second):
		fmt.Println("timed out waiting for the terminal event")
	}

	// Output:
	// job_type: greet
	// status: succeeded
}

// ExampleWire demonstrates the host-side wiring call, the shape every
// host's assembly uses after Kernel.Bootstrap: the modules have declared
// their handlers on the registry (the single Handle call stands in for a
// module's Register walking its own declarations), the host hands the whole
// registry to Wire, and Start remains the host's own step -- a
// worker-disabled replica stops after Wire and still serves Enqueue. See
// examples/reference-app's BuildServer for the same call against the real
// module set.
func ExampleWire() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:jobs_example_wire?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	err = reg.Jobs.Handle("greet", exampleGreeter{})
	if err != nil {
		fmt.Println("declare handler:", err)
		return
	}

	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))
	err = jobs.Wire(ctx, queue, reg.Jobs)
	if err != nil {
		fmt.Println("wire:", err)
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
		Payload:  []byte("wire"),
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
	fmt.Println("result:", string(job.Result.Data))

	// Output:
	// status: succeeded
	// result: Hello, wire!
}

// The distributed deployment mode's own Queue implementation lives in its
// own subpackage, go/jobs/queue/asynq, precisely so a consumer that only
// ever runs jobs.StandaloneQueue (the standalone deployment mode) never
// pulls in asynq or go-redis at all; see that subpackage's own
// ExampleNewQueue for the equivalent shape.

// exampleSweepRuns is a jobs.Handler that reports the tenant each sweep
// runs for, so ExampleScheduler can observe a real end-to-end tick.
type exampleSweepRuns struct{ ran chan string }

func (*exampleSweepRuns) Type() string { return "storage.expiry_sweep" }

func (h *exampleSweepRuns) Handle(ctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	tenant, _ := pkgcore.TenantFromContext(ctx)
	select {
	case h.ran <- string(tenant):
	default:
	}
	return jobs.Result{}, nil
}

// exampleTenantUniverse is the structural TenantLister seam: any value with
// a ListTenants method of this shape satisfies it, so a host implementation
// already built for another module needs no adapter.
type exampleTenantUniverse struct{ tenants []pkgcore.TenantID }

func (u exampleTenantUniverse) ListTenants(context.Context) ([]pkgcore.TenantID, error) {
	return u.tenants, nil
}

// ExampleScheduler shows the portable scheduler: it reads the periodic
// tasks a module declared on a pkgcore Registry's Schedules seat, expands
// the per-tenant ones through the host's tenant lister, and enqueues each
// under a window-scoped idempotency key -- so same-window ticks (however
// many replicas produce them) collapse onto one job, and the first tick of
// the next window runs the task again.
func ExampleScheduler() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:jobs_example_scheduler?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// The module side: declare the schedule where the task's handler is
	// registered. Declaring means scheduled -- a host that starts a
	// scheduler over the seat runs exactly this declaration.
	sweeps := &exampleSweepRuns{ran: make(chan string, 1)}
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Jobs.Handle(sweeps.Type(), sweeps); err != nil {
		fmt.Println("declare handler:", err)
		return
	}
	err = reg.Schedules.Add(pkgcore.PeriodicTask{
		Type:      sweeps.Type(),
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "storage.sweep:",
	})
	if err != nil {
		fmt.Println("declare schedule:", err)
		return
	}

	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))
	if err := jobs.Wire(ctx, queue, reg.Jobs); err != nil {
		fmt.Println("wire:", err)
		return
	}
	if err := queue.Start(ctx); err != nil {
		fmt.Println("start:", err)
		return
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = queue.Close(shutdownCtx)
	}()

	// The host side: one scheduler over the declarations, the tenant
	// universe it expands per-tenant tasks through, and the tick cadence.
	scheduler := jobs.NewScheduler(queue,
		jobs.WithSchedules(reg.Schedules),
		jobs.WithTenantLister(exampleTenantUniverse{tenants: []pkgcore.TenantID{"acme"}}),
		jobs.WithInterval(5*time.Millisecond),
	)
	if err := scheduler.Start(ctx); err != nil {
		fmt.Println("start scheduler:", err)
		return
	}
	defer scheduler.Stop()

	select {
	case tenant := <-sweeps.ran:
		fmt.Println("swept:", tenant)
	case <-time.After(2 * time.Second):
		fmt.Println("swept: nothing within the deadline")
	}

	// The key one window's enqueue landed under is deterministic: the
	// declaration's prefix, the tenant segment and the window start
	// (truncated on the absolute clock).
	fmt.Println(jobs.ScheduleIdempotencyKey("storage.sweep:", "acme",
		jobs.ScheduleWindowStart(time.Date(2026, 3, 4, 5, 37, 0, 0, time.UTC), time.Hour)))

	// Output:
	// swept: acme
	// storage.sweep:acme:2026-03-04T05:00:00Z
}
