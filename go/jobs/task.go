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
	//
	// The queue PERSISTS Payload with the Job's record and keeps it for
	// the record's whole lifetime -- forever on StandaloneQueue, whose job
	// rows are never deleted -- readable back by any caller that can Get
	// the Job (the owning tenant's own code, or a system context) and by
	// the Handler on every attempt. Never put credentials, bearer tokens
	// or personal data here: this package cannot tell a secret from
	// harmless bytes and will store either without complaint. The
	// precedent is real -- go/admin's audit-export job once carried a
	// one-time sharing delivery token in a job result (the P1-B finding,
	// since fixed) before review caught it: a token that must be usable
	// exactly once is a credential, and a credential at rest in a job
	// record is exactly the leak this warning exists to prevent. Wherever
	// the data is sensitive, Payload should carry a reference to it (an
	// id, an object key), not the data itself.
	Payload []byte

	// IdempotencyKey, when non-empty, makes Enqueue idempotent: a second
	// Enqueue call for the same (TenantID, IdempotencyKey) pair returns
	// the JobID of the Job already created for the first call, without
	// creating a second row or ever invoking Handle a second time for it —
	// for as long as the first Job's record lives. On StandaloneQueue that
	// lifetime is unconditional: the row is never deleted, so the dedupe
	// holds forever, regardless of what that first Job's outcome was,
	// including a StatusDeadLetter one (see AGENTS.md for why this is
	// unconditional rather than conditioned on the existing Job's outcome).
	// On the asynq-backed Queue the record's lifetime is bounded by asynq's
	// own retention and eviction windows, so a duplicate Enqueue arriving
	// after the first Job's record has expired creates a NEW, independent
	// Job instead of returning the original's id — the two implementations'
	// dedupe answers agree while the first Job's record exists and differ
	// only after it is gone (AGENTS.md's "Idempotency" section states the
	// exact windows). One consequence a caller must design around: a key
	// names ONE business-operation instance, so a task whose operation
	// repeats over time — a periodic sweep, say — must scope its key to the
	// period it is for (the storage and compliance sweep keys carry their
	// window start), or the dedupe intended to collapse concurrent
	// duplicates of one run will instead collapse every later run into the
	// first-ever one.
	//
	// "Derived from the business operation", the phrase this field's
	// AGENTS.md example uses, means deterministic — a replay of the same
	// operation reproduces the key, which is what lets a duplicate
	// Enqueue dedupe onto the first Job's id. It says nothing about
	// secrecy, and unlike Payload and Result — whose do-not-put-
	// credentials warnings this one mirrors — the key is not opaque
	// bytes: the asynq-backed Queue composes it verbatim into its
	// deterministic TaskID ("idem:" + tenantID + ":" + key), which IS
	// the JobID that queue's enqueue and claim-recovery log lines carry
	// in their job_id attribute, and the text is persisted with the Job's
	// own record for the record's whole lifetime on both queues —
	// StandaloneQueue's idempotency_key column, the asynq-backed queue's
	// TaskID and task headers. A key that embeds an email address, a
	// phone number or a token therefore puts that text into the
	// platform's logs and job records verbatim, and no redaction layer
	// can recognize it for removal — go/observability's value-shape net
	// catches credential shapes, never PII, and job_id is exempt from
	// even that scan so log lines stay joinable. Build the key from
	// the operation's own opaque identifiers, exactly as the AGENTS.md
	// example shows; where the
	// operation's only natural identity is a PII-bearing value (an
	// invite addressed to an email, say), hash that value into the key
	// rather than embedding it.
	//
	// Left empty, every Enqueue call creates a new, independent Job.
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
