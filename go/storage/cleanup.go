package storage

// LifecycleService is the module's deletion and expiry runtime: Delete ends
// one object's life, Sweep walks a tenant's rows for everything a periodic
// run must clean up, and EnqueueExpirySweep puts that run on the queue the
// host's scheduler drains.
//
// # The delete protocol
//
// Delete runs the crash-convergent protocol the A5 design fixes: mark the
// row (completed -> deleting), remove the bytes the row names from the
// object store, then remove the rows in one transaction. The mark is the
// protocol's crash point -- once the row reads deleting, its readers already
// see nothing (every read surface serves completed rows only), and every
// later step can be re-run safely by any caller: byte removal is idempotent
// (pkgcore's DeleteObject contract), the derivative-row walk lists what
// still exists, and the row removal reports whether it committed, so a run
// interrupted at any step leaves work that the next run converges, not
// duplicates.
//
// The protocol's race partner is the derive worker, and the derivative
// row's insert is where the two converge, as derive.go's header records:
// the insert is gated in one transaction on the object's own row still
// existing and completed, so a delete that removed the row first wins (the
// gate refuses the insert and the worker drops its bytes), while a delete
// that races the gate blocks on the locked object row until the insert
// commits and its own row removal -- object row first, then derivative
// rows -- then removes what just landed. No window is left to close later.
// Sweep itself is what makes interrupted deletions finish: deleting rows
// exist only because a Delete did not finish, and the sweep's first phase
// re-runs the protocol over each of them.
//
// # The sweep's clock
//
// Sweep reclaims two kinds of unfinished life on one now: uploading rows
// whose upload window closed (their bytes, if any, are never becoming
// readable) and completed rows whose retention deadline passed. Reclaiming
// an upload is safe against a completion racing it only because the upload
// window is enforced at the finalize write itself -- repository.go's
// finalizeUpload refuses a completion that straddles its own window end --
// so a row
// this sweep listed can never complete behind its back: either the
// completion committed before the window closed, in which case the row is
// completed and no longer matches the reclaim listing, or the write is
// refused and the row is reclaimed. A transfer
// whose store write interleaves with the reclaim converges on its own side:
// Upload re-reads the row after its put, and Complete's lost-finalize
// branch re-reads after its sanitizer writeback, and whichever finds the
// row reclaimed (or the window closed) takes its own bytes back before
// answering storage.content_missing (object.go) -- a reclaim never inherits
// a late write under the key it just emptied, and a lost finalize never
// leaves its writeback behind. Uploads are reclaimed without an event:
// nothing ever read them, so no subscriber has anything to forget. Deleted
// completed objects announce EventObjectDeleted exactly like a Delete call.
//
// # The expiry-sweep task
//
// EnqueueExpirySweep is the host-facing enqueue for one tenant's sweep, and
// expirySweepHandler is the jobs.Handler claiming the task on the queue.
// The task is tenant-scoped because every query the sweep runs is: one task
// exists per tenant -- the registered declaration expands through the
// host's TenantLister, and a manual EnqueueExpirySweep call names its own
// tenant -- and the tenant rides in the task's TenantID field, rebuilt
// into context by the worker before Handle runs, never inherited from the
// enqueuing side.
// LifecycleService sits at the same tier as ObjectService and DeriveService:
// inert until Module.Register attaches the registry, failing closed with
// ErrStoreUnavailable on any seam it needs before then.

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// taskTypeExpirySweep names the jobs queue task a sweep run is enqueued
// under. The task carries no payload -- the sweep takes its inputs from the
// rows and the clock at run time -- and its handler is expirySweepHandler,
// backed by LifecycleService.
const taskTypeExpirySweep = "storage.expiry_sweep"

// expirySweepWindowSize is the period one expiry-sweep idempotency key
// covers: a sweep is enqueued under the key of the expirySweepWindowSize
// window (expirySweepWindowStart) its enqueue falls in, so the same-window
// duplicates the original key existed to collapse -- a scheduler with two
// replicas, a manual re-run -- still merge into one job, while an enqueue
// in a later window becomes a NEW job and the sweep runs again. The window
// is what makes the sweep periodic at all: jobs' idempotency is
// unconditional for one key on StandaloneQueue (a resolved key is held
// forever), so a tenant-only key would give each tenant exactly one sweep
// per database file -- the pre-window design's recorded residual -- and,
// worse, a sweep job that dead-letters would poison its tenant forever,
// since every later enqueue would keep returning the dead job's id. A
// dead-lettered job now poisons only its own window; the next window's
// enqueue is a fresh key and runs. One hour means an expired object is
// reaped at most expirySweepWindowSize after the sweep that should have
// caught it was enqueued -- well inside the day-scale horizons retention
// deadlines are declared in -- while keeping the sweep load at one task
// per tenant per hour at most.
const expirySweepWindowSize = time.Hour

