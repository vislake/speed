package jobs

import "time"

// EventJobTerminal is the routing key of jobs.job.terminal, the domain event
// both Queue implementations publish on the bus they were configured with
// (StandaloneQueue.WithEventBus / asynq.WithEventBus) whenever one of their
// Jobs reaches a terminal Status. The payload is JobTerminalEvent. It is a
// single event type carrying the terminal status inside the payload, rather
// than one type per status: Status is the field that says which of the three
// terminal states the Job settled into.
//
// The event is the queue's terminal signal, and nothing more: it turns "how
// do I learn a Job ended" from a poll into a subscription. It is NOT failure
// compensation, and the queue still interprets no task type -- the payload
// is a set of facts about the Job, and what a subscriber does with them is
// the business module's own concern (the signal-side dual of "compensation
// belongs to the business module"; see FailureHook's doc comment for the
// boundary between the two surfaces). jobs is not a pkgcore.Module, so it
// does not declare this type into the registry's event catalog: the catalog
// is a documentation-and-mapping contract, not a precondition for
// publishing (pkgcore.EventRegistrar.Publishes), and the type's definition
// ships here as this exported constant plus the documentation on this file.
//
// Consumer contract -- the three clauses every subscriber is written
// against:
//
//  1. Delivery is at least once, never exactly once. Duplicate events for
//     one terminal transition are expected: a StandaloneQueue stamp write
//     that failed after a successful publish makes the next publish pass
//     republish the row, a crash between an asynq publish and asynq's own
//     completion/archive write can reach terminal a second time on
//     redelivery, and the broker-backed buses may drop a failed subscriber's
//     delivery by design (each EventBus implementation's own docs). A
//     subscriber MUST be idempotent.
//
//  2. The Job row is the truth; the event is a notification. A subscriber
//     that needs the Job's data reads it back through Queue.Get (or its own
//     durable records) and must accept duplicates and absences -- and on the
//     asynq leg it must not depend on the row having reached its terminal
//     state by the time the event arrives: asynq.Queue publishes before
//     asynq's own archive/completion write, so a read-back may still observe
//     StatusRunning (the same ordering discipline FailureHook's doc comment
//     pins for OnFailure on that implementation).
//
//  3. A subscriber that needs completeness keeps its own reconciliation
//     net. The event shortens the delay from "the next sweep" to "at the
//     transition"; it does not replace a consumer's own sweep over its
//     durable records, which the bus implementations' documented drop
//     behavior, a subscriber's own downtime, and the asynq leg's bounded
//     retention (see AGENTS.md's Known limitations) all require.
//
// Event.TenantID is copied from the Job row's own TenantID -- the same
// field a worker rebuilds a Handle context from, and the only tenant fact a
// background publish pass has (nothing about the enqueuing context survives
// to publish time). A platform-scoped task carries its platform sentinel as
// that value (for example "_pki_platform_scan"), and a subscriber must not
// resolve a sentinel as a real tenant.
//
// The payload deliberately carries no Result body: the event is a
// notification and the row is the data plane, so a success subscriber reads
// Result back through Queue.Get like every other field (clause 2 above).
const EventJobTerminal = "jobs.job.terminal"

// JobTerminalEvent is the payload of an EventJobTerminal event: the facts of
// one Job's terminal transition, and never anything the queue interprets.
type JobTerminalEvent struct {
	// JobID is the terminal Job's id, the value Queue.Get and Queue.Cancel
	// resolve.
	JobID JobID `json:"job_id"`

	// JobType is the Job's task type -- the Handler.Type it dispatched to,
	// the value Job.Type reports. Deliberately NOT the event's own routing
	// key, which is always EventJobTerminal.
	JobType string `json:"job_type"`

	// Status is the terminal status the Job reached: StatusSucceeded,
	// StatusDeadLetter or StatusCancelled.
	Status Status `json:"status"`

	// Error is the Job's recorded failure message: the message that
	// exhausted its retries for StatusDeadLetter, the last failed attempt's
	// message for a Job cancelled after a failed attempt (kept for
	// diagnostic visibility, exactly as Job.Error keeps it), and empty for
	// StatusSucceeded. Like Job.Error, it carries err.Error() text for
	// operator visibility, not a structured *apperr.Error.
	Error string `json:"error"`

	// Attempts is the number of Handle attempts the Job consumed, mirroring
	// Job.Attempts for the Job's own terminal state.
	Attempts int `json:"attempts"`

	// CompletedAt is the moment the Job reached its terminal state. For
	// StatusSucceeded and StatusDeadLetter it is the persisted completion
	// moment. For StatusCancelled -- a transition whose write records no
	// completed_at -- it is the moment the cancellation was recorded: the
	// row's updated_at on StandaloneQueue, which no later write touches
	// (markCancelled stamps updated_at and the terminal row is never written
	// again), and the cancellation marker's timestamp on asynq.Queue, the
	// exact moment the marker write recorded. This pin is what keeps the
	// field's meaning identical on both implementations, and it is never
	// zero -- every terminal status has a moment to report.
	CompletedAt time.Time `json:"completed_at"`
}
