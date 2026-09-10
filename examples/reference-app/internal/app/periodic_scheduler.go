package app

import (
	"context"

	"github.com/vislake/speed/go/admin"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// The reference app's periodic-task scheduling.
//
// The periodic tasks this app runs are the ones its modules declared: each
// module puts its own schedule on the pkgcore.Registry.Schedules seat in
// its Register call, exactly where it registers the task's handler --
// storage's per-tenant expiry sweep, compliance's per-tenant retention
// sweep, pki's two platform-wide tasks (the signing-key expiry scan and
// CRL regeneration), sharing's and integration's per-tenant expiry
// sweeps. Declaring means scheduled: BuildServer starts one
// jobs.Scheduler over the finished registry's declarations (the job-queue
// block below), and this file supplies the two host-side inputs that
// scheduler needs -- nothing here enqueues anything itself.
//
//   - The tenant universe every per-tenant declaration expands through:
//     periodicTenantUniverse below, the host's configured tenants joined
//     with go/admin's D3 tenant ledger.
//   - The tick cadence: cfg.PeriodicTaskInterval, defaulting (at zero) to
//     jobs.DefaultScheduleInterval.
//
// The scheduler starts and stops on the same cfg.DisableQueueWorker gate
// that guards standaloneQueue.Start -- a task this replica can never
// execute is pointless to enqueue -- and its stop func is what cleanup
// calls before standaloneQueue.Close, so no tick can enqueue against a
// queue that is being torn down.
//
// Each declaration's enqueue carries the module's own window-scoped
// idempotency key (the scheduler derives it from the declaration's key
// prefix, the tenant segment and the window start, byte-identical to what
// the module's Enqueue* method resolves), so same-window ticks -- on this
// app's StandaloneQueue, whose resolved keys are held forever -- collapse
// onto one job per window, a dead-lettered task poisons only its own
// window, and a later window's tick schedules the task again. A tick body
// is synchronous and time.Ticker drops a tick rather than queueing it, so
// a slow tick cannot pile up a backlog; a failed enqueue, or a failed
// tenant-listing read, is logged and retried on the next tick. The
// end-to-end proofs live in flowtests/periodic_scheduler_flow_test.go
// (the storage and compliance two-boot legs),
// periodic_scheduler_flow_clinic_test.go (the self-registered-clinic leg)
// and periodic_pki_scan_flow_test.go (the rotation leg).
//
// pki's CRL-regeneration schedule is declared by the module and therefore
// runs here too, even though this app's verifier paths read row state plus
// the certificate chain and generate CRLs on demand (internal/attestation):
// declaring means scheduled, and the host's lever is whether it starts a
// scheduler at all. billing's payment-poll schedule is absent by the same
// rule from the other side -- this app wires billing without a queue (see
// server.go's billing wiring note), so billing registers neither the poll
// handler nor its declaration.
//
// periodicTenantUniverse answers the question every per-tenant declaration
// expands through -- "which tenants does this host serve?" -- from
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
// that is the ledger's. The universe resolves fresh on every call
// (ListTenants), so a tenant that appears after the scheduler started -- a
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

// ListTenants implements the tenant-lister seam the scheduler expands
// per-tenant declarations through (jobs.TenantLister -- structurally the
// same shape go/compliance's own TenantLister declares, so this one value
// serves both seams): every unique tenant id configured hosts map to,
// then every tenant id the D3 ledger names, deduplicated into one sweep
// set.
//
// The ledger read is per-call by design -- a tick enqueues once per
// tenant, so resolving the universe once per tick is the whole cost, and
// a tenant recorded after the scheduler started (a clinic provisioned
// mid-flight) is swept from the tick after its ledger row lands.
//
// A ledger read failure is logged and degrades to the configured half
// alone for that tick: the schedule must not lose the tenants it was told
// about because the discovery half was temporarily unreadable (SQLite
// contention, a restart mid-write), and the next tick retries the
// discovery half exactly as it retries a failed enqueue.
func (u *periodicTenantUniverse) ListTenants(ctx context.Context) ([]pkgcore.TenantID, error) {
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
		return ids, nil
	}
	for _, ledgerTenant := range ledgerIDs {
		tenantID := pkgcore.TenantID(ledgerTenant)
		if _, done := seen[tenantID]; done {
			continue
		}
		seen[tenantID] = struct{}{}
		ids = append(ids, tenantID)
	}
	return ids, nil
}
