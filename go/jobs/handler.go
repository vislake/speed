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
// OnFailure runs at most once per Job, strictly after job's Status has
// already been persisted as StatusDeadLetter, on the same rebuilt tenant
// context Handle itself receives. Whatever OnFailure does is not retried or
// otherwise observed by the queue.
type FailureHook interface {
	OnFailure(ctx context.Context, job *Job, cause error)
}
