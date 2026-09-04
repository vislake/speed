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
	"github.com/vislake/speed/go/sharing"
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

// ConfigExportDeliveryExpiry is the dotted configuration key for how long a
// data-export download link stays valid once Export mints it through
// go/sharing. Declared, like sharing's own ConfigDefaultExpiry
// (go/sharing/module.go), so the value is visible and, eventually,
// editable through go/config's own admin-console machinery -- this round
// does not wire a live per-tenant reader for it (ExportService holds no
// *config.Service), so every export this round mints uses
// defaultExportDeliveryExpiry regardless of what an operator sets here.
// That is the identical, honestly-recorded limitation sharing's own
// ConfigDefaultExpiry carries for the same reason (AGENTS.md's "Tenant-
// configured default expiry" section).
const ConfigExportDeliveryExpiry = "compliance.export_delivery_expiry"

// defaultExportDeliveryExpiry is ConfigExportDeliveryExpiry's own declared
// Default, and, this round, the only value Export ever actually uses.
//
// This is deliberately far shorter than sharing's own 30-day
// defaultShareExpiry: an export bundles a subject's complete personal
// data into one downloadable package, so the window in which a leaked or
// intercepted link stays usable must be measured in hours, not weeks --
// docs/internal/10-compliance-and-audit.md's data-export bullet describes
// asynchronously generating the package and handing it off through
// sharing, which this round reads as a one-time credentialed handoff to
// the requesting subject, not an open, long-lived download link. 24 hours
// is chosen as long enough for a subject to notice and follow a delivery
// notification (a later round's job -- this round mints the share and
// returns its token, see ExportDelivery) without leaving the window open
// for days.
const defaultExportDeliveryExpiry = 24 * time.Hour

// exportDeliveryMaxViews caps a data-export share at exactly one granted
// view. The design alternative -- a password-protected share -- was
// considered and rejected for this round: a password needs its own
// delivery channel (the caller would have to relay it to the subject
// separately from the link itself), which is more moving parts than this
// round's scope, while a single-view, 256-bit-token share
// (sharing/token.go's newShareToken) already gives the "one-time
// credentialed handoff" docs/internal/10-compliance-and-audit.md
// describes: the token itself is the credential, and MaxViews=1 means the
// link is spent the moment it is actually used, not merely until it
// expires.
const exportDeliveryMaxViews = 1

// SharingCreator is the read/write seam ExportService.Export uses to hand
// a stored export off to go/sharing for delivery -- exactly
// sharing.Service.Create's own shape. Declared as a small interface here,
// rather than depending on the concrete *sharing.Service type in
// ExportService's own field, keeps this package's unit tests independent
// of a real sharing.Service's own gorm.DB, migrations and registry wiring.
//
// go/compliance's go.mod requires go/sharing directly -- sanctioned by
// this codebase's dependency direction, since sharing sits below
// compliance in the module graph (root CLAUDE.md: "... ->
// authn/rbac/org/metering -> billing/ai-gateway/sharing/integration ->
// compliance -> admin"), the identical reasoning go/billing's own
// UsageReader doc comment gives for its own sanctioned direct dependency
// on go/metering. module.go's compile-time assertion proves
// *sharing.Service satisfies this interface structurally, so a host wires
// the real thing with no adapter to write.
type SharingCreator interface {
	Create(ctx context.Context, p sharing.CreateParams) (*sharing.CreateResult, error)
}

// ExportDelivery is what Export hands back once a completed export has
// been delivered through go/sharing: the minted share's id, for the
// requester's own bookkeeping (revoking it early, listing its access
// log), and the raw bearer token a caller uses to build the one-time
// download link -- exactly sharing's own CreateResult.Token, returned
// exactly once and never persisted anywhere, including here (see
// sharing's Share.TokenHash doc comment for why the token itself is never
// stored). ExpiresAt echoes the minted share's own expiry so a caller
// never has to separately compute it.
type ExportDelivery struct {
	// ShareID is the id of the go/sharing.Share this export was delivered
	// through.
	ShareID string
	// Token is the one-time bearer token a caller uses to build the
	// download link. Never persisted; the only copy of it in existence
	// once this call returns is whatever the caller does with it.
	Token string
	// ExpiresAt is when the share link stops being accessible.
	ExpiresAt time.Time
}

