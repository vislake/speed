package app

import (
	"context"
	"time"

	"github.com/vislake/speed/go/admin"
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
//   - storage's per-tenant expiry sweep, one task per tenant this host
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
//     window: an object whose retention deadline passes is reaped by the
//     next window's sweep, and a sweep job that dead-letters poisons only
//     its own window. What flowtests/periodic_scheduler_flow_test.go's two-boot
//     test proves
//     end to end: boot 1 lets a completed object
//     expire with the scheduler disabled, and boot 2's first sweep -- the
//     first tick lands in a fresh window -- must remove that object's row
//     and bytes through the real host wiring.
//   - compliance's per-tenant retention sweep, one task per tenant this
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
//     flowtests/periodic_scheduler_flow_test.go's retention leg lets boot 1 leave a
//     soft-deleted note 45 days past the default window with the scheduler
//     disabled, and boot 2's first sweep must then hard-delete that row
//     through the real host wiring.
//   - The "tenant this host serves" set both per-tenant mechanisms
//     enqueue for is NOT a configured list alone, and the scheduler's
//     universe is resolved accordingly each tick (periodicTenantUniverse
//     below): the tenants cfg.HostTenants names are joined with every
//     tenant go/admin's D3 ledger records. The ledger half closes the
//     wiring P1 where a self-registered clinic -- a tenant this app's own
//     registration flow provisions at runtime (self_service.go's
//     ClinicTenantOf: "tenant-" + user id, by construction never a
//     cfg.HostTenants value) -- never entered the scheduler's universe,
//     so storage's expiry sweep and compliance's retention sweep never
//     ran for it: expired objects stayed unreclaimed and soft-deleted
//     rows past their retention windows were retained indefinitely, for
//     EVERY self-registered tenant. Neither module can close that gap
//     itself -- no tenant registry exists below the modules, by
//     deliberate design -- so the host's scheduler must draw its universe
//     from what the platform has actually SEEN, which is exactly what
//     the D3 ledger records (go/admin/tenant_service.go's
//     handleOrgNodeCreated: every org ROOT creation, self-service
//     provisioning's first step included, lazily lands a durable ledger
//     row for its tenant).
//   - pki's signing-key expiry scan, one platform-level task per tick
//     (Service.EnqueueExpiryScan: pki's keys are platform data, so the
//     scan carries no tenant at all), whose enqueue carries a
//     window-scoped idempotency key (go/pki/job.go's
//     expiryScanIdempotencyKey, DefaultExpiryScanWindow -- one hour
//     against this app's one-minute tick, a 60:1 ratio): on this app's
//     StandaloneQueue the scan therefore runs at most once per window,
//     per-minute ticks collapsing into the hour's one job, which is all
//     the day-scale rotation cadence needs. This is what actually drives
//     the signing-key lifecycle state machine (pending -> active ->
//     retiring -> retired) in this app -- without these enqueues, the
//     boot key authn signs with would simply age out of its rotation
//     policy with nobody ever staging its replacement. (The rotation
//     flow test compresses the window below its own tick cadence through
//     cfg.PKIExpiryScanWindow; the window semantics themselves are
//     proven in go/pki's own enqueue_window_test.go.)
//
// What is deliberately NOT scheduled here, and why:
//
//   - pki's CRL-regeneration task (CAService.EnqueueCRLRegenerate) is
//     never enqueued, even though this app now consumes pki's X.509/CRL
//     layer for real (the AI-output attestation layer, internal/
//     attestation: CA issuance, per-tenant certificate signing and chain-
//     verified sharing gates): the app's verifier paths read ROW STATE
//     plus the certificate chain (VerifyCertificate refuses a revoked row
//     directly), its CRL documents are generated on demand through the Go
//     API and fetched over pki's HTTP read operation, and a periodic
//     refresh still has no reader waiting on it -- so regenerating a CRL
//     on a schedule would be work for its own sake. pki.Module.Register
//     still declares the CRL handler on the registry whenever the module
//     is wired with a queue -- this host wires pki.WithQueue below, so the
//     handler IS registered and drained onto the shared queue; it simply
//     never receives a task.
//
// The tick body is synchronous: every tick runs every enqueue to
// completion before the next tick can fire, and time.Ticker drops a tick
// rather than queueing it, so a slow tick can never pile up a backlog of
// overlapping sweeps. A failed enqueue is logged and retried by the next
// tick -- the schedule itself never stops on an error, matching
// internal/smilesim's reconciler.
//
// The scheduler's start and stop are bound to the queue's lifecycle in
// BuildServer: it starts only when the queue worker did (the same
// cfg.DisableQueueWorker gate that guards standaloneQueue.Start -- a task
// this replica can never execute is pointless to enqueue), and cleanup
// stops it before standaloneQueue.Close, so no tick can enqueue against a
// queue that is being torn down.
const defaultPeriodicTaskSchedulerInterval = time.Minute

// periodicTenantUniverse answers the question each tick's per-tenant
// enqueues depend on -- "which tenants does this host serve?" -- from
// the union of two sources:
//
//   - configured: cfg.HostTenants, the Host -> TenantID map this
//     deployment was told about at boot. Sweeping these never depends on
//     any discovery mechanism: a configured tenant is swept whether or
//     not anything else in the platform has recorded it.
//   - the D3 tenant ledger (go/admin's TenantService, this app's
//     adminModule.Tenants()): the platform's operator-facing record of
//     which tenants exist, populated two ways -- lazily, from org's real
//     org.node.created events when a tenant's ROOT node is created
//     (go/admin/tenant_service.go's handleOrgNodeCreated), and by an
//     operator's manual CRUD through admin's own HTTP surface. The
//     ledger is the durable, restart-safe half of the universe: a tenant
//     the host never configured -- a self-service clinic this app's own
//     registration flow provisions at runtime (self_service.go) -- is
//     recorded the moment its org root is created, and the record
//     survives the process that created it.
//
// The union exists because the two sources answer different questions.
// cfg.HostTenants is what the host was CONFIGURED with; the ledger is
// what the platform has SEEN. A boot that never creates a configured
// tenant's org root (the demo seeds are opt-in; a flow test that grants
// roster membership without an org tree is the same shape) must still
// sweep that tenant -- that is the configured half's job -- and a
// self-registered clinic is a live tenant no configuration ever names --
// that is the ledger's. Each tick resolves the union fresh
// (tenantIDs), so a tenant that appears after the scheduler started -- a
// clinic provisioned by a register request this process served -- is
// covered by the very next tick, with no reconfiguration and no restart.
type periodicTenantUniverse struct {
	// configured is cfg.HostTenants: the host map whose VALUES are the
	// configured tenants swept, deduplicated, because two hosts can map
	// to one tenant and the sweep is per tenant, not per host (the same
	// dedupe seedDemoGrants performs for grants).
	configured map[string]pkgcore.TenantID
	// ledger is go/admin's D3 tenant-ledger service
	// (adminModule.Tenants()). Its ListAllIDs names every ledger row --
	// "every tenant the platform knows about" -- self-registered clinics
	// included. Always non-nil in this app's composition: BuildServer
	// wires go/admin mandatorily and hands its Tenants() service here.
	ledger *admin.TenantService
}

// newPeriodicTenantUniverse returns the universe over the configured
// host map and the D3 ledger service.
func newPeriodicTenantUniverse(configured map[string]pkgcore.TenantID, ledger *admin.TenantService) *periodicTenantUniverse {
	return &periodicTenantUniverse{configured: configured, ledger: ledger}
}

// tenantIDs resolves the universe at call time: every unique tenant id
// configured hosts map to, then every tenant id the D3 ledger names,
// deduplicated into one sweep set. The ledger read is per-call by
// design -- a tick enqueues once per tenant, so resolving the universe
// once per tick is the whole cost, and a tenant recorded after the
// scheduler started (a clinic provisioned mid-flight) is swept from the
// tick after its ledger row lands.
//
// A ledger read failure is logged and degrades to the configured half
// alone for that tick: the schedule must not lose the tenants it was
// told about because the discovery half was temporarily unreadable
// (SQLite contention, a restart mid-write), and the next tick retries
// the discovery half exactly as it retries a failed enqueue.
func (u *periodicTenantUniverse) tenantIDs(ctx context.Context) []pkgcore.TenantID {
	log := obs.FromContext(ctx)
	seen := make(map[pkgcore.TenantID]struct{}, len(u.configured))
	ids := make([]pkgcore.TenantID, 0, len(u.configured))
	for _, tenantID := range u.configured {
		if _, done := seen[tenantID]; done {
			continue
		}
		seen[tenantID] = struct{}{}
		ids = append(ids, tenantID)
	}
	ledgerIDs, err := u.ledger.ListAllIDs(ctx)
	if err != nil {
		log.Warn("periodic tasks: tenant-ledger read failed, sweeping the configured tenants only; the ledger half retries on the next tick",
			"error", err)
		return ids
	}
	for _, ledgerTenant := range ledgerIDs {
		tenantID := pkgcore.TenantID(ledgerTenant)
		if _, done := seen[tenantID]; done {
			continue
		}
		seen[tenantID] = struct{}{}
		ids = append(ids, tenantID)
	}
	return ids
}

// startPeriodicTaskScheduler starts the loop that enqueues the wired
// periodic mechanisms' tasks every interval (defaultPeriodicTaskSchedulerInterval
// when interval is non-positive -- the injectable cadence exists for the
// flow tests, which drive real ticks within test time, exactly like the
// ServerConfig test-override fields of server.go).
//
// universe is the host's tenant universe, resolved fresh by every tick
// (periodicTenantUniverse's own doc comment): the tenants cfg.HostTenants
// names joined with every tenant go/admin's D3 ledger records, so a
// self-registered clinic provisioned at runtime is swept from the tick
// after its org root's ledger row lands. ctx is the context every tick's
// enqueues run under; like StartReconciler's own ctx it should be
// context.Background() -- a context cancelled by the same shutdown
// sequence that calls the returned stop func would fail every enqueue on
// the final tick.
//
// The returned stop func is idempotent-safe to call exactly once: it
// signals the loop and blocks until the in-flight tick (if any) has
// returned, so the caller knows no enqueue is still running when the
// queue is closed underneath it.
func startPeriodicTaskScheduler(ctx context.Context, interval time.Duration, universe *periodicTenantUniverse, lifecycle *storage.LifecycleService, retention *compliance.RetentionService, signingKeys *pki.Service) func() {
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
				runPeriodicTasks(ctx, universe, lifecycle, retention, signingKeys)
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

// runPeriodicTasks performs one scheduler tick: one expiry-sweep enqueue
// and one retention-sweep enqueue per unique tenant in the universe the
// tick resolves (periodicTenantUniverse.tenantIDs -- the configured host
// tenants joined with the D3 ledger's, so a self-registered clinic is
// enqueued for from the tick after its ledger row lands), then one
// signing-key expiry-scan enqueue (window-scoped, so same-window ticks
// collapse into one job -- see startPeriodicTaskScheduler's doc
// comment). Every enqueue failure is logged and left for the next tick
// to retry -- enqueues are durable row inserts, so a failed one changes
// nothing and the mechanism stays exactly as due as it was.
func runPeriodicTasks(ctx context.Context, universe *periodicTenantUniverse, lifecycle *storage.LifecycleService, retention *compliance.RetentionService, signingKeys *pki.Service) {
	log := obs.FromContext(ctx)
	for _, tenantID := range universe.tenantIDs(ctx) {
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
