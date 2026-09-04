package compliance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// SystemPurposeRetentionSweep is the audited system purpose
// RetentionService.SweepTenant enters (via tenancy.WithSystemContext)
// before calling any registered participant's Sweep callback. Module.
// Register calls pkgcore.RegisterSystemPurpose with it, so a host that
// bootstraps this module never needs to register it by hand -- mirroring
// go/config's identical SystemPurposeSystemWrite convention.
const SystemPurposeRetentionSweep pkgcore.SystemPurpose = "compliance.retention_sweep"

// AuditActionRetentionSweep is the audit action Module.Register declares
// and RetentionService.SweepTenant emits under, once per sweep run
// (never once per participant): one AuditEvent records the whole pass,
// with Changes carrying the per-participant breakdown.
const AuditActionRetentionSweep = "compliance.retention.sweep"

// defaultRetentionWindow is the retention window a sweep uses for a
// tenant that has never overridden ConfigDefaultRetentionWindow, and the
// window every sweep uses at all when no *config.Service was wired
// through WithConfigService. Thirty days is a deliberate judgment call
// for this round -- docs/internal/10-compliance-and-audit.md gives an
// explicit default (one year) for a different retention window, the
// audit trail's own pre-archival retention, not the mark-delete-to-hard-
// delete window this constant governs -- chosen as the common "recycle
// bin" duration this pattern uses elsewhere in the industry, and is
// exactly the kind of value ConfigDefaultRetentionWindow exists to let an
// operator override per tenant without a code change.
const defaultRetentionWindow = 30 * 24 * time.Hour

// retentionSweepActor identifies the automated task performing a
// retention sweep, for both the tenancy.WithSystemContext grant's
// SystemReason.Actor and the pkgcore.WithActor carried alongside it --
// see SweepTenant's doc comment for why both are set.
const retentionSweepActor = "compliance.retention_sweep"

// taskTypeRetentionSweep names the jobs queue task EnqueueRetentionSweep
// schedules and retentionSweepHandler claims.
const taskTypeRetentionSweep = "compliance.retention_sweep"

// retentionSweepIdempotencyKey derives one tenant's retention-sweep task
// idempotency key from the tenant id, mirroring go/storage's
// expirySweepIdempotencyKey: a scheduler with two replicas, or a manual
// re-run, collapses into one job, so a tenant is never swept by two
// workers at once.
func retentionSweepIdempotencyKey(tenant pkgcore.TenantID) string {
	return "compliance.retention_sweep:" + string(tenant)
}

// TenantLister is a host-supplied, structurally typed seam letting
// RetentionService.SweepAllTenants discover which tenants to sweep,
// without compliance importing org (or any other module that owns a
// tenant directory) -- the same no-import-seam shape as org's own
// Scope/FeatureGate interfaces and config's WithResolver. A host that
// only ever schedules one tenant's sweep at a time (EnqueueRetentionSweep
// called once per tenant from its own platform loop, the same shape
// go/storage's EnqueueExpirySweep documents) needs no TenantLister at
// all.
type TenantLister interface {
	// ListTenants returns every tenant a retention sweep should cover.
	// ctx carries no tenant of its own -- this is inherently a cross-
	// tenant enumeration -- so an implementation must not require one.
	ListTenants(ctx context.Context) ([]pkgcore.TenantID, error)
}

// SweepResult is SweepTenant's outcome: how many rows each registered
// participant reaped, and any per-participant error that did not stop the
// pass -- Sweep runs every participant regardless of an earlier one's
// failure (see SweepTenant's doc comment), so a caller must consult
// Errors rather than assume a nil top-level error means every participant
// succeeded.
type SweepResult struct {
	// Tenant is the tenant swept.
	Tenant pkgcore.TenantID
	// Cutoff is the time.Time every participant's Sweep callback was
	// asked to reap soft-deleted rows at or before.
	Cutoff time.Time
	// Reaped maps participant Name to how many rows it reported reaping.
	// A participant absent from this map either errored (see Errors) or
	// was never called (should not happen for a completed pass).
	Reaped map[string]int
	// Errors maps participant Name to the error its Sweep callback
	// returned, for participants whose callback failed. A participant
	// present here reaped 0 rows this pass, whatever partial progress its
	// own callback may or may not have made internally.
	Errors map[string]error
}

