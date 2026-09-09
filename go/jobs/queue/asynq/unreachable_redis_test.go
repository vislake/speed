package asynq

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the unit-testable half of this subpackage's error
// surface -- what Queue does when its Redis backend cannot be reached --
// driven against a client pointed at a reserved loopback port
// (127.0.0.1:1, which refuses every connection instantly), never a fake:
// the go-redis/asynq clients are not mockable without replacing the very
// seams this package is a thin wrapper around, and faking them would test
// nothing this package ships. Every success path (a real enqueue, a
// real Get, an idempotency dedupe, a real dispatch) requires a live
// Redis and lives in this module's Docker-backed integration tier; what
// the unit suite can honestly exercise is the fail-closed reporting side
// -- every operation answers its wrapped error rather than hanging or
// panicking, and the marker-guarded dispatch logic makes its
// fail-closed-refusal decision from the read's failure without a server.

// deadRedisAddr is the backend every queue in this file is wired to: a
// reserved port that refuses connections instantly, so every command
// fails fast with a connection error and no test ever waits on a dial
// timeout.
const deadRedisAddr = "127.0.0.1:1"

// deadRedisOpt is the asynqlib.RedisConnOpt form of deadRedisAddr.
var deadRedisOpt = asynqlib.RedisClientOpt{Addr: deadRedisAddr}

// captureLogs swaps slog's default for a buffer for the duration of the
// test, capturing every log line this package emits through
// obs.FromContext on a context that carries no logger (all of this
// file's code sites log through context.Background()-rooted contexts).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestNewQueue_WiresSharedClientAndDefaults pins NewQueue's construction
// without any Redis I/O: the shared client, the asynq client/inspector/
// server triad and the documented defaults are all in place, so a queue
// can be built, configured and refused by its backend long before any
// server exists.
func TestNewQueue_WiresSharedClientAndDefaults(t *testing.T) {
	q := NewQueue(deadRedisOpt, WithConcurrency(3))

	if q.rdb == nil || q.client == nil || q.inspector == nil || q.server == nil {
		t.Fatal("NewQueue() left client/inspector/server/rdb nil, want the shared wiring in place")
	}
	if q.tenantConcurrency != DefaultTenantConcurrencyLimit {
		t.Errorf("tenantConcurrency = %d, want %d", q.tenantConcurrency, DefaultTenantConcurrencyLimit)
	}
	if q.defaultTimeout != jobs.DefaultTimeout {
		t.Errorf("defaultTimeout = %v, want %v", q.defaultTimeout, jobs.DefaultTimeout)
	}
	if q.completedRetention != DefaultCompletedRetention {
		t.Errorf("completedRetention = %v, want %v", q.completedRetention, DefaultCompletedRetention)
	}
	if q.cancelledRetention != DefaultCancelledRetention {
		t.Errorf("cancelledRetention = %v, want %v", q.cancelledRetention, DefaultCancelledRetention)
	}
	if q.throttleRetryDelay != DefaultThrottleRetryDelay {
		t.Errorf("throttleRetryDelay = %v, want %v", q.throttleRetryDelay, DefaultThrottleRetryDelay)
	}
	if len(q.queueWeights) != 3 {
		t.Errorf("queueWeights = %v, want the three default tiers", q.queueWeights)
	}
}

// nonUniversalRedisConnOpt is an asynqlib.RedisConnOpt whose
// MakeRedisClient returns something that is not a redis.UniversalClient
// -- the unsupported-conn-opt shape NewQueue's construction panic exists
// to refuse loudly at construction time rather than inside asynq's own
// constructors.
type nonUniversalRedisConnOpt struct{}

func (nonUniversalRedisConnOpt) MakeRedisClient() interface{} { return 42 }

// TestNewQueue_UnsupportedRedisConnOpt_Panics pins that refusal.
func TestNewQueue_UnsupportedRedisConnOpt_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewQueue(unsupported RedisConnOpt) did not panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "unsupported asynq.RedisConnOpt type") {
			t.Errorf("panic value = %v, want the unsupported-RedisConnOpt message", r)
		}
	}()
	NewQueue(nonUniversalRedisConnOpt{})
}

