package main

import (
	"context"
	"time"

	"github.com/vislake/speed/go/compliance"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/storage"
)

// The reference app's periodic-task scheduler.
//
// go/jobs deliberately ships no periodic facility of its own -- a task
// exists only once something enqueues it, and who decides when that
// happens is the host. storage's, compliance's and pki's job-driven
// mechanisms are therefore scheduled here, in the host, by this file's
// single ticker loop, exactly the way internal/smilesim's own reconciler
// ticker (StartReconciler) runs in this same process. One shared cadence
// drives the three mechanisms this app actually wired:
//
//   - storage's per-tenant expiry sweep, one task per tenant the host
//     serves (LifecycleService.EnqueueExpirySweep under a pkgcore tenant
//     context -- the sweep itself runs tenant-scoped, so the tenant must
//     travel on the task). The enqueue carries a deterministic idempotency
//     key scoped to the expirySweepWindowSize window the enqueue falls in
//     (go/storage/cleanup.go's expirySweepIdempotencyKey): the design
//     intent is to collapse concurrent replica enqueues of ONE window into
//     one in-flight sweep -- which is what same-window ticks do on both
//     queues -- while the first tick of every later window resolves a
//     fresh key. On this app's StandaloneQueue (where a resolved key is
//     held forever, go/jobs) that means at most one sweep per tenant per
//     window -- not one per database file, the pre-window design's
//     residual: an object whose retention deadline passes is reaped by the
//     next window's sweep, and a sweep job that dead-letters poisons only
//     its own window. (The older once-per-file behaviour and its
//     two-boot proof are recorded in go/storage/AGENTS.md's dated sweep
//     entry.) What periodic_scheduler_flow_test.go's two-boot test proves
//     end to end is unchanged in shape: boot 1 lets a completed object
//     expire with the scheduler disabled, and boot 2's first sweep -- the
//     first tick lands in a fresh window -- must remove that object's row
//     and bytes through the real host wiring.
//   - compliance's per-tenant retention sweep, one task per tenant the
//     host serves (RetentionService.EnqueueRetentionSweep under a pkgcore
//     tenant context -- the sweep runs tenant-scoped for the same reason
//     the expiry sweep's does, so the tenant must travel on the task).
//     The enqueue carries the window-scoped idempotency key
//     go/compliance/retention.go derives (retentionSweepIdempotencyKey),
//     deliberately aligned with the expiry sweep's, so the two mechanisms
//     share one schedule shape: on this app's StandaloneQueue at most one
//     retention sweep per tenant per retentionSweepWindowSize window
//     runs, later windows' ticks schedule the sweep again, and a
//     dead-lettered sweep poisons only its own window -- so a soft-deleted
//     row that ages past its retention window is hard-deleted by a later
//     window's sweep, never retained until the database file happens to be
//     replaced. The sweep drives every participant registered on the
//     kernel's reg.Retention seat -- notes' soft-deleted notes and
//     compliance's own stored export manifests alike -- so nothing else
//     needs its own schedule point. The trigger-half proof is the same
//     two-boot shape as the expiry sweep's:
//     periodic_scheduler_flow_test.go's retention leg lets boot 1 leave a
//     soft-deleted note 45 days past the default window with the scheduler
//     disabled, and boot 2's first sweep must then hard-delete that row
//     through the real host wiring.
//   - pki's signing-key expiry scan, one platform-level task per tick
//     (Service.EnqueueExpiryScan: pki's keys are platform data, so the
//     scan carries no tenant at all). This is what actually drives the
//     signing-key lifecycle state machine (pending -> active -> retiring
//     -> retired) in this app -- without these enqueues, the boot key
//     authn signs with would simply age out of its rotation policy with
//     nobody ever staging its replacement.
//
// What is deliberately NOT scheduled here, and why:
//
//   - pki's CRL-regeneration task (CAService.EnqueueCRLRegenerate) is
//     never enqueued: nothing in this app consumes pki's X.509/CRL layer
//     (no CA issuance, no certificate verification), so regenerating a
//     CRL nobody reads would be work for its own sake. pki.Module.Register
//     still declares the CRL handler on the registry whenever the module
//     is wired with a queue -- this host wires pki.WithQueue below, so the
//     handler IS registered and drained onto the shared queue; it simply
//     never receives a task. go/pki/AGENTS.md records that honestly.
//
// The tick body is synchronous: every tick runs every enqueue to
// completion before the next tick can fire, and time.Ticker drops a tick
// rather than queueing it, so a slow tick can never pile up a backlog of
// overlapping sweeps. A failed enqueue is logged and retried by the next
// tick -- the schedule itself never stops on an error, matching
// internal/smilesim's reconciler.
//
// The scheduler's start and stop are bound to the queue's lifecycle in
// buildServer: it starts only when the queue worker did (the same
// cfg.DisableQueueWorker gate that guards standaloneQueue.Start -- a task
// this replica can never execute is pointless to enqueue), and cleanup
// stops it before standaloneQueue.Close, so no tick can enqueue against a
// queue that is being torn down.
const defaultPeriodicTaskSchedulerInterval = time.Minute

