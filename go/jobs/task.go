package jobs

import (
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Task is what a caller hands to Queue.Enqueue: everything needed to
// create a new Job.
type Task struct {
	// Type selects the Handler that will process this Task. It must match
	// some Handler's Type() exactly. Enqueue does not validate this
	// up front against the registered set — a Task may legitimately be
	// enqueued before its Handler registers, or from a process that never
	// registers any Handler at all — only the worker that later claims the
	// resulting Job needs one. A worker that finds no Handler registered
	// for Type when it claims the Job fails that attempt exactly like any
	// other Handle failure — see ErrHandlerNotRegistered.
	Type string

	// TenantID is the tenant the resulting Job belongs to. It is a field
	// on Task, not resolved from Enqueue's ctx, because Enqueue is
	// legitimately called from contexts with no single ambient tenant — a
	// platform-level scheduler enqueuing one cleanup Task per tenant in a
	// loop, for example — mirroring pkgcore.Event.TenantID's identical
	// reasoning. It must be non-empty: per
	// docs/internal/07-platform-services.md, every Task must carry a
	// tenant, and Enqueue rejects one that does not (ErrInvalidTask).
	TenantID pkgcore.TenantID

	// Payload is this Task's opaque input, already serialized by the
	// caller (typically json.Marshal of a request-specific struct). The
	// queue never interprets it; only the Handler registered for Type
	// does, by deserializing Job.Payload itself.
	Payload []byte

	// IdempotencyKey, when non-empty, makes Enqueue idempotent: a second
	// Enqueue call for the same (TenantID, IdempotencyKey) pair returns
	// the JobID of the Job already created for the first call, without
	// creating a second row or ever invoking Handle a second time for it —
	// regardless of what that first Job's outcome was, including a
	// StatusDeadLetter one. See AGENTS.md for why this is unconditional
	// rather than conditioned on the existing Job's outcome. Left empty,
	// every Enqueue call creates a new, independent Job.
	IdempotencyKey string
}

// ErrInvalidTask is returned by Enqueue when task fails validation: an
// empty Type or an empty TenantID.
var ErrInvalidTask = apperr.Invalid("jobs.invalid_task")

// Validate reports ErrInvalidTask when t is missing a required field.
// Exported so the queue/asynq subpackage's Queue.Enqueue can run the exact
// same check StandaloneQueue.Enqueue does, rather than a second
// hand-maintained copy of it.
func (t Task) Validate() error {
	switch {
	case t.Type == "":
		return ErrInvalidTask.WithParam("field", "type")
	case t.TenantID == "":
		return ErrInvalidTask.WithParam("field", "tenant_id")
	default:
		return nil
	}
}
