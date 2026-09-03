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
// The protocol's race partner is the derive worker, and the two converge as
// derive.go's header records: bytes the worker wrote after this protocol's
// walk are dropped on its own re-read, and the microsecond window the two
// still share is closed by the sweep, which treats a ghost derivative as an
// object's ordinary row and removes it -- and the bytes it names -- with the
// next resumed run. Sweep itself is what makes interrupted deletions
// finish: deleting rows exist only because a Delete did not finish, and the
// sweep's first phase re-runs the protocol over each of them.
//
// # The sweep's clock
//
// Sweep reclaims two kinds of unfinished life on one now: uploading rows
// whose upload window closed (their bytes, if any, are never becoming
// readable) and completed rows whose retention deadline passed. Reclaiming
// an upload is safe against a completion racing it only because the upload
// window is enforced at the finalize write itself -- a completion that
// straddles its own window end cannot commit after it (repository.go's
// finalizeUpload, the fix this round's earlier commit shipped) -- so a row
// this sweep listed can never complete behind its back: either the
// completion committed before the window closed, in which case the row is
// completed and no longer matches the reclaim listing, or the write is
// refused and the row is reclaimed. Uploads are reclaimed without an event:
// nothing ever read them, so no subscriber has anything to forget. Deleted
// completed objects announce EventObjectDeleted exactly like a Delete call.
//
// # The expiry-sweep task
//
// EnqueueExpirySweep is the host-facing enqueue for one tenant's sweep, and
// expirySweepHandler is the jobs.Handler claiming the task on the queue.
// The task is tenant-scoped because every query the sweep runs is: a host
// with many tenants schedules one task per tenant (a platform loop is the
// ordinary shape, exactly the caller jobs.Task's TenantID doc names), and
// the tenant rides in the task's TenantID field, rebuilt into context by
// the worker before Handle runs, never inherited from the enqueuing side.
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

// expirySweepIdempotencyKey derives the jobs idempotency key of a
// tenant's expiry-sweep task from the tenant id, per the rule that an
// idempotency key derives from the business operation, never random: two
// enqueues for the same tenant's sweep -- a scheduler with two replicas, a
// manual re-run -- collapse into one job, so a tenant is never swept by two
// workers at once. The "storage.sweep:" prefix keeps the key inside the
// module's namespace within the shared queue store.
func expirySweepIdempotencyKey(tenant pkgcore.TenantID) string {
	return "storage.sweep:" + string(tenant)
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
//  4. deleteObjectRows removes the derivative rows and the object row in
//     one transaction -- the protocol's commit point, after which no row
//     references the deleted bytes anywhere.
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
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
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

	if err := st.DeleteObject(ctx, row.Key); err != nil {
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
		if err := st.DeleteObject(ctx, d.Key); err != nil {
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
// agree on what "expired" means within one pass, and each phase fails fast:
// the first row that refuses stops the pass, with that row left in the
// state the next pass can resume (a deleting row stays deleting, an
// expired upload or object stays as it was). A tenant is swept at most one
// worker at a time by the task's idempotency key, so a failed row is
// normally the store or the database itself -- failing the pass rather than
// plowing through the rest of the rows on a broken seam is what keeps each
// row's error attributable.
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
func (s *LifecycleService) Sweep(ctx context.Context) error {
	now := time.Now()

	// Phase 1: rows in deleting exist only because a Delete did not finish;
	// resume each one's protocol.
	deleting, err := s.objects.listStateRows(ctx, ObjectStateDeleting)
	if err != nil {
		return err
	}
	for _, row := range deleting {
		if err := s.Delete(ctx, row.ID); err != nil {
			return err
		}
	}

	// Phase 2: uploads whose window closed are reclaimed as never-arriving.
	uploads, err := s.objects.listExpiredUploads(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range uploads {
		if err := s.reclaimUpload(ctx, row); err != nil {
			return err
		}
	}

	// Phase 3: completed objects whose retention deadline passed are
	// deleted like any other object, same protocol, same event.
	expired, err := s.objects.listExpiredCompleted(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range expired {
		if err := s.Delete(ctx, row.ID); err != nil {
			return err
		}
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
// carries. It is the host-facing schedule point: a host with workers runs
// it on its own timer per tenant, and the task's per-tenant idempotency key
// collapses concurrent enqueues -- a scheduler with replicas, a manual
// re-run -- into one job, so a tenant is never swept by two workers at
// once. The task carries no payload: the sweep reads the rows and the
// clock when it runs.
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
		IdempotencyKey: expirySweepIdempotencyKey(tenant),
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
