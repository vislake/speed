package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/jobs"
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
}

// NewExportService returns an ExportService calling export and enqueuing
// through queue.
func NewExportService(export *compliance.ExportService, queue jobs.Queue) *ExportService {
	return &ExportService{export: export, queue: queue}
}

// Enqueue validates tenantID and enqueues one go/jobs task that will run
// compliance.ExportService.Export for it when a worker picks it up,
// returning the job's id immediately -- never blocking on Export itself,
// which gathers every registered compliance participant's data, stores it
// and delivers it through go/sharing, none of which belongs inside an
// HTTP request's own timeout budget.
func (s *ExportService) Enqueue(ctx context.Context, tenantID string) (jobs.JobID, error) {
	if tenantID == "" {
		return "", ErrTenantIDRequired
	}
	return s.queue.Enqueue(ctx, jobs.Task{
		Type:     jobTypeAuditExport,
		TenantID: pkgcore.TenantID(tenantID),
	})
}

// Type implements jobs.Handler.
func (s *ExportService) Type() string { return jobTypeAuditExport }

// Handle implements jobs.Handler: ctx already carries job.TenantID
// (pkgcore.WithTenant, rebuilt by the worker -- jobs.Handler.Handle's own
// doc comment), so this simply forwards to
// compliance.ExportService.Export and marshals its outcome into the
// job's Result for a caller polling jobs.Queue.Get to retrieve later.
// Export's own ErrExportPartialFailure (some participant's data could not
// be gathered, but the rest was still delivered) is returned as this
// call's error too -- the job still ends StatusDeadLetter or
// StatusRetrying for it exactly as any other Handle failure would,
// consistent with jobs' own retry semantics, while
// ExportService.Export's already-completed, already-delivered manifest is
// not undone by that -- a caller inspecting the export result (once a
// later round adds a way to retrieve it) still finds what was gathered.
func (s *ExportService) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	result, err := s.export.Export(ctx, job.TenantID)
	if err != nil {
		return jobs.Result{}, err
	}
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

// compile-time check that *ExportService satisfies jobs.Handler.
var _ jobs.Handler = (*ExportService)(nil)
