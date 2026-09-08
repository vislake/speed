package integration

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is the API-key expiry sweep: the jobs task a host schedules per
// tenant on its own timer, which physically removes the APIKey rows (and
// their apiKeyHashIndex companions) whose ExpiresAt has passed.
//
// Nothing about correct authentication depends on the sweep: Authenticate
// checks IsExpired against the caller's own clock on every call, so an
// unswept, past-expiry row is refused correctly today -- the sweep only
// reclaims disk (and the dead index rows Authenticate can no longer use).
// That is what makes the sweep optional, host-scheduled work rather than a
// correctness fix, and it is why an unwired queue is a plain error at the
// enqueue point instead of a boot failure: a host that runs no workers has
// nothing to enqueue onto, and its keys are not one day less secure for
// that.
//
// The per-tenant shape is forced by this module's own rules, not a choice:
// APIKey is TenantScoped, every repository read goes through the
// tenant-scope plugin, and this module holds no system-context grant -- so
// the sweep operates on exactly the tenant its context carries, and a host
// with many tenants schedules one task per tenant, exactly like go/storage's
// own EnqueueExpirySweep (go/storage/cleanup.go), whose windowed
// idempotency-key design this file mirrors.

// jobTypeAPIKeyExpirySweep is the Task.Type of every expiry-sweep task this
// module enqueues and handles, following the same "integration.<entity>.
// <action>" naming as jobTypeWebhookDeliver. The task carries no payload:
// the sweep reads the rows and the clock when it runs.
const jobTypeAPIKeyExpirySweep = "integration.apikey.expiry_sweep"

// apiKeyExpirySweepWindowSize is the period one expiry-sweep idempotency
// key covers, mirroring go/storage's identical expirySweepWindowSize: the
// same-window duplicates an idempotency key exists to collapse -- a
// scheduler with two replicas, a manual re-run -- merge into one job, while
// an enqueue in a later window becomes a new job and the sweep runs again.
// The window is what makes the sweep periodic at all: jobs' idempotency is
// unconditional for one key on StandaloneQueue (a resolved key is held
// forever), so a tenant-only key would give each tenant exactly one sweep
// per database file -- and, worse, a sweep job that dead-letters would
// poison its tenant forever, since every later enqueue would keep returning
// the dead job's id. A dead-lettered job now poisons only its own window;
// the next window's enqueue is a fresh key and runs. One hour means an
// expired key is reaped at most an hour after the sweep that should have
// caught it was enqueued, at the cost of one task per tenant per hour at
// most.
const apiKeyExpirySweepWindowSize = time.Hour

// apiKeyExpirySweepWindowStart is the expiry-sweep window the enqueue at
// now belongs to -- the absolute hour boundary now.Truncate(
// apiKeyExpirySweepWindowSize) lands in. Two replicas enqueuing within the
// same window share one key (and one job); a tick in a later window gets
// its own. Truncation is on the absolute clock, never a timezone-local
// calendar cut, so every replica agrees on the boundary regardless of its
// own location.
func apiKeyExpirySweepWindowStart(now time.Time) time.Time {
	return now.Truncate(apiKeyExpirySweepWindowSize)
}

// apiKeyExpirySweepIdempotencyKey derives the jobs idempotency key of one
// expiry-sweep window for a tenant, per the rule that an idempotency key
// derives from the business operation, never random: the operation one key
// names is "the sweep of windowStart", not "some sweep or other" -- a
// periodic task's identity inherently includes WHICH period it is for. The
// "integration.sweep:" prefix keeps the key inside the module's namespace
// within the shared queue store, and the RFC 3339 window stamp keeps the
// key readable while staying unambiguous.
func apiKeyExpirySweepIdempotencyKey(tenant pkgcore.TenantID, windowStart time.Time) string {
	return "integration.sweep:" + string(tenant) + ":" + windowStart.UTC().Format(time.RFC3339)
}

