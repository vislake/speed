package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// editable through go/config's own admin-console machinery; a host reads
// its tenant-resolved value live by wiring an ExportDeliveryExpiryReader
// through WithExportConfigReader (its own doc comment has the wiring
// detail) -- without one, every export mints
// defaultExportDeliveryExpiry regardless of what an operator sets here,
// exactly as sharing's own ConfigDefaultExpiry falls back to
// defaultShareExpiry with no TenantConfigReader wired.
const ConfigExportDeliveryExpiry = "compliance.export_delivery_expiry"

// defaultExportDeliveryExpiry is ConfigExportDeliveryExpiry's own declared
// Default, and the value exportDeliveryExpiry falls back to when no
// ExportDeliveryExpiryReader is wired or the wired one reports the tenant
// has configured none.
//
// This is deliberately far shorter than sharing's own 30-day
// MaxExplicitShareLifetime: an export bundles a subject's complete personal
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

// ExportDeliveryExpiryReader is the structurally-typed seam ExportService
// reads a tenant's configured export-delivery-link expiry through -- the
// same (d, ok, err) shape go/sharing's own TenantConfigReader uses for its
// ConfigDefaultExpiry, even though compliance already imports go/config
// directly elsewhere (RetentionService.cfg, a plain *config.Service field):
// a construction-time Module Option cannot capture config.Module.Attach's
// *config.Service, which per its own doc comment is only produced strictly
// after Kernel.Bootstrap returns -- by which point every module's own
// NewModule call, this one included, has already run. This interface
// exists so a host can wire a lazy adapter over a later-filled
// **config.Service the exact way examples/reference-app/cmd/server/
// server.go's orgFeatureGate already does for org.FeatureGate, not to
// avoid an import edge compliance does not have.
//
// ok is false when the tenant has configured none (the value resolved at
// go/config's own schema default) -- Export then falls back to
// defaultExportDeliveryExpiry, exactly as if no
// ExportDeliveryExpiryReader had been wired at all. err is a genuine read
// failure: Export reports it wrapped in ErrExportDeliveryFailed rather
// than guessing at a default.
type ExportDeliveryExpiryReader interface {
	ExportDeliveryExpiry(ctx context.Context, tenant pkgcore.TenantID) (d time.Duration, ok bool, err error)
}

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
// and grants nothing extra. That ctx tenant is the single data boundary an
// export may ever cross: Export refuses a ctx carrying no tenant
// (pkgcore.ErrNoTenant) and refuses a tenant argument that differs from
// the ctx tenant (ErrExportTenantMismatch), before gathering, storing or
// delivering anything -- see Export's own doc comment.
//
// The zero value is not ready to use; construct one with newExportService
// and wire it through Module.Register.
type ExportService struct {
	retention pkgcore.RetentionRegistrar
	bus       pkgcore.EventBus
	actions   pkgcore.AuditActionRegistrar
	store     pkgcore.ObjectStore
	sharing   SharingCreator

	// cfg is the optional live reader of a tenant's configured export
	// delivery expiry. Nil is a legal, fully supported configuration --
	// see ExportDeliveryExpiryReader's own doc comment.
	cfg ExportDeliveryExpiryReader
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
// retrieve the export with. The stored object itself is deleted before
// Export returns in that case: a manifest no share can ever reference is
// an un-shareable copy of the tenant's complete data with no legitimate
// consumer path, and an admin retrying Export must re-gather and re-store
// fresh rather than pile up one such dump per failed attempt (see
// ErrExportDeliveryFailed's doc comment). A delivery failure takes
// precedence over a participant partial failure in the returned error
// (both cannot be represented by one apperr code at once), but
// ExportManifest.Errors is unaffected either way, so a caller inspecting
// the manifest still learns about a participant failure even when the
// returned error names the delivery problem instead.
//
// A successfully delivered manifest is not kept in the object store
// forever either: its lifetime is anchored to the delivery share this
// module mints against it. Module.Register registers compliance's own
// export-manifests retention participant (export_cleanup.go), whose Sweep
// reaps, on the regular per-tenant retention sweep, every stored manifest
// whose delivery share's expiry has itself fallen past the tenant's
// retention window -- the audit event every completed Export leaves behind
// (export_cleanup.go's own doc comment) is the record that makes that
// sweep possible without a store listing primitive.
//
// ctx must carry a tenant (pkgcore.WithTenant), and the tenant argument
// must echo that same tenant back: Export reads every participant's rows
// through the ctx tenant -- the parameter exists only so the job-handler-
// style caller that rebuilt ctx from a stored tenant id can pass that same
// id through, per this repository's own API rule that the tenant comes
// from the context, never from a caller-supplied parameter. A ctx carrying
// no tenant is refused with pkgcore.ErrNoTenant, and a tenant argument
// naming any other tenant is refused with ErrExportTenantMismatch, both
// before anything is gathered, stored or delivered -- a bare or
// mis-scoped ctx must never become a license to pick any tenant.
// The request and its outcome -- including the minted share id when
// delivery succeeded -- are recorded as one AuditActionExportRequest audit
// event; a failure to publish it is reported by wrapping
// ErrAuditRecordFailed, exactly like Sweep and Erase, and does not erase
// the manifest already stored or the share already minted.
func (s *ExportService) Export(ctx context.Context, tenant pkgcore.TenantID) (*ExportResult, error) {
	// The ctx tenant is the one data boundary an export may ever cross:
	// every participant's Export callback below reads through it, so the
	// tenant argument may only echo it back. A bare ctx must never become
	// a license to pick any tenant -- refuse it fail-closed, mirroring
	// AuditQuery.Query's identical no-tenant handling (a raw
	// pkgcore.MustTenantFromContext error, unwrapped, is this module's
	// established no-tenant idiom), and refuse a mismatch outright before
	// anything is gathered, stored or delivered.
	ctxTenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if ctxTenant != tenant {
		return nil, ErrExportTenantMismatch.
			WithParam("ctx_tenant", string(ctxTenant)).
			WithParam("tenant", string(tenant))
	}
	if s.sharing == nil {
		return nil, ErrSharingRequired
	}

	manifest := ExportManifest{
		Tenant:       tenant,
		GeneratedAt:  time.Now(),
		Participants: make(map[string]any),
	}
	for _, p := range s.retention.Participants() {
		if p.Export == nil {
			continue
		}
		data, exportErr := p.Export(ctx, tenant)
		if exportErr != nil {
			if manifest.Errors == nil {
				manifest.Errors = make(map[string]string)
			}
			manifest.Errors[p.Name] = exportErr.Error()
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

	delivery, deliverErr := s.deliverExport(ctx, tenant, key)
	if deliverErr != nil {
		// A manifest that could not be handed to its subject must not
		// stay stored: it is an un-shareable copy of the tenant's complete
		// data with no legitimate consumer path, and leaving it behind
		// would let an admin's retried Export calls accumulate one such
		// dump per attempt. Delete it before returning -- DeleteObject is
		// idempotent, so a retry that re-gathers and re-stores fresh is
		// the only recovery path, never an append to a pile. A failed
		// cleanup is chained into the reported cause: the store itself is
		// misbehaving, and the operator must know the dump may still be
		// there rather than assume the delete succeeded.
		if deleteErr := s.store.DeleteObject(ctx, key); deleteErr != nil {
			deliverErr = errors.Join(deliverErr, fmt.Errorf("compliance: delete undelivered export manifest: %w", deleteErr))
		}
		if auditErr := s.emitExportAudit(ctx, tenant, key, manifest, ExportDelivery{}, deliverErr); auditErr != nil {
			return result, ErrAuditRecordFailed.WithCause(auditErr)
		}
		return result, ErrExportDeliveryFailed.WithCause(deliverErr)
	}
	result.Delivery = delivery

	if auditErr := s.emitExportAudit(ctx, tenant, key, manifest, delivery, nil); auditErr != nil {
		return result, ErrAuditRecordFailed.WithCause(auditErr)
	}
	if manifest.HasErrors() {
		return result, ErrExportPartialFailure.WithParam("participants", exportFailureReason(manifest))
	}
	return result, nil
}

// deliverExport hands the manifest stored under key off to go/sharing: a
// single-view (exportDeliveryMaxViews), exportDeliveryExpiry(ctx, tenant)-
// lived share naming key as its opaque ResourceRef. Sensitive is always
// true -- an export is, by construction, a subject's complete personal
// data, so it always qualifies for sharing's own sensitive-resource
// confirmation audit (sharing.share.create_sensitive, go/sharing/
// AGENTS.md's "Sensitive-resource confirmation" section), independently of
// and in addition to this module's own AuditActionExportRequest event.
//
// No password is set -- see exportDeliveryMaxViews's own doc comment for
// why a single-view link over a 256-bit token is this round's chosen
// mechanism instead.
func (s *ExportService) deliverExport(ctx context.Context, tenant pkgcore.TenantID, key string) (ExportDelivery, error) {
	maxViews := exportDeliveryMaxViews
	expiry, err := s.exportDeliveryExpiry(ctx, tenant)
	if err != nil {
		return ExportDelivery{}, err
	}
	expiresAt := time.Now().Add(expiry)
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

// exportDeliveryExpiry resolves the expiry deliverExport should mint the
// delivery share with: tenant's own ConfigExportDeliveryExpiry override
// when one is set and an ExportDeliveryExpiryReader was wired
// (WithExportConfigReader), defaultExportDeliveryExpiry otherwise --
// exactly the same "optional wiring, honest fallback" shape
// RetentionService.RetentionWindow gives ConfigDefaultRetentionWindow. The
// same guard carries over too: a wired reader answering a non-positive
// duration -- zero or negative -- with ok == true falls back to
// defaultExportDeliveryExpiry rather than being honored, mirroring
// RetentionWindow's own `<= 0` clamp on a configured retention window. A
// zero or negative value is nonsense as a link lifetime -- it would hand
// the requesting subject a share already expired (or long past) at mint
// time -- and the reader is a host-supplied seam whose answer this module
// cannot trust to be sensible, so the nonsense must resolve to the honest
// default, never to an instantly dead link minted silently.
//
// A wired reader answering a duration LONGER than go/sharing's own
// explicit-expiry ceiling (sharing.MaxExplicitShareLifetime) is clamped
// to that ceiling rather than honored -- the mirror-image guard to the
// `<= 0` clamp above, and the same "the host's answer cannot be minted as
// given" family: deliverExport hands the resolved duration to
// sharing.Service.Create as an EXPLICIT ExpiresAt, and sharing refuses an
// explicit expiry beyond its ceiling with sharing.expiry_out_of_range. A
// window beyond the ceiling is not nonsense the way a non-positive one is
// (the default Export path always mints within it), but no host
// configuration may be able to break every export this way -- so the
// answer is clamped DOWN to the sharing ceiling, the closest mintable
// duration to what the operator configured, rather than silently dropped
// to defaultExportDeliveryExpiry. The clamp uses the ceiling sharing
// itself enforces for a tenant with no longer configured default of its
// own (MaxExplicitShareLifetime's own doc comment: a tenant's own sharing
// default can only raise the operative bound, never lower it below this
// floor), so a window clamped here is accepted by sharing for every
// tenant.
func (s *ExportService) exportDeliveryExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, error) {
	if s.cfg == nil {
		return defaultExportDeliveryExpiry, nil
	}
	d, ok, err := s.cfg.ExportDeliveryExpiry(ctx, tenant)
	if err != nil {
		return 0, err
	}
	if !ok {
		return defaultExportDeliveryExpiry, nil
	}
	if d <= 0 {
		return defaultExportDeliveryExpiry, nil
	}
	if d > sharing.MaxExplicitShareLifetime {
		return sharing.MaxExplicitShareLifetime, nil
	}
	return d, nil
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