// expirySweepWindowStart is the expiry-sweep window the enqueue at now
// belongs to -- the absolute hour boundary now.Truncate(expirySweepWindowSize)
// lands in. Two replicas enqueuing within the same window share one key
// (and one job); a tick in a later window gets its own. Truncation is on
// the absolute clock, never a timezone-local calendar cut, so every
// replica agrees on the boundary regardless of its own location.
func expirySweepWindowStart(now time.Time) time.Time {
	return now.Truncate(expirySweepWindowSize)
}

// expirySweepKeyPrefix is the prefix of every expiry-sweep idempotency
// key. It is a named constant because two derivations must agree on it
// byte for byte: expirySweepIdempotencyKey below, and the declaration
// (expirySweepSchedule) a jobs.Scheduler composes keys from with its own
// window derivation -- one window must resolve one key through both
// paths.
const expirySweepKeyPrefix = "storage.sweep:"

// expirySweepIdempotencyKey derives the jobs idempotency key of one
// expiry-sweep window for a tenant, per the rule that an idempotency key
// derives from the business operation, never random: the operation one key
// names is "the sweep of windowStart", not "some sweep or other" -- a
// periodic task's identity inherently includes WHICH period it is for (the
// same reasoning notification's derived keys encode the business
// operation's identity). windowStart is the expirySweepWindowSize window
// start the enqueue belongs to (expirySweepWindowStart). The
// expirySweepKeyPrefix keeps the key inside the module's namespace within
// the shared queue store, and the RFC 3339 window stamp keeps the key
// readable in DeadLetterJobs while staying unambiguous.
func expirySweepIdempotencyKey(tenant pkgcore.TenantID, windowStart time.Time) string {
	return expirySweepKeyPrefix + string(tenant) + ":" + windowStart.UTC().Format(time.RFC3339)
}

// expirySweepSchedule is the module's declaration of the expiry sweep on
// the pkgcore.Registry.Schedules seat: a per-tenant task at the sweep's
// own window, keyed with the same prefix and window function the manual
// EnqueueExpirySweep path uses, so a scheduler tick and a manual enqueue
// landing in one window resolve one key and dedupe onto one job.
var expirySweepSchedule = pkgcore.PeriodicTask{
	Type:      taskTypeExpirySweep,
	Every:     expirySweepWindowSize,
	Scope:     pkgcore.PeriodicScopePerTenant,
	KeyPrefix: expirySweepKeyPrefix,
}

// LifecycleService ends object life: Delete removes one object -- its
// bytes, its derivatives and its rows -- and Sweep walks a tenant's rows
// for everything a periodic run must clean up. It reads and writes through
// the same repositories the transfer runtime uses, is constructed by the
// Module next to ObjectService and DeriveService, and like them is inert
// until Module.Register attaches the registry.
type LifecycleService struct {
	serviceHost

	// objects is the metadata repository the delete protocol's row work
	// runs through, the same instance Module hands ObjectService.
	objects *ObjectRepository
	// derivatives is the repository the protocol's derivative-row listing
	// runs through.
	derivatives *DerivativeRepository
	// queue is the jobs.Queue expiry-sweep tasks are enqueued on, the same
	// instance Module wires for the thumbnail-derive task.
	queue jobs.Queue

	// now is the clock EnqueueExpirySweep reads to place the enqueue in its
	// expirySweepWindowSize window (expirySweepWindowStart). It is a field,
	// not a time.Now() call at the enqueue site, so the window a sweep is
	// enqueued under is deterministic in tests -- the same clock-seam
	// pattern Sweep's own callers rely on -- while defaulting to the real
	// clock for every production call.
	now func() time.Time
}

