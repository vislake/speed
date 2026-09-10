package smilesim

import (
	"context"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// OnJobTerminal is the Service's subscriber to the queue's terminal signal
// (jobs.EventJobTerminal, payload jobs.JobTerminalEvent -- see that event's
// own doc comment for the delivery contract it carries). internal/app
// installs it on the app's event bus at assembly time
// (wireSmilesimTerminalSignal), and the queue publishes it the moment a Job
// reaches a terminal status, so both halves of a simulation's completion
// happen at the transition itself rather than at the next client poll:
//
//   - the job's credit reservation is settled by the very settleCredit
//     NotifyOnCompletion's poll-driven leg and the reconciliation sweep
//     call -- same semantics, same idempotency, and a job with no
//     reservation row on file (any job this Service never reserved for,
//     including every other task type this app's queue runs) is a no-op;
//   - a job Simulate was given a recipient for publishes
//     EventSimulationCompleted through the shared latch
//     (publishCompletionEvent), so the notification no longer depends on a
//     client happening to poll; the poll route's own call stays installed
//     as the second leg.
//
// The event's facts are authoritative for the settlement, deliberately: the
// payload's terminal Status decides Confirm (StatusSucceeded) versus Refund
// (StatusDeadLetter/StatusCancelled) and Event.TenantID supplies the tenant
// settleCredit rebuilds its context from, exactly as the *jobs.Job the poll
// path passes does -- never a read-back of the Job row, which the consumer
// contract says a subscriber must not depend on having reached its terminal
// state by the time the event arrives (the payload's CompletedAt pins why
// the event can outrun that write). A status outside the three terminal
// states is not this signal's shape and is dropped with a warning rather
// than settled: Refund's "anything but StatusSucceeded" branch must never
// see a non-outcome. An unreadable payload is dropped the same way.
//
// The notification half does read the row back -- one Queue.Get, for a
// succeeded job only -- because the payload deliberately carries no Result
// body, and that read is what fills the completion event's OutputObjectID
// the way the poll path's own job does. A read-back failure is logged and
// swallowed: the completion event still publishes, without the output
// object id, rather than hold the notification hostage to a detail.
//
// Errors are returned, never swallowed: a failed settlement and a refused
// publish both propagate to the publisher, which on the standalone queue
// leaves the terminal row owed and republishes it on the next pass -- the
// retry that lets a refused publish land later, the dual of the fresh
// attempt a later poll gives the poll-driven leg. Both halves are
// idempotent under that retry (settleCredit's compare-and-set, the latch's
// claim-and-rollback), so a duplicate signal is absorbed like any repeat.
//
// Everything here is durable bookkeeping for a job that is already
// terminal, so it runs on a cancel-free derivation of ctx -- the identical
// context.WithoutCancel boundary NotifyOnCompletion draws.
func (s *Service) OnJobTerminal(ctx context.Context, evt pkgcore.Event) error {
	var terminal jobs.JobTerminalEvent
	if err := pkgcore.DecodeEventPayload(evt.Payload, &terminal); err != nil {
		obs.FromContext(ctx).Warn("smilesim: dropping a jobs.job.terminal event with an unreadable payload",
			"event_type", evt.Type, "error", err)
		return nil
	}
	if !terminal.Status.Terminal() {
		obs.FromContext(ctx).Warn("smilesim: dropping a jobs.job.terminal event whose status is not terminal",
			"event_type", evt.Type, "job_id", string(terminal.JobID), "status", string(terminal.Status))
		return nil
	}

	persistCtx := context.WithoutCancel(ctx)
	job := &jobs.Job{
		ID:       terminal.JobID,
		TenantID: evt.TenantID,
		Status:   terminal.Status,
	}

	if err := s.settleCredit(persistCtx, job); err != nil {
		return err
	}

	if job.Status == jobs.StatusSucceeded && s.queue != nil {
		if row, getErr := s.queue.Get(pkgcore.WithTenant(persistCtx, job.TenantID), job.ID); getErr != nil {
			obs.FromContext(ctx).Warn("smilesim: reading a succeeded job back for its result failed; publishing the completion event without the output object id",
				"job_id", string(job.ID), "error", getErr)
		} else {
			job.Result = row.Result
		}
	}

	return s.publishCompletionEvent(persistCtx, job)
}
