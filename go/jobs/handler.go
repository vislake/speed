package jobs

import "context"

// ProgressFn reports incremental progress from inside a Handle call. pct is
// a caller-defined percentage (0-100 is the expected convention, though
// this package does not enforce the range) and msg is a short,
// human-readable status string. Each call overwrites the Job's previously
// reported progress — see StandaloneQueue's own doc comment for how it persists
// this, so a caller polling Queue.Get observes it.
type ProgressFn func(pct int, msg string)

// Handler processes every Job of one Task Type.
type Handler interface {
	// Type identifies which Task.Type this Handler processes. It must be
	// stable for the lifetime of the Handler and unique within one Queue —
	// see StandaloneQueue.RegisterHandler.
	Type() string

	// Handle runs one attempt of job. ctx already carries job.TenantID via
	// pkgcore.WithTenant, rebuilt by the worker from the Job's own stored
	// tenant before this call — never inherited from whatever context the
	// original Queue.Enqueue call happened to run in, which no longer
	// exists by the time a worker picks the Job up (see AGENTS.md's "The
	// tenant context trap"). A Handler implementation may call
	// pkgcore.WithTenant itself too — harmless, it would set the same
	// value again — but does not need to.
	//
	// Returning a nil error marks job StatusSucceeded with result
	// recorded. Returning a non-nil error marks it StatusRetrying (if
	// attempts remain) or StatusDeadLetter (if not) — see AGENTS.md's
	// retry/backoff section. Handle should respect ctx's cancellation
	// (which fires when the Job's configured timeout elapses) and return
	// promptly once it does.
	Handle(ctx context.Context, job *Job, progress ProgressFn) (Result, error)
}

// handlerFunc adapts a plain function into a Handler, mirroring
// http.HandlerFunc's role for http.Handler.
type handlerFunc struct {
	jobType string
	fn      func(ctx context.Context, job *Job, progress ProgressFn) (Result, error)
}

// Type implements Handler.
func (h handlerFunc) Type() string { return h.jobType }

// Handle implements Handler.
func (h handlerFunc) Handle(ctx context.Context, job *Job, progress ProgressFn) (Result, error) {
	return h.fn(ctx, job, progress)
}

// NewHandlerFunc adapts fn into a Handler for jobType, for a caller (often
// a test) that does not need a dedicated named type.
func NewHandlerFunc(jobType string, fn func(ctx context.Context, job *Job, progress ProgressFn) (Result, error)) Handler {
	return handlerFunc{jobType: jobType, fn: fn}
}

// compile-time check that handlerFunc satisfies Handler.
var _ Handler = handlerFunc{}

// FailureHook is implemented by a Handler that needs to run
// business-specific compensation once a Job exhausts its retries and moves
// to StatusDeadLetter — refunding a pay-per-use credit reservation, for
// example. This is the queue's ENTIRE failure-compensation surface, by
// design: root CLAUDE.md's "Asynchronous work" discipline states "the
// queue offers an OnFailure hook; refunding credits and similar
// compensation belongs to the business module." jobs itself never inspects
// a Job's business meaning and never runs compensation logic of its own —
// a Handler that needs compensation implements this interface itself,
// alongside Handler, and the queue calls it as a hook, nothing more.
//
// OnFailure runs at most once per Job, on the final attempt's failure path
// only, and never for a Job a concurrent Cancel already moved to
// StatusCancelled while that final attempt was executing: the cancellation
// wins, the attempt's failure outcome is discarded in favor of
// StatusCancelled (Queue.Cancel's own doc comment), and no compensation
// runs. Both Queue implementations consult their own cancellation state at
// the failure-processing point before invoking this hook -- StandaloneQueue
// through completeDeadLetter's transition report, go/jobs/queue/asynq's
// Queue through its cancellation marker (see the mode-by-mode bullets
// below for what each one guarantees about the dead-letter record of a
// cancelled Job). Whatever OnFailure does is not retried or otherwise
// observed by the queue. OnFailure receives the same rebuilt tenant context
// Handle itself receives. A panic inside OnFailure is recovered and logged
// by BOTH queue implementations (this module's own worker.go's
// invokeOnFailure, and queue/asynq/worker.go's invokeOnFailure — asynq's
// library recover covers the handler call only, never its ErrorHandler,
// which is where a hook panic would otherwise escape and crash the whole
// process), never allowed to crash the worker process: a buggy hook may
// fire compensation wrongly, but it cannot take every other tenant's
// in-flight and queued Jobs down with it. Write hooks against the WEAKER
// of the two ordering guarantees below; the panic containment is identical
// on both.
//
// The two deployment modes' Queue implementations do NOT give OnFailure the
// identical ordering guarantee relative to dead-letter persistence, and a
// FailureHook must be written for the weaker of the two:
//
//   - StandaloneQueue runs OnFailure strictly AFTER its dead-letter write has
//     actually persisted job's Status as StatusDeadLetter — the final
//     attempt's failure path only invokes it once that write really
//     transitioned the row from StatusRunning (worker.go's execute consults
//     completeDeadLetter's transition report first). The no-transition case
//     is exactly a concurrent Cancel: the row stays StatusCancelled, no
//     dead-letter is ever persisted for a cancelled Job, and no OnFailure
//     runs. A FailureHook may safely read the Job back through Queue.Get
//     from inside OnFailure here and observe StatusDeadLetter.
//   - go/jobs/queue/asynq's Queue runs OnFailure BEFORE that same
//     information is durable: asynq's own archival write (its dead-letter
//     equivalent, the "archived" state) happens inside the library's own
//     dispatch loop strictly after the registered ErrorHandler — this
//     package's own hook point — already returned, with no separate
//     post-archive callback asynq exposes to reorder around (see
//     go/jobs/queue/asynq/AGENTS.md's "FailureHook has no direct asynq
//     equivalent to hook into" section, and its worker.go's handleError doc
//     comment, for the mechanism). A FailureHook that reads its own Job back
//     through Queue.Get from inside OnFailure under this implementation
//     observes StatusRunning, not StatusDeadLetter — a FailureHook must
//     therefore never depend on its own job's dead-letter persistence
//     having already happened by the time OnFailure runs, and must instead
//     treat the OnFailure call itself, not a Get() read-back, as its one
//     and only "this Job has failed for good" signal.
//
// The cancel-wins guarantee holds under asynq's Queue the same way it does
// under StandaloneQueue — handleError (queue/asynq/worker.go) reads the
// cancellation marker Cancel wrote before invoking OnFailure, and Cancel
// (queue/asynq/queue.go) writes that marker BEFORE sending its best-effort
// CancelProcessing interruption signal, so every failure-processing path
// that signal itself triggers observes the cancellation as already durable.
// What differs is the cancelled Job's dead-letter record: asynq's own
// dispatch loop may still archive the underlying task record strictly after
// the ErrorHandler returns, so the raw task can land in asynq's archive —
// visible only through asynq's own Inspector/asynqmon, never through this
// package's API, since Get()/DeadLetterJobs keep reporting StatusCancelled
// for it from the very marker that silenced OnFailure.
type FailureHook interface {
	OnFailure(ctx context.Context, job *Job, cause error)
}