// TestStartAndClose_UnreachableBackend_CompleteWithoutHanging pins the
// lifecycle's failure behavior against a backend that refuses every
// connection: Start launches asynq's own processor goroutines (which
// simply fail their polls against the dead backend), returns nil, is a
// no-op on a second call, and Close shuts the goroutines and the shared
// client down, idempotently. A queue whose backend vanished must not
// wedge Start or Close -- the operator restarts the backend, not the
// process.
func TestStartAndClose_UnreachableBackend_CompleteWithoutHanging(t *testing.T) {
	q := NewQueue(deadRedisOpt)

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil (asynq's server launch does not depend on a reachable Redis)", err)
	}
	if err := q.Start(context.Background()); err != nil {
		t.Errorf("second Start() error = %v, want nil (no-op after a successful Start)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := q.Close(ctx); err != nil {
		t.Errorf("second Close() error = %v, want nil (idempotent)", err)
	}
}

// TestEnqueue_InvalidTask_RefusedBeforeAnyBackendTouch pins that Enqueue
// validates the task before the queue's own client is ever consulted:
// an invalid task is refused with the validation error even though the
// backend is unreachable -- validation failure must never be reported as
// a backend failure.
func TestEnqueue_InvalidTask_RefusedBeforeAnyBackendTouch(t *testing.T) {
	q := NewQueue(deadRedisOpt)

	_, err := q.Enqueue(context.Background(), jobs.Task{Type: "", TenantID: "tenant-a"})
	if err == nil {
		t.Fatal("Enqueue() with an empty Type error = nil, want the validation error")
	}
	if _, ok := apperr.As(err); !ok {
		t.Errorf("Enqueue() error = %v, want a coded *apperr.Error", err)
	}
}

// TestEnqueue_UnreachableBackend_ReturnsWrappedError pins the two
// Enqueue paths' failure reporting: a fresh task whose insert cannot
// reach Redis and a keyed task whose idempotency claim cannot reach
// Redis each answer their wrapped error -- never a hang, never a
// fabricated id.
func TestEnqueue_UnreachableBackend_ReturnsWrappedError(t *testing.T) {
	q := NewQueue(deadRedisOpt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Enqueue(ctx, jobs.Task{Type: "plain", TenantID: "tenant-a"}); err == nil ||
		!strings.Contains(err.Error(), "jobs: enqueue job") {
		t.Errorf("Enqueue(no key) error = %v, want a wrapped enqueue error", err)
	}

	if _, err := q.Enqueue(ctx, jobs.Task{Type: "keyed", TenantID: "tenant-a", IdempotencyKey: "invoice-1"}); err == nil ||
		!strings.Contains(err.Error(), "claim idempotent enqueue") {
		t.Errorf("Enqueue(idempotency key) error = %v, want a wrapped claim error", err)
	}
}

// TestGetCancelAndDeadLetterJobs_UnreachableBackend_ReturnWrappedErrors
// pins the reporting side's failure answers: a Job lookup that cannot
// reach any of the three priority queues, a Cancel built on the same
// lookup, and a dead-letter listing over unreachable queues each answer
// their wrapped error rather than a fabricated not-found or an empty
// list -- an operator must be able to tell "backend down" from "no such
// job".
func TestGetCancelAndDeadLetterJobs_UnreachableBackend_ReturnWrappedErrors(t *testing.T) {
	q := NewQueue(deadRedisOpt)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := q.Get(ctx, "job-1"); err == nil || !strings.Contains(err.Error(), "jobs: look up job") {
		t.Errorf("Get() error = %v, want a wrapped lookup error", err)
	}
	if err := q.Cancel(ctx, "job-1"); err == nil || !strings.Contains(err.Error(), "jobs: look up job") {
		t.Errorf("Cancel() error = %v, want a wrapped lookup error", err)
	}
	if _, err := q.DeadLetterJobs(ctx); err == nil || !strings.Contains(err.Error(), "list archived jobs in queue") {
		t.Errorf("DeadLetterJobs() error = %v, want a wrapped listing error", err)
	}
}

// TestCancelMarkers_UnreachableBackend_ReportTheReadFailure pins the two
// marker primitives' failure reporting on a bare queue whose rdb is an
// unreachable client: both the write (Cancel's only authoritative
// effect) and the read (Get's fail-closed probe) answer the connection
// error -- never redis.Nil's "no marker", which only a live Redis can
// truthfully answer.
func TestCancelMarkers_UnreachableBackend_ReportTheReadFailure(t *testing.T) {
	q := &Queue{rdb: redis.NewClient(&redis.Options{Addr: deadRedisAddr})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := q.writeCancelMarker(ctx, "job-1"); err == nil {
		t.Error("writeCancelMarker() error = nil, want the connection error")
	}
	if _, err := q.readCancelMarker(ctx, "job-1"); err == nil {
		t.Error("readCancelMarker() error = nil, want the connection error")
	}
}

// TestProcessTask_MarkerReadFailure_RefusesTheRun pins processTask's own
// fail-closed decision end to end without a server: with the marker read
// failing (the dead rdb), the dispatched task is refused with
// errCancelMarkerUnreadable -- never executed, never reported as a clean
// skip.
func TestProcessTask_MarkerReadFailure_RefusesTheRun(t *testing.T) {
	q := &Queue{
		rdb:      redis.NewClient(&redis.Options{Addr: deadRedisAddr}),
		handlers: make(map[string]jobs.Handler),
	}
	buf := captureLogs(t)
	task := asynqlib.NewTaskWithHeaders("marker-refused", nil, map[string]string{headerTenantID: "tenant-a"})

	err := q.processTask(context.Background(), task)

	if !errors.Is(err, errCancelMarkerUnreadable) {
		t.Errorf("processTask() error = %v, want errCancelMarkerUnreadable", err)
	}
	if out := buf.String(); !strings.Contains(out, "cancellation marker unreadable") {
		t.Errorf("processTask() logged: %s, want the unreadable-marker warning", out)
	}
}

// TestHandleError_TerminalMarkerReadFailure_LogsAndRunsTheTerminalPath
// pins handleError's own marker half: for an attempt asynq's dispatch
// loop is about to archive (retried >= maxRetry), the cancellation
// marker read runs on a cancellation-stripped context and its failure is
// logged -- the attempt still converges through the ordinary terminal
// machinery, since the archive itself is asynq's own next step. With a
// bare context asynq's own accessors answer zero (retried = maxRetry =
// 0), which is exactly the terminal shape.
func TestHandleError_TerminalMarkerReadFailure_LogsAndRunsTheTerminalPath(t *testing.T) {
	q := &Queue{
		rdb:      redis.NewClient(&redis.Options{Addr: deadRedisAddr}),
		handlers: make(map[string]jobs.Handler),
	}
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	buf := captureLogs(t)
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleError(context.Background(), task, errors.New("attempt failed"))

	if out := buf.String(); !strings.Contains(out, "reading cancellation marker failed") {
		t.Errorf("handleError() logged: %s, want the marker-read-failure warning", out)
	}
	if len(h.calls) != 1 {
		t.Errorf("OnFailure called %d times, want exactly 1 (the terminal archive must still fire the hook)", len(h.calls))
	}
}

// TestProcessTaskUncancelled_MissingTenantHeader_RefusedBeforeAnyWrite
// pins the missing-tenant defense on the dispatch path: a task whose
// stored headers carry no tenant is refused with the coded
// errTaskMissingTenant before the attempt reaches the tenant slot or the
// ResultWriter (whose envelope writes a synthetic task cannot perform) --
// the one full branch of processTaskUncancelled past the handler lookup
// that needs no real dispatch machinery.
func TestProcessTaskUncancelled_MissingTenantHeader_RefusedBeforeAnyWrite(t *testing.T) {
	q := newTestQueue(t)
	handled := false
	if err := q.RegisterHandler(jobs.NewHandlerFunc("missing-tenant", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		handled = true
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	task := asynqlib.NewTaskWithHeaders("missing-tenant", nil, map[string]string{})
	err := q.processTaskUncancelled(context.Background(), task, "job-1", slog.Default())

	if handled {
		t.Error("Handle ran for a task with no tenant header, want the refusal before any execution")
	}
	if err == nil {
		t.Fatalf("processTaskUncancelled() error = nil, want the coded missing-tenant refusal")
	}
	if e, ok := apperr.As(err); !ok || e.Code != errTaskMissingTenant.Code {
		t.Errorf("processTaskUncancelled() error = %v, want code %q", err, errTaskMissingTenant.Code)
	}
}