// TotalReaped sums Reaped across every participant.
func (r SweepResult) TotalReaped() int {
	total := 0
	for _, n := range r.Reaped {
		total += n
	}
	return total
}

// HasErrors reports whether any participant's Sweep callback failed.
func (r SweepResult) HasErrors() bool { return len(r.Errors) > 0 }

// RetentionService runs the retention-window sweep: for one tenant, at a
// cutoff derived from the tenant's configured retention window, it calls
// every pkgcore.RetentionParticipant registered on the host's
// pkgcore.Registry.Retention and asks each one to hard-delete its own
// model's soft-deleted rows older than the cutoff. It never touches a
// participant's table directly -- see this module's own doc comment for
// why that is the whole point of the design.
//
// The zero value is not ready to use; construct one with
// newRetentionService (Module.NewModule's constructor calls it) and wire
// it into a live pkgcore.Registry through Module.Register.
type RetentionService struct {
	retention pkgcore.RetentionRegistrar
	bus       pkgcore.EventBus
	actions   pkgcore.AuditActionRegistrar
	cfg       *config.Service
	lister    TenantLister
	queue     jobs.Queue
}

// newRetentionService returns a RetentionService with no seams wired yet;
// Module.Register attaches the registry's EventBus, AuditActions and
// Retention registrar, and Module's own With* options attach the optional
// *config.Service, TenantLister and jobs.Queue.
func newRetentionService() *RetentionService {
	return &RetentionService{}
}

// RetentionWindow resolves the retention window a sweep should use for
// tenant: the tenant's own ConfigDefaultRetentionWindow override when one
// is set and a *config.Service was wired (WithConfigService), the
// schema's platform default otherwise, and defaultRetentionWindow when no
// config service was wired at all -- see ErrConfigServiceRequired's doc
// comment for why that last case is a fallback here rather than an error;
// SweepTenant itself never fails merely because no config service was
// wired.
func (s *RetentionService) RetentionWindow(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, error) {
	if s.cfg == nil {
		return defaultRetentionWindow, nil
	}
	tenantCtx := pkgcore.WithTenant(ctx, tenant)
	window, err := config.GetTyped[time.Duration](s.cfg, tenantCtx, ConfigDefaultRetentionWindow)
	if err != nil {
		return 0, err
	}
	if window <= 0 {
		return defaultRetentionWindow, nil
	}
	return window, nil
}