// EnqueueAPIKeyExpirySweep enqueues the expiry-sweep task for the tenant ctx
// carries. It is the host-facing schedule point: a host with workers runs it
// on its own timer per tenant, and the task's window-scoped idempotency key
// (apiKeyExpirySweepIdempotencyKey) collapses the enqueues of one
// apiKeyExpirySweepWindowSize window into one job, so a tenant is never
// swept by two workers at once -- and an enqueue whose clock has moved into
// a later window (apiKeyExpirySweepWindowStart) is a new job and runs
// again, which is what keeps the sweep periodic and what keeps one
// dead-lettered sweep from poisoning its tenant forever.
//
// ctx must carry a tenant; the task's own TenantID is taken from it. A
// caller with no tenant in context gets ErrInternal, because a tenant-less
// sweep is a wiring error -- nothing in this service may guess a tenant.
// With no queue wired (nil -- the Module was built without WithWebhookQueue),
// it fails with a plain error: a host that runs no workers has nothing to
// enqueue onto, and sweeping is optional work, not a reason to boot-fail.
// Enqueue errors pass through unchanged.
func (s *Service) EnqueueAPIKeyExpirySweep(ctx context.Context) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if s.queue == nil {
		return errors.New("integration: no queue wired (WithWebhookQueue)")
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           jobTypeAPIKeyExpirySweep,
		TenantID:       tenant,
		IdempotencyKey: apiKeyExpirySweepIdempotencyKey(tenant, apiKeyExpirySweepWindowStart(s.clock())),
	})
	return err
}

// SweepExpiredAPIKeys removes every API key of the tenant in ctx whose
// ExpiresAt has passed, together with each removed key's apiKeyHashIndex
// row -- the sweep's row lifecycle, in full:
//
//   - A key is never removed before its own ExpiresAt, whatever else is
//     true of it. A revoked key lingers until its natural expiry so its row
//     stays visible in List (Revoked: true) for the remainder of the
//     lifetime it was issued for -- the same reason a rotation's predecessor
//     stays listed until then -- and only a key whose expiry has genuinely
//     passed is dead weight this sweep may reclaim.
//   - Once ExpiresAt has passed, nothing this module does needs the row:
//     Authenticate already refuses the key (IsExpired, on every call), List
//     can no longer describe a credential with any use left in it, and
//     Rotate/Revoke answering not-found for a swept id is indistinguishable
//     from their answer for any other expired key. Removing the row changes
//     no observable refusal: Authenticate of a swept key answers
//     ErrAuthenticationFailed exactly as it did before, now at the
//     tenant-resolution step instead of the tenant-scoped read -- the
//     outward answer is the same sentinel either way, which is why deleting
//     the hash-index row alongside the key row is safe rather than a
//     no-enumeration regression.
//
// Each removed pair is one transaction (APIKeyRepository.
// deleteWithHashIndex), so an interruption mid-sweep leaves the remaining
// rows untouched for the next run rather than half-removed. A failure
// listing or deleting is reported so the job retries; re-running the sweep
// converges on the rows still left, never re-processing removed ones.
func (s *Service) SweepExpiredAPIKeys(ctx context.Context) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		// The tenant-scope plugin would fail the listing below closed on its
		// own, but naming the missing context here is the same clearer
		// answer EnqueueAPIKeyExpirySweep gives its caller for the identical
		// wiring error -- nothing in this service may guess a tenant.
		return ErrInternal.WithCause(err)
	}
	now := s.clock()
	expired, err := s.repo.ListExpired(ctx, now)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	for i := range expired {
		row := expired[i]
		if err := s.repo.deleteWithHashIndex(ctx, &row); err != nil {
			return ErrInternal.WithCause(err)
		}
	}
	if len(expired) > 0 {
		obs.FromContext(ctx).Info("integration API key expiry sweep removed expired keys",
			"count", len(expired))
	}
	return nil
}

// apiKeyExpirySweepHandler adapts Module onto jobs.Handler for the
// expiry-sweep task type, forwarding to the Service Attach built -- the
// identical "Module method registered during Register, Service built later
// during Attach" split webhookDeliveryHandler uses, needed for the same
// reason: reg.Jobs.Handle must be called during Register, before a Service
// exists to hand it directly. ctx already carries the job's tenant (jobs
// rebuilds it before calling Handle), so the sweep below runs against the
// tenant the task was enqueued for.
type apiKeyExpirySweepHandler struct {
	module *Module
}

// Type implements jobs.Handler.
func (h apiKeyExpirySweepHandler) Type() string { return jobTypeAPIKeyExpirySweep }

// Handle implements jobs.Handler.
func (h apiKeyExpirySweepHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if h.module.service == nil {
		return jobs.Result{}, errors.New("integration: API key expiry sweep job ran before Module.Attach")
	}
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("integration: API key expiry-sweep task carries an unexpected payload")
	}
	if err := h.module.service.SweepExpiredAPIKeys(ctx); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// compile-time check that apiKeyExpirySweepHandler satisfies jobs.Handler.
var _ jobs.Handler = apiKeyExpirySweepHandler{}
