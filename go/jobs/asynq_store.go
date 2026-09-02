package jobs

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/hibiken/asynq"

	"github.com/vislake/speed/go/pkgcore"
)

// This file holds AsynqQueue's pure encode/decode and mapping helpers: how
// our Task/Job metadata rides inside asynq's own Task.Headers and
// ResultWriter-backed Result bytes, and how an *asynq.TaskInfo (or a
// mid-Handle *asynq.Task plus its ctx-carried metadata) translates into our
// pinned *Job shape. None of these functions touch Redis -- see
// asynq_store_test.go for their (non-integration) unit tests, and
// asynq_queue.go / asynq_worker.go for the Redis-touching code that calls
// them.

// Header keys asynq's own Task.Headers carries our metadata under. Headers
// are part of base.TaskMessage (see asynq's internal/base package) and
// survive retries and archiving unchanged -- confirmed by reading
// processor.go's retry()/handleFailedMessage(), which reconstruct the Task
// via NewTaskWithHeaders(msg.Type, msg.Payload, msg.Headers) on every
// attempt. This is why they are the right place for tenant_id and
// idempotency_key, rather than folding them into Payload (which must stay
// exactly the caller's own opaque bytes, per Task.Payload's and Job.
// Payload's doc comments) or inventing a parallel Redis structure.
const (
	headerTenantID       = "tenant_id"
	headerIdempotencyKey = "idempotency_key"
	headerCreatedAt      = "created_at" // RFC3339Nano; see asynq.TaskInfo has no creation-time field of its own.
)

// The three fixed asynq queues our Priority levels map onto, ordered
// highest-priority first. This set is intentionally small and static (see
// AGENTS.md's "Priority maps onto three fixed asynq queues" section for why
// a queue-per-tenant or queue-per-arbitrary-priority scheme is not used):
// asynq's own processor iterates its configured queue list on every dequeue
// poll, so the queue count must stay O(1), and a Get/Cancel call that does
// not know which queue a JobID landed in has to probe each of them in turn
// (findTaskInfo, asynq_queue.go) -- a fan-out that only stays cheap because
// there are exactly three.
const (
	asynqQueueCritical = "critical"
	asynqQueueDefault  = "default" // == asynq's own base.DefaultQueueName; also asynq's own Server default when Config.Queues is left nil.
	asynqQueueLow      = "low"
)

// asynqPriorityQueues lists every queue AsynqQueue ever enqueues into or
// reads from, in the fixed order findTaskInfo and DeadLetterJobs probe them.
var asynqPriorityQueues = [3]string{asynqQueueCritical, asynqQueueDefault, asynqQueueLow}

// queueForPriority maps a Priority to one of asynqPriorityQueues.
// Intermediate values collapse to "default" -- see AGENTS.md's documented
// difference from DemoQueue's fully continuous ScheduledAt/Priority
// ordering (asynq's own priority model is three weighted queues, not a
// per-job numeric sort key; see docs/internal/07-platform-services.md and
// server.go's Config.Queues doc comment for the weighted-queue design this
// mirrors).
func queueForPriority(p Priority) string {
	switch {
	case p >= PriorityHigh:
		return asynqQueueCritical
	case p <= PriorityLow:
		return asynqQueueLow
	default:
		return asynqQueueDefault
	}
}

// priorityForQueue is queueForPriority's approximate inverse, used only to
// populate Job.Priority when reporting a Job back through Get/DeadLetterJobs.
// Because queueForPriority is a many-to-one collapse (any Priority strictly
// between PriorityLow and PriorityHigh also lands in "default"), this can
// only recover the REPRESENTATIVE value for each bucket, not necessarily the
// exact int originally passed to WithPriority -- e.g. Priority(7) is
// enqueued into "default" and reported back as PriorityNormal (5), not 7.
// This is the same "coarser than demo" trade-off documented on
// queueForPriority; see AGENTS.md.
func priorityForQueue(queue string) Priority {
	switch queue {
	case asynqQueueCritical:
		return PriorityHigh
	case asynqQueueLow:
		return PriorityLow
	default:
		return PriorityNormal
	}
}