// SweepTenant runs one retention-window sweep pass for tenant: it
// resolves the tenant's retention window (RetentionWindow), computes a
// single cutoff = now - window shared by every participant so the whole
// pass agrees on what "expired" means, enters an audited system context
// scoped to tenant (tenancy.WithSystemContext, purpose
// SystemPurposeRetentionSweep), and calls every registered participant's
// Sweep callback with (ctx, tenant, cutoff) in registration order.
//
// A participant's Sweep failing does not stop the pass: every other
// participant still runs, and the failure is recorded in the returned
// SweepResult.Errors rather than aborting -- a soft-deleted row belonging
// to a participant whose table is temporarily unreachable should not
// block a healthy participant's own cleanup, and SweepResult's aggregate
// view is what lets a caller (or a retry) target exactly the participants
// that need it. Once every participant has run, SweepTenant returns
// ErrSweepPartialFailure alongside the full SweepResult (never a nil
// result) whenever SweepResult.HasErrors() is true, so a caller checking
// only "err != nil" still learns that something needs attention rather
// than mistaking a partial pass for a clean one. SweepTenant is safe to
// call repeatedly for the same
// (tenant, cutoff-ish) window: every participant's Sweep callback is
// itself documented to be safely re-runnable (pkgcore.RetentionParticipant's
// doc comment), so a second pass over already-reaped rows finds nothing
// left to reap and reports 0, not an error.
//
// Attribution: SweepTenant sets pkgcore.WithActor on ctx (a system Actor
// naming this task) *before* entering system context, because dbkit's
// HardDelete doc comment (go/dbkit/hard_delete.go) warns that a bare
// system context supplies no Actor of its own -- the audit-capture
// plugin's write on each participant's own table would otherwise be
// attributed to the zero Actor. A caller's own ctx Actor, if it already
// set one, is left untouched only when this call is itself made from
// inside an already-actor-carrying context; SweepTenant always sets its
// own system Actor for a bare sweep run, since a scheduled sweep has no
// human caller to attribute to.
//
// The whole pass, once every participant has run, is recorded as exactly
// one AuditActionRetentionSweep audit event (never one per participant)
// via dbkit/audit.Emit, with Resource naming the tenant and Changes
// carrying the full per-participant reaped/error breakdown. A failure to
// publish that audit event is reported by wrapping ErrAuditRecordFailed --
// see that error's own doc comment for why it is surfaced rather than
// swallowed, and why it does not erase the SweepResult already computed:
// SweepTenant returns both the SweepResult and the wrapped audit error
// together in that case, never a nil result.
func (s *RetentionService) SweepTenant(ctx context.Context, tenant pkgcore.TenantID) (SweepResult, error) {
	now := time.Now()
	window, err := s.RetentionWindow(ctx, tenant)
	if err != nil {
		return SweepResult{}, err
	}
	cutoff := now.Add(-window)

	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{
		Type:        pkgcore.ActorTypeSystem,
		ID:          retentionSweepActor,
		DisplayName: "Compliance Retention Sweep",
	})
	ctx = pkgcore.WithTenant(ctx, tenant)
	sysCtx, err := tenancy.WithSystemContext(ctx, s.bus, pkgcore.SystemReason{
		Actor:   retentionSweepActor,
		Purpose: SystemPurposeRetentionSweep,
	})
	if err != nil {
		return SweepResult{}, err
	}

	result := SweepResult{
		Tenant: tenant,
		Cutoff: cutoff,
		Reaped: make(map[string]int),
		Errors: make(map[string]error),
	}
	for _, p := range s.retention.Participants() {
		if p.Sweep == nil {
			continue
		}
		reaped, err := p.Sweep(sysCtx, tenant, cutoff)
		if err != nil {
			result.Errors[p.Name] = err
			continue
		}
		result.Reaped[p.Name] = reaped
	}

	if err := s.emitSweepAudit(ctx, result); err != nil {
		return result, ErrAuditRecordFailed.WithCause(err)
	}
	if result.HasErrors() {
		return result, ErrSweepPartialFailure.WithParam("participants", sweepFailureReason(result))
	}
	return result, nil
}

// emitSweepAudit records one AuditActionRetentionSweep event for a
// completed pass, using dbkit/audit.Emit -- ctx here is the pre-elevation
// context (still carrying the sweep's own system Actor and the tenant),
// matching how tenancy.WithSystemContext itself publishes its own
// SystemContextEntered event against the original, non-elevated ctx.
func (s *RetentionService) emitSweepAudit(ctx context.Context, result SweepResult) error {
	changes := map[string]any{"reaped": result.Reaped}
	if result.HasErrors() {
		errs := make(map[string]string, len(result.Errors))
		for name, err := range result.Errors {
			errs[name] = err.Error()
		}
		changes["errors"] = errs
	}
	return audit.Emit(ctx, s.bus, s.actions, audit.Input{
		Action: AuditActionRetentionSweep,
		Resource: audit.Resource{
			Type: "compliance.tenant",
			ID:   string(result.Tenant),
		},
		Result: audit.Result{
			Success:       !result.HasErrors(),
			FailureReason: sweepFailureReason(result),
		},
		Changes: &audit.Diff{After: changes},
	})
}

