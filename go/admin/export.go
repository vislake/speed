package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// jobTypeAuditExport is the jobs.Task.Type this module registers its
// export handler under (Module.Register's reg.Jobs.Handle call).
const jobTypeAuditExport = "admin.audit_export"

// exportJobResult is what ExportService.Handle marshals into
// jobs.Result.Data once an export's work completes -- fully, or partially
// with the failing participants recorded in the audit event rather than
// the queue (see Handle's own doc comment): the object key the manifest
// was stored under, and the go/sharing delivery minted for it (share id
// and expiry) -- the fields compliance.ExportResult/ExportDelivery
// already carry minus the one field that must never be stored, re-shaped
// as a stable, explicitly-tagged JSON document (rather than re-using
// compliance's own Go types directly) so a caller decoding
// jobs.Job.Result.Data has one well-known wire shape regardless of how
// compliance's own internal types evolve.
//
// The omitted field is the one-time delivery token. It is a bearer
// credential -- go/sharing's whole design stores only its hash, and
// compliance's ExportDelivery doc comment promises it is "returned
// exactly once and never persisted anywhere, including here" -- while
// jobs.Result.Data is persisted with the job record itself, so
// marshalling it here would put the credential at rest in the jobs
// table. Handle receives the token in Export's synchronous return value
// and deliberately lets it die in its own frame: the stored result keeps
// the facts a later retrieval needs (what was exported, which share it
// was delivered through, when it expires), and a caller that must hand
// the download link to its recipient receives the token only through a
// synchronous return channel -- the shape compliance.Export's own return
// already provides -- never from a stored job record. No admin surface
// provides such a synchronous channel yet (Enqueue returns only the job
// id, see Enqueue's doc comment); one must exist before any operator can
// actually relay a link, which is future surface work recorded here
// rather than an excuse to ship the credential at rest.
type exportJobResult struct {
	ObjectKey string    `json:"object_key"`
	ShareID   string    `json:"share_id"`
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
	// call Handle makes once an export's work actually completes -- fully
	// or partially (P1-2's fix, widened by P2-4 to the partial outcome) --
	// the same "explicit Emit over automatic write capture" shape
	// ImpersonationService.recordAudit uses, for the identical reason:
	// this module owns no row of its own to auto-capture a write against.
	// Nil until Module.Register calls attachAudit, tolerated by skipping
	// the audit side effect exactly like every other seam in this module
	// before Register runs.
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar

	// authnSvc resolves the requesting operator's display name onto the
	// ctx actor Handle installs (see Handle and resolveActorName). Nil
	// until Module.Register calls attachAudit; WithAuthn is a mandatory
	// production option, so this is never nil in a correctly wired
	// Bootstrap, and a nil-seam unit fixture records id-only actors
	// exactly as before.
	authnSvc *authn.Service
}

// NewExportService returns an ExportService calling export and enqueuing
// through queue.
func NewExportService(export *compliance.ExportService, queue jobs.Queue) *ExportService {
	return &ExportService{export: export, queue: queue}
}

// attachAudit gives the service the bus and audit-action registry Handle
// needs to record admin.audit_export -- plus the *authn.Service whose
// users table Handle reads the requesting operator's display name from
// (resolveActorName's own doc comment) -- all read from the host's
// *pkgcore.Registry (and authn module) during Module.Register.
func (s *ExportService) attachAudit(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, authnSvc *authn.Service) {
	s.bus = bus
	s.auditActions = actions
	s.authnSvc = authnSvc
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
// caller polling jobs.Queue.Get to retrieve later -- the object key, the
// minted share id and its expiry, deliberately never the one-time
// delivery token (P1-B: the token is a bearer credential that must not
// be persisted with the job record; see exportJobResult's own doc
// comment for why it dies in this frame instead).
//
// Export's own ErrExportPartialFailure (some participant's data could
// not be gathered, but the rest was gathered, stored and delivered) is
// deliberately NOT returned as this call's error: partial failure is
// terminal handling for this job type (P2-4). Export returns that error
// only AFTER its work is done -- the manifest is stored, the single-view
// share is minted, compliance's own audit event is out -- so surfacing it
// as a Handle error would hand the queue a side-effectful, non-idempotent
// operation to retry: every retried attempt mints a fresh object key and
// a fresh share, piling up delivered dumps while admin's own audit
// record stays silent. Handle instead completes the job (StatusSucceeded,
// with the partial result -- object key, share id, expiry -- recorded in
// jobs.Result.Data exactly like a full export's) and records the partial
// outcome in its own admin.audit_export event: Success false, the failing
// participants named, the delivered result in Changes (recordAudit's own
// doc comment). An error Export returns BEFORE its work completed -- a
// refused or mis-scoped request, a storage or delivery failure whose
// manifest Export itself deleted, an audit-write failure Export's own
// contract says to surface for operator attention -- still fails the
// attempt and rides the queue's retry budget like any other Handle
// failure, since none of those left delivered work behind to duplicate.
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
	//
	// P2-pkgcore-actor-1: the actor is resolved against the users table
	// here (resolveActorName) before it is layered on, so both this
	// event and compliance's own carry the operator's display name, not
	// an id-only Actor -- the worker context rebuilt from the job record
	// has no other channel to learn it from, and the payload carries only
	// the id Enqueue's HTTP caller resolved.
	if payload.OperatorUserID != "" {
		ctx = pkgcore.WithActor(ctx, resolveActorName(ctx, s.authnSvc,
			pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: payload.OperatorUserID}))
	}

	result, err := s.export.Export(ctx, job.TenantID)
	if err != nil && !isExportPartialFailure(err) {
		return jobs.Result{}, err
	}

	// Full success and partial failure converge here: both mean the
	// export's work actually completed and was delivered, so both record
	// admin.audit_export and marshal the delivered result into the job's
	// Result -- the partial failure never reaches the queue as an error
	// (see Handle's own doc comment for why that would amplify delivered
	// dumps instead of alerting anyone). failureReason is empty on a full
	// success and names the failing participants on a partial one.
	failureReason := ""
	if err != nil {
		failureReason = auditExportFailureReason(result.Manifest)
	}
	s.recordAudit(ctx, job.TenantID, result, failureReason)

	encoded, marshalErr := json.Marshal(exportJobResult{
		ObjectKey: result.ObjectKey,
		ShareID:   result.Delivery.ShareID,
		ExpiresAt: result.Delivery.ExpiresAt,
	})
	if marshalErr != nil {
		return jobs.Result{}, marshalErr
	}
	return jobs.Result{Data: encoded}, nil
}