// buildTaskHeaders assembles the Headers map Enqueue attaches to every
// asynq.Task: the metadata our Job/Task contract needs that asynq's own
// Task/TaskInfo shape has no field for (TenantID, IdempotencyKey) or never
// exposes at all (a task's original creation time -- TaskInfo has no
// CreatedAt field; see asynqJobFromTaskInfo below).
func buildTaskHeaders(task Task, createdAt time.Time) map[string]string {
	return map[string]string{
		headerTenantID:       string(task.TenantID),
		headerIdempotencyKey: task.IdempotencyKey,
		headerCreatedAt:      strconv.FormatInt(createdAt.UnixNano(), 10),
	}
}

// headerCreatedAtTime parses the header buildTaskHeaders wrote back into a
// time.Time. A missing or unparseable value (never expected for a Task this
// package's own Enqueue created, but possible for a Redis instance carrying
// tasks enqueued by some other, non-jobs asynq client) reports the zero
// time rather than an error -- CreatedAt is a reporting field, not something
// any code path branches on.
func headerCreatedAtTime(headers map[string]string) time.Time {
	raw, ok := headers[headerCreatedAt]
	if !ok {
		return time.Time{}
	}
	nanos, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}

// asynqResultEnvelope is what every ResultWriter.Write call in this package
// actually writes, and what asynqJobFromTaskInfo decodes back out of
// TaskInfo.Result. It carries three things a *Job needs that asynq's own
// per-task Redis hash has no field for: the current attempt's start time
// (StartedAt), and the caller-reported progress (ProgressPct/ProgressMsg) --
// see AGENTS.md's "Progress reporting" section for why ResultWriter, a
// mechanism asynq documents for exactly this kind of mid-execution write, is
// used instead of a bespoke Redis key. Data is populated only on the final,
// success-path write (mirrors Job.Result's own "set once Status is
// StatusSucceeded; nil otherwise").
type asynqResultEnvelope struct {
	StartedAt   int64  `json:"started_at,omitempty"` // UnixNano; 0 means unset.
	ProgressPct int    `json:"progress_pct,omitempty"`
	ProgressMsg string `json:"progress_msg,omitempty"`
	Data        []byte `json:"data,omitempty"`
}

// encodeResultEnvelope marshals env. Errors are impossible for this
// concrete, JSON-safe struct shape (no channels/funcs/cyclic pointers), so
// callers (asynq_worker.go) treat a returned error as an assertion failure
// worth logging, never a reason to change control flow.
func encodeResultEnvelope(env asynqResultEnvelope) ([]byte, error) {
	return json.Marshal(env)
}

// decodeResultEnvelope parses data written by encodeResultEnvelope. Empty or
// malformed data (a task that never called progress() and has not yet
// succeeded, so ResultWriter.Write was never called at all -- TaskInfo.
// Result is nil in that case) decodes as the zero envelope rather than an
// error: every field of Job that reads from this envelope already treats
// its own zero value as "not reported yet" (ProgressPct/ProgressMsg per
// their own doc comment; StartedAt via asynqJobFromTaskInfo's nil check).
func decodeResultEnvelope(data []byte) asynqResultEnvelope {
	if len(data) == 0 {
		return asynqResultEnvelope{}
	}
	var env asynqResultEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return asynqResultEnvelope{}
	}
	return env
}

// attemptsFromTaskInfo computes Job.Attempts ("the number of times a worker
// has invoked Handle for this Job so far, including the current or most
// recent one") from an *asynq.TaskInfo. This is NOT simply Retried+1 for
// every state -- asynq's own retry/archive bookkeeping (processor.go's
// handleFailedMessage, and internal/rdb/rdb.go's Retry/Archive Lua scripts)
// increments Retried only on a FAILURE that leads to a *retry*, never on the
// dequeue that starts a new attempt, and never on the failure that leads
// straight to archiving:
//
//   - Pending / Scheduled / Aggregating: never dequeued yet, Retried is
//     always 0 and no attempt has been made -- Attempts = 0, matching a
//     freshly-created DemoQueue Job's Attempts=0 before its first claim.
//   - Active: Retried reflects every PRIOR failed attempt (it does not
//     count the one now in flight) -- Attempts = Retried+1.
//   - Retry: Retried was just incremented by the failure that produced this
//     state, so it already equals the exact number of completed attempts --
//     Attempts = Retried (no +1).
//   - Completed: succeeded without Retried changing on the success path --
//     Attempts = Retried+1, same reasoning as Active.
//   - Archived: the fatal failure that triggered archiving is detected via
//     msg.Retried >= msg.Retry BEFORE Retry() would have incremented it, so
//     archive() never bumps Retried for that last attempt -- Attempts =
//     Retried+1, same as Active/Completed.
//
// See asynq_store_test.go for a table test pinning every case above, and
// AGENTS.md's asynq mapping table for the same reasoning in prose.
func attemptsFromTaskInfo(info *asynq.TaskInfo) int {
	switch info.State {
	case asynq.TaskStateActive, asynq.TaskStateCompleted, asynq.TaskStateArchived:
		return info.Retried + 1
	default: // Pending, Scheduled, Retry, Aggregating.
		return info.Retried
	}
}

