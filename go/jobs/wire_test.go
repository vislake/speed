package jobs

// wire_test.go is the suite for wire.go's Wire -- the host-side call that
// drains a registry's declared handlers onto a StandaloneQueue and creates
// the queue's tables. It pins the properties a worker-disabled host depends
// on (handlers registered, tables present, no worker needed) and the order
// the reference app relies on (an entry that is not a jobs.Handler fails the
// wiring before any schema work happens).

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// wireTestRegistry returns a real pkgcore.ComponentRegistry over in-memory
// seams with declares driven inside a real Init stage: the registrar shape
// the assembly fills and a host hands to Wire.
func wireTestRegistry(t *testing.T, declares ...func(*pkgcore.ComponentRegistry) error) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := componenttest.NewRegistry()
	if err := componenttest.DeclareAll(reg, declares...); err != nil {
		t.Fatalf("declare into the registry: %v", err)
	}
	return reg
}

// queueTableExists reports whether table is present on db.
func queueTableExists(t *testing.T, db *gorm.DB, table string) bool {
	t.Helper()
	var n int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n).Error; err != nil {
		t.Fatalf("probe sqlite_master for table %q: %v", table, err)
	}
	return n == 1
}

// echoHandler returns a Handler echoing its Task payload back as the Result.
func echoHandler(jobType string) Handler {
	return NewHandlerFunc(jobType, func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		return Result{Data: job.Payload}, nil
	})
}

// TestWire_RegistersDeclaredHandlersAndCreatesTheQueueTables is Wire's core
// contract: after one call, every handler the registry carries is on the
// queue and both of the queue's tables exist -- with no Start anywhere in
// the test, which is what a worker-disabled replica does.
func TestWire_RegistersDeclaredHandlersAndCreatesTheQueueTables(t *testing.T) {
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)
	reg := wireTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		for _, jobType := range []string{"wire.first", "wire.second"} {
			if err := r.Jobs.Handle(jobType, echoHandler(jobType)); err != nil {
				return err
			}
		}
		return nil
	})

	if err := Wire(context.Background(), q, reg.Jobs); err != nil {
		t.Fatalf("Wire() error = %v", err)
	}

	for _, jobType := range []string{"wire.first", "wire.second"} {
		if q.handlers[jobType] == nil {
			t.Errorf("handler %q is not registered on the queue after Wire", jobType)
		}
	}
	for _, table := range []string{jobsTable, queueWritersTable} {
		if !queueTableExists(t, db, table) {
			t.Errorf("table %q does not exist after Wire", table)
		}
	}
}

// TestWire_DeclaredHandlerRunsEndToEnd drives a full round trip through the
// wiring: declare a handler on the registry, Wire it onto a queue, Start the
// queue, and watch the Task the registry declared reach StatusSucceeded with
// the handler's Result -- the proof that a Wire-registered handler is the
// same object a worker dispatches to.
func TestWire_DeclaredHandlerRunsEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db, WithPollInterval(5*time.Millisecond), WithWorkerCount(1))
	reg := wireTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.Jobs.Handle("wire.echo", echoHandler("wire.echo"))
	})

	if err := Wire(ctx, q, reg.Jobs); err != nil {
		t.Fatalf("Wire() error = %v", err)
	}
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.Close(shutdownCtx); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	id, err := q.Enqueue(ctx, Task{
		Type:     "wire.echo",
		TenantID: pkgcore.TenantID("tenant-a"),
		Payload:  []byte("ping"),
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	deadline := time.Now().Add(5 * time.Second)
	var job *Job
	for {
		job, err = q.Get(tenantCtx, id)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", id, err)
		}
		if job.Status.Terminal() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job.Status != StatusSucceeded {
		t.Fatalf("job status = %s (attempts %d, error %q), want %s", job.Status, job.Attempts, job.Error, StatusSucceeded)
	}
	if got := string(job.Result.Data); got != "ping" {
		t.Errorf("job result = %q, want the handler from the registry to have echoed %q", got, "ping")
	}
}

// TestWire_WithoutDeclaredHandlers_CreatesTheQueueTables pins the
// empty-registry case: a host whose modules declared no asynchronous work
// still gets a usable queue (tables present, no handlers).
func TestWire_WithoutDeclaredHandlers_CreatesTheQueueTables(t *testing.T) {
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)
	reg := wireTestRegistry(t)

	if err := Wire(context.Background(), q, reg.Jobs); err != nil {
		t.Fatalf("Wire() error = %v", err)
	}

	if len(q.handlers) != 0 {
		t.Errorf("queue carries %d handlers, want none", len(q.handlers))
	}
	for _, table := range []string{jobsTable, queueWritersTable} {
		if !queueTableExists(t, db, table) {
			t.Errorf("table %q does not exist after Wire", table)
		}
	}
}