// isExportPartialFailure reports whether err is compliance's
// ErrExportPartialFailure -- the one error Export returns only after the
// export's work itself completed (the manifest gathered minus the failing
// participants, stored and delivered through go/sharing). Every other
// error Export can return precedes or aborts that work.
func isExportPartialFailure(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == compliance.ErrExportPartialFailure.Code
}

// auditExportFailureReason renders the failure-reason text admin's own
// admin.audit_export event carries for a partial export, naming the
// participants whose Export callback failed -- the same vocabulary
// compliance's own export event uses for its FailureReason
// (compliance.exportFailureReason), so the two events an operator reads
// for one export agree. Empty when the manifest carries no errors.
func auditExportFailureReason(manifest compliance.ExportManifest) string {
	if len(manifest.Errors) == 0 {
		return ""
	}
	names := make([]string, 0, len(manifest.Errors))
	for name := range manifest.Errors {
		names = append(names, name)
	}
	sort.Strings(names)
	return "participants failed: " + strings.Join(names, ", ")
}

// recordAudit emits admin.audit_export once a tenant's audit-event export
// has actually completed -- result is the export's delivered outcome
// Export returned, which this event records (Changes carries the object
// key and, when a share was minted, its id and expiry) rather than
// discarding on the way out, and failureReason is empty on a full
// success or names the failing participants on a partial one, mirrored
// into the event's Result (Success false with that reason). P2-4: on the
// unfixed code the event only ever reported Success true and carried no
// result at all -- and on a partial export it never fired, because the
// error return sat before the recordAudit call.
//
// The operator who asked for the export (Enqueue's operatorUserID,
// carried through the job's own payload, and already attached to ctx as
// Actor by Handle before Export ran) is the Actor -- an ordinary,
// single-identity attribution, never OnBehalfOf: admin's own routes
// deliberately never sit behind ImpersonationMiddleware (AGENTS.md's
// "The impersonation request pipeline" section), so callerUserID always
// names the REAL calling operator here, exactly like
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
func (s *ExportService) recordAudit(ctx context.Context, tenantID pkgcore.TenantID, result *compliance.ExportResult, failureReason string) {
	if s.bus == nil {
		return
	}
	after := map[string]any{
		"object_key": result.ObjectKey,
	}
	if result.Delivery.ShareID != "" {
		after["share_id"] = result.Delivery.ShareID
		after["share_expires_at"] = result.Delivery.ExpiresAt
	}
	err := audit.Emit(ctx, s.bus, s.auditActions, audit.Input{
		Action:   AuditActionAuditExport,
		Resource: audit.Resource{Type: "admin.tenant", ID: string(tenantID)},
		Result:   audit.Result{Success: failureReason == "", FailureReason: failureReason},
		Changes:  &audit.Diff{After: after},
	})
	if err != nil {
		obs.FromContext(ctx).Warn("admin failed to record an audit-export audit event",
			"tenant_id", tenantID, "error", err)
	}
}

// compile-time check that *ExportService satisfies jobs.Handler.
var _ jobs.Handler = (*ExportService)(nil)