// sweepFailureReason renders a short summary of which participants failed
// a sweep, for audit.Result.FailureReason. Empty when result has no
// errors.
func sweepFailureReason(result SweepResult) string {
	if !result.HasErrors() {
		return ""
	}
	names := make([]string, 0, len(result.Errors))
	for name := range result.Errors {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("participants failed: %s", strings.Join(names, ", "))
}

// SweepAllTenants sweeps every tenant TenantLister.ListTenants returns,
// in the order it returns them, aggregating each tenant's SweepResult.
// One tenant's sweep failing (its RetentionWindow lookup, its system-
// context grant, or its audit publish) does not stop the others -- the
// per-tenant isolation this method exists to prove is exactly that one
// tenant's trouble never blocks another's cleanup -- but the aggregate
// error is non-nil whenever any tenant's own SweepTenant call returned
// one, so a caller that only checks the top-level error still learns that
// something needs attention.
//
// SweepAllTenants returns ErrTenantListerRequired, touching nothing, when
// no TenantLister was wired through WithTenantLister -- SweepTenant
// itself needs none, since it already knows which single tenant to sweep.
func (s *RetentionService) SweepAllTenants(ctx context.Context) (map[pkgcore.TenantID]SweepResult, error) {
	if s.lister == nil {
		return nil, ErrTenantListerRequired
	}
	tenants, err := s.lister.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	results := make(map[pkgcore.TenantID]SweepResult, len(tenants))
	var failed []string
	for _, tenant := range tenants {
		result, err := s.SweepTenant(ctx, tenant)
		results[tenant] = result
		if err != nil {
			failed = append(failed, string(tenant))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return results, fmt.Errorf("compliance: retention sweep failed for tenants: %s", strings.Join(failed, ", "))
	}
	return results, nil
}

// EnqueueRetentionSweep enqueues the retention-sweep task for the tenant
// ctx carries, mirroring go/storage's EnqueueExpirySweep: the host-facing
// schedule point a platform loop calls once per tenant on its own timer,
// relying on the task's per-tenant idempotency key to collapse concurrent
// enqueues into one job. ctx must carry a tenant (pkgcore.WithTenant);
// with none, this returns a plain error rather than guessing one. With no
// queue wired (WithQueue), it returns ErrQueueRequired.
func (s *RetentionService) EnqueueRetentionSweep(ctx context.Context) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return fmt.Errorf("compliance: enqueue retention sweep: %w", err)
	}
	if s.queue == nil {
		return ErrQueueRequired
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeRetentionSweep,
		TenantID:       tenant,
		IdempotencyKey: retentionSweepIdempotencyKey(tenant),
	})
	return err
}

// retentionSweepHandler is the jobs.Handler claiming
// taskTypeRetentionSweep, the task EnqueueRetentionSweep schedules. Its
// Handle runs RetentionService.SweepTenant on the tenant context the
// worker rebuilt from the task.
type retentionSweepHandler struct {
	svc *RetentionService
}

// Type implements jobs.Handler.
func (h retentionSweepHandler) Type() string { return taskTypeRetentionSweep }

// Handle implements jobs.Handler. The task carries no payload -- a sweep
// takes its inputs from the rows, the tenant's configured window and the
// clock at run time.
func (h retentionSweepHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, fmt.Errorf("compliance: retention-sweep task carries an unexpected payload")
	}
	result, err := h.svc.SweepTenant(ctx, job.TenantID)
	if err != nil {
		return jobs.Result{}, err
	}
	observability.FromContext(ctx).Info("retention sweep completed",
		"tenant_id", string(job.TenantID), "reaped", result.TotalReaped())
	return jobs.Result{}, nil
}

// compile-time check that retentionSweepHandler satisfies jobs.Handler.
var _ jobs.Handler = retentionSweepHandler{}