// statusFromTaskState maps an asynq.TaskState to our Status. cancelled, when
// true, wins unconditionally regardless of state -- Cancel's own semantics
// (see Queue.Cancel's doc comment and AGENTS.md's asynq mapping) make
// StatusCancelled override whatever the underlying asynq state naturally
// evolves to, exactly mirroring DemoQueue's completeSucceeded/
// completeRetrying/completeDeadLetter's own "WHERE status = running" guard
// that discards a still-in-flight attempt's outcome once Cancel has already
// won the race.
func statusFromTaskState(state asynq.TaskState, cancelled bool) Status {
	if cancelled {
		return StatusCancelled
	}
	switch state {
	case asynq.TaskStateActive:
		return StatusRunning
	case asynq.TaskStateRetry:
		return StatusRetrying
	case asynq.TaskStateCompleted:
		return StatusSucceeded
	case asynq.TaskStateArchived:
		return StatusDeadLetter
	default: // Pending, Scheduled, Aggregating: never dispatched yet.
		return StatusPending
	}
}

// asynqJobFromTaskInfo builds the public *Job Get/DeadLetterJobs report from
// one asynq.TaskInfo. cancelledAt is non-nil when a cancellation marker
// exists for this id (see asynq_queue.go's cancellation-marker helpers) --
// its own timestamp becomes Job.UpdatedAt for a cancelled Job, since asynq
// itself has no "cancelled" state to derive a timestamp from.
func asynqJobFromTaskInfo(info *asynq.TaskInfo, cancelledAt *time.Time) *Job {
	env := decodeResultEnvelope(info.Result)
	createdAt := headerCreatedAtTime(info.Headers)
	cancelled := cancelledAt != nil

	job := &Job{
		ID:             JobID(info.ID),
		Type:           info.Type,
		TenantID:       pkgcore.TenantID(info.Headers[headerTenantID]),
		Payload:        info.Payload,
		IdempotencyKey: info.Headers[headerIdempotencyKey],
		Status:         statusFromTaskState(info.State, cancelled),
		Priority:       priorityForQueue(info.Queue),
		ProgressPct:    env.ProgressPct,
		ProgressMsg:    env.ProgressMsg,
		Attempts:       attemptsFromTaskInfo(info),
		MaxRetries:     info.MaxRetry,
		ScheduledAt:    info.NextProcessAt,
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt, // refined below
	}

	if env.StartedAt != 0 {
		t := time.Unix(0, env.StartedAt).UTC()
		job.StartedAt = &t
	}

	switch job.Status {
	case StatusSucceeded:
		job.Result = &Result{Data: env.Data}
		job.Error = ""
		job.CompletedAt = &info.CompletedAt
		job.UpdatedAt = info.CompletedAt
	case StatusDeadLetter:
		job.Error = info.LastErr
		completedAt := info.LastFailedAt
		job.CompletedAt = &completedAt
		job.UpdatedAt = completedAt
	case StatusCancelled:
		job.CompletedAt = cancelledAt
		job.UpdatedAt = *cancelledAt
		// A Job cancelled after a failed attempt keeps that attempt's
		// message for diagnostic visibility -- Job.Error's own doc comment
		// -- so LastErr (if any) is preserved rather than cleared.
		job.Error = info.LastErr
	case StatusRetrying:
		job.Error = info.LastErr
		if !info.LastFailedAt.IsZero() {
			job.UpdatedAt = info.LastFailedAt
		}
	}

	return job
}