// newLifecycleService returns a LifecycleService deleting and sweeping over
// objects and derivatives. queue is the queue EnqueueExpirySweep enqueues
// on; a nil queue makes EnqueueExpirySweep fail with a plain error, the
// same "storage: no queue wired" answer ObjectService gives, because
// sweeping is optional work and a host that runs no workers must not be
// forced to wire a queue it cannot drain.
func newLifecycleService(objects *ObjectRepository, derivatives *DerivativeRepository, queue jobs.Queue) *LifecycleService {
	return &LifecycleService{
		objects:     objects,
		derivatives: derivatives,
		queue:       queue,
		now:         time.Now,
	}
}

// Delete permanently removes one object of the caller's tenant: its bytes,
// every derivative's bytes and all of its rows. It is the delete protocol's
// entry point, and its contract is that of a crash-convergent delete: a
// caller may run it any number of times, concurrently included, and every
// run that observes the object already gone -- deleted by an earlier run,
// or belonging to another tenant -- converges on nil, "already gone", never
// an error. The object's id is never reused, so a caller deleting an
// already-deleted id is answering "is it gone" with "yes" either way.
//
// Delete refuses exactly one state: an object still in ObjectStateUploading
// reports ErrObjectUploading and is left untouched, because an upload in
// flight belongs to the transfer runtime and may still complete -- only the
// expiry sweep reclaims uploading rows, once their window closed. An object
// already in ObjectStateDeleting is an interrupted protocol run and is
// resumed from where it stopped.
//
// The steps, in order:
//
//  1. markDeleting advances the row completed -> deleting (a guarded write;
//     a concurrent run can only find the row already deleting, never flip a
//     state this call did not see). From this point on the object's readers
//     see nothing.
//  2. The original bytes are removed from the store. A store error stops
//     the protocol here -- the row stays deleting and the next run resumes
//     from step 1, byte removal being idempotent.
//  3. The derivative rows are listed in the repository's deterministic
//     order and each derivative's bytes are removed, stopping on the first
//     store error for the same reason as step 2.
//  4. deleteObjectRows removes the object row and the derivative rows in
//     one transaction -- the protocol's commit point, after which no row
//     references the deleted bytes anywhere. The object row goes first on
//     purpose: it is the row a concurrent derive insert's gate locks, so
//     deleting it first makes this transaction's derivative-row removal
//     the race's last statement and removes whatever the gate admitted
//     while this deletion was waiting (repository.go's deleteObjectRows).
//
// The run whose row removal commits -- exactly one, however many runs raced
// over the object -- logs the deletion and publishes EventObjectDeleted.
// Publishing is the usual warn-and-stand: the deletion is already durable,
// and a failed bus is logged, never failed.
func (s *LifecycleService) Delete(ctx context.Context, objectID string) error {
	// The guarded state flip first: a row that never existed, or one a
	// concurrent run already removed, reports not-found and the delete
	// converges on "already gone".
	row, err := s.objects.markDeleting(ctx, objectID)
	if err != nil {
		if dbkit.IsRecordNotFound(err) {
			return nil
		}
		return err
	}
	if row.State == ObjectStateUploading {
		// The one state a delete refuses: an upload in flight belongs to
		// the transfer runtime, and only the sweep reclaims uploading rows
		// once their window closes. markDeleting returns completed rows
		// already flipped, so no completed row can reach this point.
		return ErrObjectUploading.WithParam("id", objectID)
	}

	st, err := s.requireStore()
	if err != nil {
		// No store means no bytes can be removed, and the protocol must not
		// remove the rows while the bytes they name still exist. The row
		// stays deleting; the sweep's first phase resumes it once a store
		// is wired.
		return err
	}

	err = st.DeleteObject(ctx, row.Key)
	if err != nil {
		// The row stays deleting and the bytes that were already removed
		// are simply not there for the next run -- DeleteObject is
		// idempotent, which is what makes the protocol resumable from any
		// step.
		return ErrStoreError.WithCause(err)
	}

	derivatives, err := s.derivatives.listByObject(ctx, objectID)
	if err != nil {
		return err
	}
	for _, d := range derivatives {
		err = st.DeleteObject(ctx, d.Key)
		if err != nil {
			return ErrStoreError.WithCause(err)
		}
	}

	removed, err := s.objects.deleteObjectRows(ctx, objectID)
	if err != nil {
		return err
	}
	if removed {
		// This run committed the row removal, so this run announces the
		// deletion -- exactly one run sees removed=true however many raced
		// over the object.
		observability.FromContext(ctx).Info("object deleted", "object_id", objectID)
		if err := s.publish(ctx, pkgcore.Event{
			Type:     EventObjectDeleted,
			TenantID: row.GetTenantID(),
			Payload:  ObjectDeletedPayload{ObjectID: objectID},
		}); err != nil {
			observability.FromContext(ctx).Warn("object-deleted event publish failed",
				"object_id", objectID, "error", err)
		}
	}
	return nil
}

