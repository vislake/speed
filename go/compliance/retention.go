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
// bootstraps this module never needs to register it by hand.
const SystemPurposeRetentionSweep pkgcore.SystemPurpose = "compliance.retention_sweep"

// AuditActionRetentionSweep is the audit action Module.Register declares
// and RetentionService.SweepTenant emits under, once per sweep run
// (never once per participant): one AuditEvent records the whole pass,
// with Changes carrying the per-participant breakdown.
const AuditActionRetentionSweep = "compliance.retention.sweep"

// defaultRetentionWindow is the retention window a sweep uses for a
// tenant that has never overridden ConfigDefaultRetentionWindow, and the
// window every sweep uses at all when no *config.Service was wired
// through WithConfigService. Thirty days is a deliberate judgment call --
// the common "recycle bin" duration this pattern uses elsewhere in the
// industry, distinct from the audit trail's own pre-archival retention,
// which has a separate default -- and is exactly the kind of value
// ConfigDefaultRetentionWindow exists to let an operator override per
// tenant without a code change.
const defaultRetentionWindow = 30 * 24 * time.Hour

// retentionSweepActor identifies the automated task performing a
// retention sweep, for both the tenancy.WithSystemContext grant's
// SystemReason.Actor and the pkgcore.WithActor carried alongside it --
// see SweepTenant's doc comment for why both are set.
const retentionSweepActor = "compliance.retention_sweep"

// taskTypeRetentionSweep names the jobs queue task EnqueueRetentionSweep
// schedules and retentionSweepHandler claims.
const taskTypeRetentionSweep = "compliance.retention_sweep"

// retentionSweepWindowSize is the period one retention-sweep idempotency
// key covers: a sweep is enqueued under the key of the
// retentionSweepWindowSize window (retentionSweepWindowStart) its enqueue
// falls in, so the same-window duplicates that key exists to collapse
// -- a scheduler with two replicas, a manual re-run -- still
// merge into one job, while an enqueue
// in a later window becomes a NEW job and the sweep runs again. The window
// is what makes the sweep periodic at all: jobs' idempotency is
// unconditional for one key on StandaloneQueue (a resolved key is held
// forever), so a tenant-only key would give each tenant exactly one
// retention sweep per database file -- the pre-window design's recorded
// residual -- and, worse, a sweep job that dead-letters would poison its
// tenant forever, since every later enqueue would keep returning the dead
// job's id. A dead-lettered job now poisons only its own window; the next
// window's enqueue is a fresh key and runs. Retention is a legal
// obligation: soft-deleted rows past their retention window are hard
// deleted within at most one retentionSweepWindowSize of the sweep that
// should have caught them being enqueued, never "whenever the next
// database file happens to exist".
const retentionSweepWindowSize = time.Hour

// retentionSweepWindowStart is the retention-sweep window the enqueue at
// now belongs to -- the absolute hour boundary now.Truncate
// (retentionSweepWindowSize) lands in, the twin of storage's
// expirySweepWindowStart. Two replicas enqueuing within the same window
// share one key (and one job); a tick in a later window gets its own.
// Truncation is on the absolute clock, never a timezone-local calendar
// cut, so every replica agrees on the boundary regardless of its own
// location.
func retentionSweepWindowStart(now time.Time) time.Time {
	return now.Truncate(retentionSweepWindowSize)
}

// retentionSweepKeyPrefix is the prefix of every retention-sweep
// idempotency key. It is a named constant because two derivations must
// agree on it byte for byte: retentionSweepIdempotencyKey below, and the
// declaration (retentionSweepSchedule) a jobs.Scheduler composes keys
// from with its own window derivation -- one window must resolve one key
// through both paths.
const retentionSweepKeyPrefix = "compliance.retention_sweep:"

// retentionSweepIdempotencyKey derives the jobs idempotency key of one
// retention-sweep window for a tenant, mirroring go/storage's
// expirySweepIdempotencyKey: the operation one key names is "the sweep of
// windowStart", not "some sweep or other" -- a periodic task's identity
// inherently includes WHICH period it is for. windowStart is the
// retentionSweepWindowSize window start the enqueue belongs to
// (retentionSweepWindowStart). A scheduler with two replicas, or a manual
// re-run, collapses into one job within one window, so a tenant is never
// swept by two workers at once; a dead-lettered job poisons only its own
// window, and an enqueue in a later window runs the sweep again.
func retentionSweepIdempotencyKey(tenant pkgcore.TenantID, windowStart time.Time) string {
	return retentionSweepKeyPrefix + string(tenant) + ":" + windowStart.UTC().Format(time.RFC3339)
}

// retentionSweepSchedule is the module's declaration of the retention
// sweep on the pkgcore.Registry.Schedules seat: a per-tenant task at the
// sweep's own window, keyed with the same prefix and window function the
// manual EnqueueRetentionSweep path uses, so a scheduler tick and a
// manual enqueue landing in one window resolve one key and dedupe onto
// one job.
var retentionSweepSchedule = pkgcore.PeriodicTask{
	Type:      taskTypeRetentionSweep,
	Every:     retentionSweepWindowSize,
	Scope:     pkgcore.PeriodicScopePerTenant,
	KeyPrefix: retentionSweepKeyPrefix,
}