// TestWire_RefusesEntryThatIsNotAHandler pins the refusal contract: an entry
// that is not a jobs.Handler is a wiring bug, reported with the offending
// job type, and -- because the drain runs before the schema step -- no table
// is created for a queue that failed to wire.
func TestWire_RefusesEntryThatIsNotAHandler(t *testing.T) {
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)
	reg := wireTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.Jobs.Handle("wire.bad", "not a handler")
	})

	err := Wire(context.Background(), q, reg.Jobs)
	if err == nil {
		t.Fatal("Wire() = nil, want an error for a registry entry that is not a jobs.Handler")
	}
	if !strings.Contains(err.Error(), "wire.bad") {
		t.Errorf("Wire() error = %q, want it to name the offending job type %q", err, "wire.bad")
	}
	if queueTableExists(t, db, jobsTable) {
		t.Errorf("table %q exists although Wire refused a handler: the drain must run before any schema work", jobsTable)
	}
}

// TestWire_RepeatedCall_IsRefused pins Wire's one-shot contract: the second
// call over the same queue fails with the duplicate-handler refusal (a queue
// already holds the Type), instead of silently re-registering -- and the
// first call's work (handler and tables) is untouched by the refusal.
func TestWire_RepeatedCall_IsRefused(t *testing.T) {
	ctx := context.Background()
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)
	reg := wireTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.Jobs.Handle("wire.once", echoHandler("wire.once"))
	})

	if err := Wire(ctx, q, reg.Jobs); err != nil {
		t.Fatalf("first Wire() error = %v", err)
	}

	err := Wire(ctx, q, reg.Jobs)
	if !apperr.HasCode(err, ErrDuplicateHandlerType.Code) {
		t.Fatalf("second Wire() error = %v, want the duplicate-handler refusal", err)
	}
	if !strings.Contains(err.Error(), "wire.once") {
		t.Errorf("second Wire() error = %q, want it to name the duplicated job type", err)
	}

	if len(q.handlers) != 1 {
		t.Errorf("queue carries %d handlers after the refused re-wiring, want the first call's 1", len(q.handlers))
	}
	for _, table := range []string{jobsTable, queueWritersTable} {
		if !queueTableExists(t, db, table) {
			t.Errorf("table %q does not exist after the refused re-wiring", table)
		}
	}
}

// TestWire_NilArguments pins Wire's refusal of the wiring mistakes that
// would otherwise panic somewhere deeper: a nil queue, a nil registrar and a
// nil context each return an error naming what is missing.
func TestWire_NilArguments(t *testing.T) {
	ctx := context.Background()
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)
	reg := wireTestRegistry(t)

	if err := Wire(ctx, nil, reg.Jobs); err == nil {
		t.Error("Wire() with a nil queue = nil, want an error")
	}
	if err := Wire(ctx, q, nil); err == nil {
		t.Error("Wire() with a nil registrar = nil, want an error")
	}
	var nilCtx context.Context
	if err := Wire(nilCtx, q, reg.Jobs); err == nil {
		t.Error("Wire() with a nil context = nil, want an error")
	}
}
