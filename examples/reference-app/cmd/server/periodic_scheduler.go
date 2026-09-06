package main

import (
	"context"
	"time"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/storage"
)

// The reference app's periodic-task scheduler.
//
// go/jobs deliberately ships no periodic facility of its own -- a task
// exists only once something enqueues it, and who decides when that
// happens is the host. storage's and pki's job-driven mechanisms are
// therefore scheduled here, in the host, by this file's single ticker
// loop, exactly the way internal/smilesim's own reconciler ticker
// (StartReconciler) runs in this same process. One shared cadence drives
// the two mechanisms this app actually wired:
//
//   - storage's per-tenant expiry sweep, one task per tenant the host
//     serves (LifecycleService.EnqueueExpirySweep under a pkgcore tenant
//     context -- the sweep itself runs tenant-scoped, so the tenant must
//     travel on the task). The enqueue carries a deterministic per-tenant
//     idempotency key whose design intent is to collapse concurrent
//     replica enqueues into one in-flight sweep; under the asynq queue's
//     bounded idempotency that intent holds. On this app's
//     StandaloneQueue the same key is permanent (go/jobs: a resolved key
//     is held forever, succeeded rows are never deleted), so each tenant
//     gets exactly one sweep per database file -- the first-ever one,
//     whenever the host's scheduler first enqueues it -- and every later
//     tick's duplicate enqueue merges into that completed job. A
//     standalone host therefore sweeps each tenant at most once per
//     database file; an object whose retention deadline passes after that
//     one sweep has run is never reaped on this queue, the residual
//     limitation go/storage/AGENTS.md records. The sweep's one chance per
//     file is exactly what periodic_scheduler_flow_test.go's two-boot
//     test proves end to end: boot 1 lets a completed object expire with
//     the scheduler disabled, boot 2's first-ever sweep must then remove
//     that object's row and bytes through the real host wiring.
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
//   - go/compliance's RetentionService scheduling is not wired here
//     either; that mechanism's host schedule is another round's work
//     (the same frozen plan that names pki's expiry scan and storage's
//     sweep as this round's wired mechanisms).
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
func startPeriodicTaskScheduler(ctx context.Context, interval time.Duration, tenants map[string]pkgcore.TenantID, lifecycle *storage.LifecycleService, signingKeys *pki.Service) func() {
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
				runPeriodicTasks(ctx, tenants, lifecycle, signingKeys)
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

// runPeriodicTasks performs one scheduler tick: one expiry-sweep enqueue
// per unique tenant in tenants, then one signing-key expiry-scan enqueue.
// Every enqueue failure is logged and left for the next tick to retry --
// enqueues are durable row inserts, so a failed one changes nothing and
// the mechanism stays exactly as due as it was.
func runPeriodicTasks(ctx context.Context, tenants map[string]pkgcore.TenantID, lifecycle *storage.LifecycleService, signingKeys *pki.Service) {
	log := obs.FromContext(ctx)
	seen := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seen[tenantID]; done {
			continue
		}
		seen[tenantID] = struct{}{}
		// The sweep handler runs tenant-scoped (its repository filters on
		// the context's tenant), so each sweep is enqueued under the
		// tenant's own context -- never the scheduler's ctx, which carries
		// none.
		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		if err := lifecycle.EnqueueExpirySweep(tenantCtx); err != nil {
			log.Warn("periodic tasks: expiry-sweep enqueue failed, will retry on the next tick",
				"tenant_id", tenantID, "error", err)
		}
	}
	if err := signingKeys.EnqueueExpiryScan(ctx); err != nil {
		log.Warn("periodic tasks: signing-key expiry-scan enqueue failed, will retry on the next tick",
			"error", err)
	}
}