// TenantLister is a host-supplied, structurally typed seam letting
// RetentionService.SweepAllTenants discover which tenants to sweep,
// without compliance importing org (or any other module that owns a
// tenant directory) -- the same no-import-seam shape as org's own
// Scope/FeatureGate interfaces and config's WithResolver. Only the
// whole-universe SweepAllTenants call needs it: a host that runs a
// jobs.Scheduler sweeps every tenant through the declared per-tenant
// schedule, expanded by the scheduler's own jobs.TenantLister seam, and a
// manual EnqueueRetentionSweep call names its one tenant -- neither path
// needs a compliance TenantLister wired through WithTenantLister.
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
	// A participant absent from this map was never called (should not
	// happen for a completed pass); one present here may also appear in
	// Errors, when its callback reaped rows before failing (see Errors).
	Reaped map[string]int
	// Errors maps participant Name to the error its Sweep callback
	// returned, for participants whose callback failed. A participant
	// present here may still appear in Reaped: a callback that failed
	// part-way through hard-deleted the rows it reports reaping, and
	// TotalReaped -- and the sweep audit's Changes["reaped"] breakdown --
	// must count rows that are genuinely and irreversibly gone even when
	// the callback also errored. This map (with the failure-site log in
	// SweepTenant) is the error text's home: the sweep audit's own
	// Changes["errors"] entry records the classification instead, never
	// the text (see emitSweepAudit).
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

	// now is the clock EnqueueRetentionSweep reads to place the enqueue in
	// its retentionSweepWindowSize window (retentionSweepWindowStart). It
	// is a field, not a time.Now() call at the enqueue site, so the window
	// a sweep is enqueued under is deterministic in tests -- the same
	// clock-seam pattern go/storage's expiry sweep uses -- while
	// defaulting to the real clock for every production call.
	now func() time.Time
}

// newRetentionService returns a RetentionService with no seams wired yet;
// Module.Register attaches the registry's EventBus, AuditActions and
// Retention registrar, and Module's own With* options attach the optional
// *config.Service, TenantLister and jobs.Queue.
func newRetentionService() *RetentionService {
	return &RetentionService{now: time.Now}
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
// carrying the full per-participant reaped breakdown plus a
// classification-only record of which participants failed -- each failed
// participant's name keyed to participantErrorMarker, never the error
// text, whose homes are the returned SweepResult.Errors and the
// failure-site log (see emitSweepAudit). A failure to publish that audit
// event is reported by wrapping ErrAuditRecordFailed -- see that error's
// own doc comment for why it is surfaced rather than swallowed, and why
// it does not erase the SweepResult already computed: SweepTenant returns
// both the SweepResult and the wrapped audit error together in that case,
// never a nil result.
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
		result.Reaped[p.Name] = reaped
		if err != nil {
			// The participant's reported count is still recorded: a
			// callback that failed part-way through has already
			// hard-deleted reaped rows (pkgcore.RetentionParticipant's
			// own contract, and the count this module's testutil
			// participants report on a mid-loop failure), and
			// TotalReaped -- and the audit event's Changes["reaped"]
			// breakdown -- must count rows that are genuinely and
			// irreversibly gone, not silently drop them from the record
			// of the sweep because the callback also errored. This is the
			// identical count-on-error semantics ErasureService.Erase's
			// own accounting applies to ErasureResult.Erased.
			result.Errors[p.Name] = err
			// The participant error's text is logged here -- behind
			// go/observability's redaction layer -- never written into the
			// audit record's Changes, whose errors entry classifies each
			// failed participant by name keyed to participantErrorMarker
			// (see emitSweepAudit, and the erasure path's identical rule):
			// the changes column is effectively permanent, and sweep-path
			// error text can carry internal row or storage details no
			// audit reader was ever promised. This log line and the
			// returned SweepResult.Errors keep the text available to the
			// operator and this call's own caller.
			observability.FromContext(sysCtx).Error("compliance: retention sweep participant failed",
				"participant", p.Name, "error", err)
		}
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
//
// Changes["errors"] carries a classification only -- each failed
// participant's name keyed to participantErrorMarker -- deliberately
// never the participant error's text: dbkit/audit's Diff content contract
// (emit.go) forbids sensitive content in the changes column, which is
// effectively permanent, and sweep-path error text can carry internal row
// or storage details. The error text's homes are the returned
// SweepResult.Errors (this call's own caller) and the structured log at
// the failure site in SweepTenant (behind go/observability's redaction
// layer) -- the identical rule the erasure path's emitErasureAudit
// already follows.
func (s *RetentionService) emitSweepAudit(ctx context.Context, result SweepResult) error {
	changes := map[string]any{"reaped": result.Reaped}
	if result.HasErrors() {
		errs := make(map[string]string, len(result.Errors))
		for name := range result.Errors {
			errs[name] = participantErrorMarker
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
// ctx carries, mirroring go/storage's EnqueueExpirySweep -- the manual
// entry point. The sweep's default schedule is the module's own: Register
// declares it on the pkgcore.Registry.Schedules seat
// (retentionSweepSchedule, a per-tenant task at the sweep's own window),
// so a host that runs a jobs.Scheduler sweeps every tenant without writing
// a schedule point of its own. The enqueue relies on the task's
// window-scoped idempotency key (retentionSweepIdempotencyKey) to collapse
// the enqueues of one retentionSweepWindowSize window into one job. An
// enqueue whose clock has moved into a later window
// (retentionSweepWindowStart) is a new job and runs again -- this is what
// makes the sweep periodic on queues whose
// idempotency is unconditional, and what keeps one dead-lettered sweep
// from poisoning its tenant forever; see retentionSweepIdempotencyKey's
// doc comment for the full window semantics. ctx must carry a tenant
// (pkgcore.WithTenant); with none, this returns a plain error rather than
// guessing one. With no queue wired (WithQueue), it returns
// ErrQueueRequired.
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
		IdempotencyKey: retentionSweepIdempotencyKey(tenant, retentionSweepWindowStart(s.now())),
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