// ExportResult is Export's full outcome, mirroring sharing.CreateResult's
// own single-struct-return shape rather than a long positional multi-
// return: the object key the manifest was stored under (in
// pkgcore.ObjectStore's own namespace, exportObjectKey's own doc
// comment), the gathered manifest itself, and the go/sharing delivery
// minted against that key. Export always returns a non-nil *ExportResult
// with whatever it has actually completed, even alongside a non-nil
// error -- see Export's own doc comment for exactly what each field holds
// under each failure mode.
type ExportResult struct {
	// ObjectKey is the pkgcore.ObjectStore key the manifest was stored
	// under.
	ObjectKey string
	// Manifest is the gathered ExportManifest, whether or not every
	// participant's Export callback succeeded.
	Manifest ExportManifest
	// Delivery is the zero value when delivery through go/sharing failed
	// or was never attempted (ErrSharingRequired); otherwise the minted
	// share's details.
	Delivery ExportDelivery
}

// ExportService gathers every registered participant's exportable data
// for one tenant into one ExportManifest, stores it through the
// pkgcore.ObjectStore seam, and delivers it to the requesting subject as a
// short-lived, single-view go/sharing.Share -- the data-portability half
// of docs/internal/10-compliance-and-audit.md's data-export capability, in
// full: gathering, storage and delivery, not gathering alone. Unlike
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
	sharing   SharingCreator
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
// JSON, stores it through pkgcore.ObjectStore under a fresh, module-
// namespaced key, and delivers it to the requesting subject as a
// short-lived, single-view go/sharing.Share pointing at that key
// (deliverExport's own doc comment for the expiry/view-limit choice). It
// always returns a non-nil *ExportResult carrying whatever it actually
// completed, even alongside a non-nil error -- never a zero-value return.
//
// Delivery requires a SharingCreator to have been wired through
// Module.WithSharing; without one, Export refuses outright with
// ErrSharingRequired before gathering anything, since a manifest this
// module cannot hand to its subject is not a completed export.
//
// Like Sweep and Erase, one participant's Export failing does not stop
// the gathering of the rest: the failure lands in ExportManifest.Errors,
// and the manifest is still built, marshaled, stored and delivered -- a
// partial export is still useful evidence of what was gathered and what
// was not. Export returns ErrExportPartialFailure whenever
// ExportManifest.HasErrors() is true, so a caller decides whether to
// re-run Export once the failing participant is healthy again rather than
// assuming a complete gather.
//
// A failure to deliver the already-stored manifest through go/sharing --
// as opposed to a participant failing to contribute data -- is reported as
// ErrExportDeliveryFailed instead: ExportResult.ObjectKey and .Manifest
// are still populated (the gather and store already succeeded), but
// .Delivery is the zero value, since no share exists for the subject to
// retrieve the export with. A delivery failure takes precedence over a
// participant partial failure in the returned error (both cannot be
// represented by one apperr code at once), but ExportManifest.Errors is
// unaffected either way, so a caller inspecting the manifest still learns
// about a participant failure even when the returned error names the
// delivery problem instead.
//
// ctx must carry a tenant (pkgcore.WithTenant); Export never accepts a
// caller-supplied tenant parameter, per this repository's own API rule.
// The request and its outcome -- including the minted share id when
// delivery succeeded -- are recorded as one AuditActionExportRequest audit
// event; a failure to publish it is reported by wrapping
// ErrAuditRecordFailed, exactly like Sweep and Erase, and does not erase
// the manifest already stored or the share already minted.
func (s *ExportService) Export(ctx context.Context, tenant pkgcore.TenantID) (*ExportResult, error) {
	if s.sharing == nil {
		return nil, ErrSharingRequired
	}
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
		return nil, fmt.Errorf("compliance: marshal export manifest: %w", err)
	}

	id := uuid.NewString()
	key := exportObjectKey(tenant, id)
	if err := s.store.PutObject(ctx, key, bytes.NewReader(encoded)); err != nil {
		return nil, fmt.Errorf("compliance: store export manifest: %w", err)
	}

	result := &ExportResult{ObjectKey: key, Manifest: manifest}

	delivery, deliverErr := s.deliverExport(ctx, key)
	if deliverErr != nil {
		if auditErr := s.emitExportAudit(ctx, tenant, key, manifest, ExportDelivery{}, deliverErr); auditErr != nil {
			return result, ErrAuditRecordFailed.WithCause(auditErr)
		}
		return result, ErrExportDeliveryFailed.WithCause(deliverErr)
	}
	result.Delivery = delivery

	if err := s.emitExportAudit(ctx, tenant, key, manifest, delivery, nil); err != nil {
		return result, ErrAuditRecordFailed.WithCause(err)
	}
	if manifest.HasErrors() {
		return result, ErrExportPartialFailure.WithParam("participants", exportFailureReason(manifest))
	}
	return result, nil
}