// startPeriodicTaskScheduler starts the loop that enqueues the wired
// periodic mechanisms' tasks every interval (defaultPeriodicTaskSchedulerInterval
// when interval is non-positive -- the injectable cadence exists for the
// flow tests, which drive real ticks within test time, exactly like the
// serverConfig test-override fields of server.go).
//
// tenants is the host's real tenant universe (cfg.HostTenants): its
// values are the tenant ids swept, deduplicated, because two hosts can
// map to one tenant and the sweep is per tenant, not per host (the same
// dedupe seedDemoGrants performs for grants). ctx is the context every
// tick's enqueues run under; like StartReconciler's own ctx it should be
// context.Background() -- a context cancelled by the same shutdown
// sequence that calls the returned stop func would fail every enqueue on
// the final tick.
//
// The returned stop func is idempotent-safe to call exactly once: it
// signals the loop and blocks until the in-flight tick (if any) has
// returned, so the caller knows no enqueue is still running when the
// queue is closed underneath it.
func startPeriodicTaskScheduler(ctx context.Context, interval time.Duration, tenants map[string]pkgcore.TenantID, lifecycle *storage.LifecycleService, retention *compliance.RetentionService, signingKeys *pki.Service) func() {
	if interval <= 0 {
		interval = defaultPeriodicTaskSchedulerInterval
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				runPeriodicTasks(ctx, tenants, lifecycle, retention, signingKeys)
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

// runPeriodicTasks performs one scheduler tick: one expiry-sweep enqueue
// and one retention-sweep enqueue per unique tenant in tenants, then one
// signing-key expiry-scan enqueue. Every enqueue failure is logged and
// left for the next tick to retry -- enqueues are durable row inserts, so
// a failed one changes nothing and the mechanism stays exactly as due as
// it was.
func runPeriodicTasks(ctx context.Context, tenants map[string]pkgcore.TenantID, lifecycle *storage.LifecycleService, retention *compliance.RetentionService, signingKeys *pki.Service) {
	log := obs.FromContext(ctx)
	seen := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seen[tenantID]; done {
			continue
		}
		seen[tenantID] = struct{}{}
		// Both sweep handlers run tenant-scoped (their repositories filter
		// on the context's tenant), so each sweep is enqueued under the
		// tenant's own context -- never the scheduler's ctx, which carries
		// none.
		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		if err := lifecycle.EnqueueExpirySweep(tenantCtx); err != nil {
			log.Warn("periodic tasks: expiry-sweep enqueue failed, will retry on the next tick",
				"tenant_id", tenantID, "error", err)
		}
		if err := retention.EnqueueRetentionSweep(tenantCtx); err != nil {
			log.Warn("periodic tasks: retention-sweep enqueue failed, will retry on the next tick",
				"tenant_id", tenantID, "error", err)
		}
	}
	if err := signingKeys.EnqueueExpiryScan(ctx); err != nil {
		log.Warn("periodic tasks: signing-key expiry-scan enqueue failed, will retry on the next tick",
			"error", err)
	}
}