// Sweep runs one tenant's full cleanup pass: it resumes every interrupted
// deletion, reclaims every upload whose window closed, and deletes every
// completed object whose retention deadline passed. It is the expiry-sweep
// task's body (expirySweepHandler) and doubles as the module's service
// entry point for a host that wants a tenant swept synchronously.
//
// ctx must carry a tenant -- the worker rebuilds it from the task's
// TenantID before Handle runs, and a direct caller passes
// pkgcore.WithTenant -- because every query the sweep runs is tenant-scoped.
//
// The three phases run on one now, captured once so the two expiry listings
// agree on what "expired" means within one pass:
//
//   - Phase 1 resumes interrupted deletions: every ObjectStateDeleting row
//     is run through Delete, which continues the protocol from wherever it
//     stopped and announces EventObjectDeleted for each deletion that
//     commits here.
//   - Phase 2 reclaims expired uploads: every ObjectStateUploading row
//     whose upload window closed before now has its bytes (if any) and its
//     rows removed -- no event, since nothing ever read them. A row another
//     sweep already reclaimed is nothing left to do, not an error.
//   - Phase 3 deletes expired completed objects: every ObjectStateCompleted
//     row whose retention deadline (expires_at) passed before now runs the
//     full Delete protocol, expiry being the same deletion the API would
//     have performed, announced with the same event.
//
// One object's failure does not stop the pass: every other row still runs,
// the shape compliance's SweepTenant applies to its own participants. A
// tenant is swept at most one worker at a time by the task's idempotency
// key and the sweep listings are deterministic, so a fail-fast pass would
// hit the same first refusing row on every run and never reach the rows
// after it -- one permanently failing object (a poisoned store key, say)
// would starve the rest of the tenant's expiry work indefinitely. Instead
// each failing row is logged with its id and left in the state the next
// pass resumes -- a deleting row stays deleting, an expired upload or
// object stays as it was -- and when any row failed, the pass ends with
// ErrSweepPartialFailure carrying the failed count, so a caller checking
// only "err != nil" still learns that the pass was not clean. (A seam-wide
// outage -- the store itself down -- makes every row of a phase fail; the
// sweep still walks the phase once, and the queue's retry policy re-runs
// the pass on the next attempt exactly as it re-ran a fail-fast one. Only
// the listings themselves fail the pass outright: a tenant whose rows
// cannot be read can have none of its cleanup run, and that is reported
// as-is rather than guessed at.)
func (s *LifecycleService) Sweep(ctx context.Context) error {
	now := time.Now()
	failures := 0

	// Phase 1: rows in deleting exist only because a Delete did not finish;
	// resume each one's protocol. A row whose resume fails stays deleting,
	// bytes intact, for the next pass.
	deleting, err := s.objects.listStateRows(ctx, ObjectStateDeleting)
	if err != nil {
		return err
	}
	for _, row := range deleting {
		if deleteErr := s.Delete(ctx, row.ID); deleteErr != nil {
			failures++
			observability.FromContext(ctx).Warn("expiry sweep could not finish an interrupted deletion",
				"object_id", row.ID, "error", deleteErr)
		}
	}

	// Phase 2: uploads whose window closed are reclaimed as never-arriving.
	uploads, err := s.objects.listExpiredUploads(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range uploads {
		if reclaimErr := s.reclaimUpload(ctx, row); reclaimErr != nil {
			failures++
			observability.FromContext(ctx).Warn("expiry sweep could not reclaim an expired upload",
				"object_id", row.ID, "error", reclaimErr)
		}
	}

	// Phase 3: completed objects whose retention deadline passed are
	// deleted like any other object, same protocol, same event.
	expired, err := s.objects.listExpiredCompleted(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range expired {
		if deleteErr := s.Delete(ctx, row.ID); deleteErr != nil {
			failures++
			observability.FromContext(ctx).Warn("expiry sweep could not delete an expired object",
				"object_id", row.ID, "error", deleteErr)
		}
	}

	if failures > 0 {
		return ErrSweepPartialFailure.WithParam("failed_rows", failures)
	}
	return nil
}

// reclaimUpload removes one expired upload's rows, deleting its bytes
// first. It is the sweep's phase-2 body and deliberately publishes no
// event: the upload never completed, so nothing ever read it and no
// subscriber has anything to forget. Its row removal tolerates a row that
// is already gone -- two sweeps racing over one tenant's rows both list an
// upload, and the second one's removal of nothing is convergence, not an
// error.
//
// The no-event shortcut, and the whole of the reclaim's licence to remove
// a row family outside the delete protocol, rests on one dependency: no
// path in this module returns a completed row -- one whose bytes were
// finalized and which may carry derivatives -- to uploading. Only rows that
// never completed can read uploading, and only they are reclaimed. The day
// someone adds a re-upload or a back-to-uploading transition for completed
// objects, this reclaim would delete a completed object's family -- its
// original bytes, its derivative rows and their bytes -- with no delete
// protocol and no EventObjectDeleted for the subscribers that watched that
// object complete, and that hazard becomes live.
func (s *LifecycleService) reclaimUpload(ctx context.Context, row Object) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	if err := st.DeleteObject(ctx, row.Key); err != nil {
		return ErrStoreError.WithCause(err)
	}
	if _, err := s.objects.deleteObjectRows(ctx, row.ID); err != nil {
		return err
	}
	observability.FromContext(ctx).Info("expired upload reclaimed", "object_id", row.ID)
	return nil
}

// EnqueueExpirySweep enqueues the expiry-sweep task for the tenant ctx
// carries. The sweep's default schedule is the module's own: Register
// declares it on the pkgcore.Registry.Schedules seat (expirySweepSchedule,
// a per-tenant task at the sweep's own window), so a host that runs a
// jobs.Scheduler sweeps every tenant without writing a schedule point of
// its own; this method remains the manual entry point. The task's
// window-scoped idempotency key (expirySweepIdempotencyKey) collapses the
// enqueues of one expirySweepWindowSize window -- a scheduler with two
// replicas ticking in the same window, a manual re-run -- into one job, so
// a tenant is never swept by two workers at once. An enqueue whose clock
// has moved into a later window (expirySweepWindowStart) is a new job and
// runs again: this is what makes the sweep periodic on queues whose idempotency
// is unconditional, and what keeps one dead-lettered sweep from poisoning
// its tenant forever -- see expirySweepIdempotencyKey's doc comment for
// the full window semantics. The task carries no payload: the sweep reads
// the rows and the clock when it runs.
//
// ctx must carry a tenant; the task's own TenantID is taken from it. A
// caller with no tenant in context gets ErrInternal, because a tenant-less
// sweep is a wiring error -- nothing in this service may guess a tenant.
// Enqueue errors (an invalid task, a queue that refuses) pass through
// unchanged. With no queue wired (nil), it fails with a plain error: a host
// that runs no workers has nothing to enqueue onto, and sweeping is
// optional work, not a reason to boot-fail -- the module's queue
// requirement is about the work it already promised (WithQueue's doc), not
// about this schedule point.
func (s *LifecycleService) EnqueueExpirySweep(ctx context.Context) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if s.queue == nil {
		return errors.New("storage: no queue wired")
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeExpirySweep,
		TenantID:       tenant,
		IdempotencyKey: expirySweepIdempotencyKey(tenant, expirySweepWindowStart(s.now())),
	})
	return err
}

// expirySweepHandler is the jobs.Handler claiming taskTypeExpirySweep, the
// task EnqueueExpirySweep schedules. Its Handle runs LifecycleService.Sweep
// on the tenant context the worker rebuilt from the task.
type expirySweepHandler struct {
	svc *LifecycleService
}

// Type returns the task type this handler claims -- the type the schedule
// point enqueues under, and the string jobs matches at dispatch.
func (h expirySweepHandler) Type() string { return taskTypeExpirySweep }

// Handle runs one expiry-sweep task. The task's payload must be empty -- a
// sweep takes its inputs from the rows and the clock at run time, so a
// payload would have nothing to say; one that is not empty is a task-shape
// violation and fails the job (the queue retries and eventually
// dead-letters such a task -- it can never succeed by re-running). Every
// other outcome is the service's own: nil for a completed pass, the typed
// service error for a failure the queue's retry policy exists for.
func (h expirySweepHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("storage: expiry-sweep task carries an unexpected payload")
	}
	if err := h.svc.Sweep(ctx); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}
