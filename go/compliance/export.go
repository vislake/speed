package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// AuditActionExportRequest is the audit action Module.Register declares
// and ExportService.Export emits under, once per export request.
const AuditActionExportRequest = "compliance.export.request"

// ExportManifest is one data-export package: every registered
// participant's own gathered data for one tenant, keyed by participant
// Name, plus any per-participant gathering error. It is the JSON document
// ExportService.Export stores through pkgcore.ObjectStore -- the whole of
// this round's export scope, per this module's own doc comment: packaging
// and storing the manifest, never delivering it to the subject (that is
// go/sharing's job, once go/sharing has landed).
type ExportManifest struct {
	// Tenant is the tenant the export was gathered for.
	Tenant pkgcore.TenantID `json:"tenant"`
	// GeneratedAt is when Export ran.
	GeneratedAt time.Time `json:"generated_at"`
	// Participants maps participant Name to the JSON-serializable value
	// its Export callback returned.
	Participants map[string]any `json:"participants"`
	// Errors maps participant Name to the error its Export callback
	// returned, for participants whose callback failed -- a participant
	// present here contributes nothing to Participants for this run.
	Errors map[string]string `json:"errors,omitempty"`
}

// HasErrors reports whether any participant's Export callback failed.
func (m ExportManifest) HasErrors() bool { return len(m.Errors) > 0 }

// ExportService gathers every registered participant's exportable data
// for one tenant into one ExportManifest and stores it through the
// pkgcore.ObjectStore seam -- the data-portability gathering half of
// docs/internal/10-compliance-and-audit.md's data-export capability. Unlike
// RetentionService and ErasureService, Export needs no system context: it
// only ever reads the caller's own ctx tenant, through each participant's
// Export callback (typically backed by that participant's own tenant-
// scoped dbkit.Repository[T] read), so it never bypasses tenant isolation
// and grants nothing extra.
//
// The zero value is not ready to use; construct one with newExportService
// and wire it through Module.Register.
type ExportService struct {
	retention pkgcore.RetentionRegistrar
	bus       pkgcore.EventBus
	actions   pkgcore.AuditActionRegistrar
	store     pkgcore.ObjectStore
}

// newExportService returns an ExportService with no seams wired yet;
// Module.Register attaches the registry's EventBus, AuditActions,
// Retention registrar and resolved ObjectStore.
func newExportService() *ExportService {
	return &ExportService{}
}

// exportObjectKey derives the ObjectStore key one export is stored under,
// namespaced by module and tenant the same way go/storage's own
// ObjectKey/DerivativeKey helpers are (a caller never supplies the key
// itself). id is a fresh UUID per Export call, so re-running Export for
// the same tenant never overwrites an earlier export.
func exportObjectKey(tenant pkgcore.TenantID, id string) string {
	return fmt.Sprintf("compliance/exports/%s/%s.json", tenant, id)
}

// Export gathers every registered participant's Export data (participants
// that left Export nil are silently skipped -- a nil Export is documented
// as a legal, common "not opted in" value, not a misconfiguration) for
// the tenant ctx carries, wraps it into one ExportManifest, marshals it to
// JSON and stores it through pkgcore.ObjectStore under a fresh, module-
// namespaced key. It returns the storage key and the manifest itself, so
// a caller with no ObjectStore-reading code of its own can still inspect
// what was gathered.
//
// Like Sweep and Erase, one participant's Export failing does not stop
// the gathering of the rest: the failure lands in ExportManifest.Errors,
// and the manifest is still built, marshaled and stored -- a partial
// export is still useful evidence of what was gathered and what was not.
// Export returns ErrExportPartialFailure (alongside the key and the
// manifest, never a zero-value return) whenever ExportManifest.HasErrors()
// is true, so a caller decides whether to re-run Export once the failing
// participant is healthy again rather than assuming a complete gather.
//
// ctx must carry a tenant (pkgcore.WithTenant); Export never accepts a
// caller-supplied tenant parameter, per this repository's own API rule.
// The request and its outcome are recorded as one AuditActionExportRequest
// audit event; a failure to publish it is reported by wrapping
// ErrAuditRecordFailed, exactly like Sweep and Erase, and does not erase
// the manifest already stored.
func (s *ExportService) Export(ctx context.Context, tenant pkgcore.TenantID) (string, ExportManifest, error) {
	ctx = pkgcore.WithTenant(ctx, tenant)

	manifest := ExportManifest{
		Tenant:       tenant,
		GeneratedAt:  time.Now(),
		Participants: make(map[string]any),
	}
	for _, p := range s.retention.Participants() {
		if p.Export == nil {
			continue
		}
		data, err := p.Export(ctx, tenant)
		if err != nil {
			if manifest.Errors == nil {
				manifest.Errors = make(map[string]string)
			}
			manifest.Errors[p.Name] = err.Error()
			continue
		}
		manifest.Participants[p.Name] = data
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", manifest, fmt.Errorf("compliance: marshal export manifest: %w", err)
	}

	id := uuid.NewString()
	key := exportObjectKey(tenant, id)
	if err := s.store.PutObject(ctx, key, bytes.NewReader(encoded)); err != nil {
		return "", manifest, fmt.Errorf("compliance: store export manifest: %w", err)
	}

	if err := s.emitExportAudit(ctx, tenant, key, manifest); err != nil {
		return key, manifest, ErrAuditRecordFailed.WithCause(err)
	}
	if manifest.HasErrors() {
		return key, manifest, ErrExportPartialFailure.WithParam("participants", exportFailureReason(manifest))
	}
	return key, manifest, nil
}

// emitExportAudit records one AuditActionExportRequest event for a
// completed export.
func (s *ExportService) emitExportAudit(ctx context.Context, tenant pkgcore.TenantID, key string, manifest ExportManifest) error {
	changes := map[string]any{
		"object_key":   key,
		"participants": participantNames(manifest.Participants),
	}
	if manifest.HasErrors() {
		changes["errors"] = manifest.Errors
	}
	return audit.Emit(ctx, s.bus, s.actions, audit.Input{
		Action: AuditActionExportRequest,
		Resource: audit.Resource{
			Type: "compliance.tenant",
			ID:   string(tenant),
		},
		Result: audit.Result{
			Success:       !manifest.HasErrors(),
			FailureReason: exportFailureReason(manifest),
		},
		Changes: &audit.Diff{After: changes},
	})
}

// participantNames returns the sorted keys of m, for a compact audit
// Changes entry that names which participants contributed data without
// duplicating their whole payload into the audit trail.
func participantNames(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// exportFailureReason renders a short summary of which participants
// failed to export, for audit.Result.FailureReason. Empty when manifest
// has no errors.
func exportFailureReason(manifest ExportManifest) string {
	if !manifest.HasErrors() {
		return ""
	}
	names := make([]string, 0, len(manifest.Errors))
	for name := range manifest.Errors {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("participants failed: %s", strings.Join(names, ", "))
}
