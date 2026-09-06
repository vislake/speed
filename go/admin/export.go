package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// jobTypeAuditExport is the jobs.Task.Type this module registers its
// export handler under (Module.Register's reg.Jobs.Handle call).
const jobTypeAuditExport = "admin.audit_export"

// exportJobResult is what ExportService.Handle marshals into
// jobs.Result.Data on a successful export: the object key the manifest
// was stored under, and the go/sharing delivery minted for it -- the same
// fields compliance.ExportResult/ExportDelivery already carry, re-shaped
// as a stable, explicitly-tagged JSON document (rather than re-using
// compliance's own Go types directly) so a caller decoding
// jobs.Job.Result.Data has one well-known wire shape regardless of how
// compliance's own internal types evolve.
type exportJobResult struct {
	ObjectKey string    `json:"object_key"`
	ShareID   string    `json:"share_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// exportJobPayload is Enqueue's job payload: the one fact Handle needs that
// is not already carried by jobs.Job itself -- which operator asked for
// this export (P1-2's fix). It travels the same way every other job
// payload in this codebase does (notification.Dispatch, say): marshaled by
// Enqueue, unmarshaled by Handle, since the worker's ctx is rebuilt from
// the job record alone (root CLAUDE.md's "workers do not inherit tenant
// context" trap applies identically to operator identity -- nothing about
// the enqueuing request's own ctx survives to the worker).
type exportJobPayload struct {
	// OperatorUserID is the platform operator who asked for this export --
	// resolved by the HTTP handler from the caller's own verified
	// Principal (callerUserID), exactly like every other admin write path,
	// never accepted as a request field. Enqueue refuses to proceed
	// without one (ErrExportOperatorRequired), so a Job this module
	// creates always carries a non-empty value here.
	OperatorUserID string `json:"operator_user_id"`
}

// ExportService is D7's export-leg runtime: an asynchronous kickoff over
// compliance.ExportService.Export, going through go/jobs rather than
// running synchronously inside the HTTP request -- root CLAUDE.md's
// asynchronous-work discipline ("Long-running operations must go through
// the jobs queue and report progress; never run them synchronously inside
// an HTTP request"), the identical shape every other long-running
// operation in this codebase already takes (go/storage's derive/expiry
// jobs, go/notification's delivery jobs, go/ai-gateway's image-generation
// job).
//
// It is also a jobs.Handler: Module.Register registers it on
// reg.Jobs.Handle(jobTypeAuditExport, ...) directly, the same "the
// service IS its own handler" shape go/notification's DeliveryService
// takes, rather than a separate handler type wrapping it.
type ExportService struct {
	export *compliance.ExportService
	queue  jobs.Queue

	// bus and auditActions back the explicit admin.audit_export audit.Emit
	// call Handle makes once an export actually completes (P1-2's fix) --
	// the same "explicit Emit over automatic write capture" shape
	// ImpersonationService.recordAudit uses, for the identical reason:
	// this module owns no row of its own to auto-capture a write against.
	// Nil until Module.Register calls attachAudit, tolerated by skipping
	// the audit side effect exactly like every other seam in this module
	// before Register runs.
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
}

// NewExportService returns an ExportService calling export and enqueuing
// through queue.
func NewExportService(export *compliance.ExportService, queue jobs.Queue) *ExportService {
	return &ExportService{export: export, queue: queue}
}

// attachAudit gives the service the bus and audit-action registry Handle
// needs to record admin.audit_export, both read from the host's
// *pkgcore.Registry during Module.Register.
func (s *ExportService) attachAudit(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar) {
	s.bus = bus
	s.auditActions = actions
}

// Enqueue validates tenantID and operatorUserID and enqueues one go/jobs
// task that will run compliance.ExportService.Export for it when a worker
// picks it up, returning the job's id immediately -- never blocking on
// Export itself, which gathers every registered compliance participant's
// data, stores it and delivers it through go/sharing, none of which
// belongs inside an HTTP request's own timeout budget.
//
// operatorUserID is the platform operator who asked for this export --
// P1-2's fix for the one admin write path that used to attribute nobody at
// all. It travels inside the job's own payload (exportJobPayload), since
// the worker's ctx is rebuilt from the job record alone and carries
// nothing of the enqueuing request's own context (root CLAUDE.md's
// "workers do not inherit tenant context" trap, applied identically to
// operator identity) -- Handle decodes it back out and sets it as the
// audit event's Actor once the export completes.
func (s *ExportService) Enqueue(ctx context.Context, tenantID, operatorUserID string) (jobs.JobID, error) {
	if tenantID == "" {
		return "", ErrTenantIDRequired
	}
	if operatorUserID == "" {
		return "", ErrExportOperatorRequired
	}
	payload, err := json.Marshal(exportJobPayload{OperatorUserID: operatorUserID})
	if err != nil {
		return "", fmt.Errorf("admin: marshal audit export job payload: %w", err)
	}
	return s.queue.Enqueue(ctx, jobs.Task{
		Type:     jobTypeAuditExport,
		TenantID: pkgcore.TenantID(tenantID),
		Payload:  payload,
	})
}

// Type implements jobs.Handler.
func (s *ExportService) Type() string { return jobTypeAuditExport }

// Handle implements jobs.Handler: ctx already carries job.TenantID
// (pkgcore.WithTenant, rebuilt by the worker -- jobs.Handler.Handle's own
// doc comment), so this decodes job.Payload for the operator identity
// Enqueue carried, forwards to compliance.ExportService.Export, records
// admin.audit_export once the export actually completes (P1-2's fix --
// see recordAudit's own doc comment for exactly what is and is not
// covered), and marshals Export's outcome into the job's Result for a
// caller polling jobs.Queue.Get to retrieve later.
//
// Export's own ErrExportPartialFailure (some participant's data could not
// be gathered, but the rest was still delivered) is returned as this
// call's error too -- the job still ends StatusDeadLetter or
// StatusRetrying for it exactly as any other Handle failure would,
// consistent with jobs' own retry semantics, while
// ExportService.Export's already-completed, already-delivered manifest is
// not undone by that -- a caller inspecting the export result (once a
// later round adds a way to retrieve it) still finds what was gathered.
// A payload that fails to decode is itself a Handle failure -- the queue
// retries and then dead-letters it, the honest outcome for a payload that
// slipped past Enqueue's own validation.
func (s *ExportService) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var payload exportJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("admin: decode audit export job payload: %w", err)
	}

	// The operator is attached to ctx itself, before compliance's own
	// Export runs -- not only to a separate ctx used later for this
	// module's own admin.audit_export emission. compliance.ExportService.Export
	// fires its own always-on compliance.export.request audit event
	// (compliance's export.go) that reads Actor from this same ctx via
	// pkgcore.ActorFromContext; without this, that event would carry a
	// zero Actor for every admin-triggered export even though the
	// operator identity was known and available the whole time.
	if payload.OperatorUserID != "" {
		ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: payload.OperatorUserID})
	}

	result, err := s.export.Export(ctx, job.TenantID)
	if err != nil {
		return jobs.Result{}, err
	}
	s.recordAudit(ctx, job.TenantID)

	encoded, marshalErr := json.Marshal(exportJobResult{
		ObjectKey: result.ObjectKey,
		ShareID:   result.Delivery.ShareID,
		Token:     result.Delivery.Token,
		ExpiresAt: result.Delivery.ExpiresAt,
	})
	if marshalErr != nil {
		return jobs.Result{}, marshalErr
	}
	return jobs.Result{Data: encoded}, nil
}

// recordAudit emits admin.audit_export once a tenant's audit-event export
// has actually completed, with the operator who asked for it (Enqueue's
// operatorUserID, carried through the job's own payload, and already
// attached to ctx as Actor by Handle before Export ran) as Actor -- an
// ordinary, single-identity attribution, never OnBehalfOf: admin's own
// routes deliberately never sit behind ImpersonationMiddleware
// (AGENTS.md's "The impersonation request pipeline" section), so
// callerUserID always names the REAL calling operator here, exactly like
// TenantService.SetStatus's own audit event, never a substituted
// impersonation target.
//
// It is a no-op before Module.Register attaches a bus (the same
// pre-Register tolerance every other explicit Emit call in this module
// has), and a publish failure is logged and swallowed: the export itself
// already succeeded and already delivered by the time this runs, so
// surfacing an audit failure as this call's own error would report a
// failure that did not happen -- matching
// ImpersonationService.recordAudit's identical reasoning.
func (s *ExportService) recordAudit(ctx context.Context, tenantID pkgcore.TenantID) {
	if s.bus == nil {
		return
	}
	err := audit.Emit(ctx, s.bus, s.auditActions, audit.Input{
		Action:   AuditActionAuditExport,
		Resource: audit.Resource{Type: "admin.tenant", ID: string(tenantID)},
		Result:   audit.Result{Success: true},
	})
	if err != nil {
		obs.FromContext(ctx).Warn("admin failed to record an audit-export audit event",
			"tenant_id", tenantID, "error", err)
	}
}

// compile-time check that *ExportService satisfies jobs.Handler.
var _ jobs.Handler = (*ExportService)(nil)
