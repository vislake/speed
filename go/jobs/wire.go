package jobs

import (
	"context"
	"errors"

	"github.com/vislake/speed/go/pkgcore"
)

// Wire prepares q to receive and execute the work every module declared on
// reg: it drains reg's job handlers onto q and creates the queue's tables
// (the jobs table, the queue_writers single-writer table and the jobs
// indexes) if they do not already exist -- the same ensureJobsSchema DDL
// Start itself runs, which makes a later Start's own call to it a no-op.
//
// It is the one call a host makes after Kernel.Bootstrap -- the point at
// which every module's Register has run and reg is complete -- and it is
// deliberately separate from Start, which stays the host's own,
// deployment-mode-gated step. A worker-disabled replica (see
// StandaloneQueue.Start's doc comment and the reference app's
// APP_DISABLE_QUEUE_WORKER) still calls Wire: the replica can never claim
// or execute a Job, but Enqueue finds its table and a handler that a
// module's wiring registered is never refused with ErrHandlerNotRegistered.
// Registering handlers before Start is RegisterHandler's documented
// contract, so a host that does start workers may call Wire and Start in
// either order.
//
// The drain itself, every entry's must-be-a-jobs.Handler rule, its
// ascending-order determinism and its non-transactional registration are
// RegisterHandlers' contract; Wire hands it reg.Handlers(). A second call
// over the same queue therefore fails with RegisterHandler's own
// ErrDuplicateHandlerType (wrapped by RegisterHandlers, naming the job
// type), because the queue already holds that Type -- the map a registry
// exposes cannot itself carry a duplicate, so a duplicate at this layer
// means the same declaration is being wired twice, which is a host bug
// worth reporting rather than papering over. A host that re-assembles
// builds a fresh queue, as the reference app's BuildServer does.
//
// Wire performs its schema work under ctx (which must not be nil) and
// launches no goroutine: with it done and Start called, the host is serving.
func Wire(ctx context.Context, q *StandaloneQueue, reg pkgcore.JobHandlerRegistrar) error {
	if q == nil {
		return errors.New("jobs: Wire requires a non-nil *StandaloneQueue")
	}
	if reg == nil {
		return errors.New("jobs: Wire requires a non-nil JobHandlerRegistrar")
	}
	if ctx == nil {
		return errors.New("jobs: Wire requires a non-nil context")
	}

	if err := RegisterHandlers(q, reg.Handlers()); err != nil {
		return err
	}

	return ensureJobsSchema(ctx, q.db)
}