// deliverExport hands the manifest stored under key off to go/sharing: a
// single-view (exportDeliveryMaxViews), defaultExportDeliveryExpiry-lived
// share naming key as its opaque ResourceRef. Sensitive is always true --
// an export is, by construction, a subject's complete personal data, so it
// always qualifies for sharing's own sensitive-resource confirmation audit
// (sharing.share.create_sensitive, go/sharing/AGENTS.md's "Sensitive-
// resource confirmation" section), independently of and in addition to
// this module's own AuditActionExportRequest event.
//
// No password is set -- see exportDeliveryMaxViews's own doc comment for
// why a single-view link over a 256-bit token is this round's chosen
// mechanism instead.
func (s *ExportService) deliverExport(ctx context.Context, key string) (ExportDelivery, error) {
	maxViews := exportDeliveryMaxViews
	expiresAt := time.Now().Add(defaultExportDeliveryExpiry)
	created, err := s.sharing.Create(ctx, sharing.CreateParams{
		ResourceRef: key,
		ExpiresAt:   &expiresAt,
		MaxViews:    &maxViews,
		Sensitive:   true,
	})
	if err != nil {
		return ExportDelivery{}, err
	}
	delivery := ExportDelivery{ShareID: created.Share.ID, Token: created.Token}
	if created.Share.ExpiresAt != nil {
		delivery.ExpiresAt = *created.Share.ExpiresAt
	}
	return delivery, nil
}

// emitExportAudit records one AuditActionExportRequest event for a
// completed export, whether or not delivery through go/sharing succeeded.
// deliverErr is nil on a successful delivery; when non-nil, delivery is
// the zero value and the audit event's own Result reports the delivery
// failure rather than any participant gathering failure, since a manifest
// gathered without errors but never delivered is still not a completed
// export from the requesting subject's point of view.
func (s *ExportService) emitExportAudit(ctx context.Context, tenant pkgcore.TenantID, key string, manifest ExportManifest, delivery ExportDelivery, deliverErr error) error {
	changes := map[string]any{
		"object_key":   key,
		"participants": participantNames(manifest.Participants),
	}
	if manifest.HasErrors() {
		changes["errors"] = manifest.Errors
	}
	success := !manifest.HasErrors()
	failureReason := exportFailureReason(manifest)
	if deliverErr != nil {
		success = false
		failureReason = fmt.Sprintf("delivery failed: %s", deliverErr.Error())
	} else {
		changes["share_id"] = delivery.ShareID
		changes["share_expires_at"] = delivery.ExpiresAt
	}
	return audit.Emit(ctx, s.bus, s.actions, audit.Input{
		Action: AuditActionExportRequest,
		Resource: audit.Resource{
			Type: "compliance.tenant",
			ID:   string(tenant),
		},
		Result: audit.Result{
			Success:       success,
			FailureReason: failureReason,
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
