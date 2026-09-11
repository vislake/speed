package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
)

// AuditActionExportRequest is the audit action Module.Register declares
// and ExportService.Export emits under, once per export request.
const AuditActionExportRequest = "compliance.export.request"

// participantErrorMarker is the classification-only value an export's or a
// retention sweep's failure record stores for each failed participant --
// in ExportManifest.Errors, in emitExportAudit's and emitSweepAudit's
// Changes["errors"] entry: the participant's Name is the map key, this
// constant the classification. It is the sweep and export halves of the
// identical rule erasureAuditErrorMarker (erasure.go) applies to the
// erasure path's audit record: the record says WHO failed and THAT it
// failed -- never the error text itself. The text must not reach either
// of this constant's two write surfaces, for two distinct reasons. The
// audit events' changes column is effectively permanent -- dbkit/audit/
// emit.go's Diff contract: "Anything written here is effectively
// permanent" -- and participant failure text can carry identifiers and
// internal details no audit reader was ever promised. And
// ExportManifest.Errors is serialized into the manifest Export stores and
// delivers over an unauthenticated, single-view go/sharing link: the link
// holder -- the export's recipient -- is entitled to read the export's
// data, not platform-internal failure text that can name other subjects,
// internal object keys or infrastructure details (the audience argument:
// two audiences conflated into one). The text's legitimate homes are the
// structured log at each failure site (behind go/observability's
// redaction layer) and the in-process results and returned errors --
// SweepResult.Errors and ErasureResult.Errors on those paths,
// ErrExportDeliveryFailed's own wrapped cause on the delivery path.
const participantErrorMarker = "failed"

// ExportManifest is one data-export package: every registered
// participant's own gathered data for one tenant, keyed by participant
// Name, plus any per-participant gathering classification. It is the JSON
// document ExportService.Export stores through pkgcore.ObjectStore and
// then delivers as a short-lived, single-view go/sharing.Share (see
// deliverExport): the manifest is the tenant-level export bundle itself,
// and the share is the credentialed window on the stored copy of it.
type ExportManifest struct {
	// Tenant is the tenant the export was gathered for.
	Tenant pkgcore.TenantID `json:"tenant"`
	// GeneratedAt is when Export ran.
	GeneratedAt time.Time `json:"generated_at"`
	// Participants maps participant Name to the JSON-serializable value
	// its Export callback returned.
	Participants map[string]any `json:"participants"`
	// Errors maps participant Name to participantErrorMarker for
	// participants whose Export callback failed -- a participant present
	// here contributes nothing to Participants for this run. The map is a
	// classification, deliberately never the callback's error text: this
	// manifest is the deliverable Export stores and hands to its
	// recipient over an unauthenticated, single-view go/sharing link, and
	// the link holder is entitled to read the export's data, not
	// platform-internal failure text that can name other subjects,
	// internal object keys or infrastructure details (participantErrorMarker's
	// own doc comment, and the same classification-only rule the erasure
	// path's audit record already follows). The error text's home is the
	// structured log at the gather site, behind go/observability's
	// redaction layer.
	Errors map[string]string `json:"errors,omitempty"`
}

// HasErrors reports whether any participant's Export callback failed.
func (m ExportManifest) HasErrors() bool { return len(m.Errors) > 0 }

// ConfigExportDeliveryExpiry is the dotted configuration key for how long a
// data-export download link stays valid once Export mints it through
// go/sharing. Declared as a go/config item, like sharing's own
// ConfigDefaultExpiry (go/sharing/module.go), so the value is tenant-
// overridable like any config item; a host reads its tenant-resolved value
// live by wiring an ExportDeliveryExpiryReader through
// WithExportConfigReader (its own doc comment has the wiring detail) --
// without one, every export mints defaultExportDeliveryExpiry regardless
// of what an operator sets here.
const ConfigExportDeliveryExpiry = "compliance.export_delivery_expiry"

// defaultExportDeliveryExpiry is ConfigExportDeliveryExpiry's own declared
// Default, and the value exportDeliveryExpiry falls back to when no
// ExportDeliveryExpiryReader is wired or the wired one reports the tenant
// has configured none.
//
// This is deliberately far shorter than sharing's own 30-day
// MaxExplicitShareLifetime: an export bundles one tenant's complete data
// -- potentially many subjects' records -- into a single downloadable
// package, so the window in which a leaked or intercepted link stays
// usable must be measured in hours, not weeks. Delivery is a one-time
// credentialed handoff: Export mints the share and returns its token to
// the caller, who relays the link to the export's recipient -- never an
// open, long-lived download link. 24 hours is long enough for a relayed
// link to reach its recipient and be used (see ExportDelivery), without
// leaving the window open for days.
const defaultExportDeliveryExpiry = 24 * time.Hour

// exportDeliveryMaxViews caps a data-export share at exactly one granted
// view. The design alternative -- a password-protected share -- was
// considered and rejected: a password needs its own delivery channel (the
// caller would have to relay it to the link's recipient separately from
// the link itself), which is more moving parts, while a single-view,
// 256-bit-token share (sharing/token.go's newShareToken) already gives
// the one-time credentialed handoff: the token itself is the credential,
// and MaxViews=1 means the link is spent the moment it is actually used,
// not merely until it expires.
const exportDeliveryMaxViews = 1

// ExportDeliveryExpiryReader is the structurally-typed seam ExportService
// reads a tenant's configured export-delivery-link expiry through -- the
// same (d, ok, err) shape go/sharing's own TenantConfigReader uses for its
// ConfigDefaultExpiry, even though compliance already imports go/config
// directly elsewhere (RetentionService.cfg, a plain *config.Service field):
// a construction-time Module Option cannot capture config.Module.Attach's
// *config.Service, which per its own doc comment is only produced strictly
// after the assembly's declaration turn returns -- by which point every module's own
// NewModule call, this one included, has already run. This interface
// exists so a host can wire a lazy adapter -- NewConfigReader, over
// config's lazy Handle, is the sanctioned one -- not to avoid an import
// edge compliance does not have.
//
// ok is false when the tenant has configured none (the value resolved at
// go/config's own schema default) -- Export then falls back to
// defaultExportDeliveryExpiry, exactly as if no
// ExportDeliveryExpiryReader were wired at all. err is a genuine read
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
// go/compliance's go.mod requires go/sharing directly: sharing sits below
// compliance in the module dependency graph, so the import edge runs in
// the sanctioned direction. module.go's compile-time assertion proves
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
// for one tenant into one tenant-level ExportManifest, stores it through
// the pkgcore.ObjectStore seam, and delivers that bundle as a short-
// lived, single-view go/sharing.Share whose one-time token it returns to
// the caller to relay: gathering, storage and delivery, not gathering
// alone. The export scope is one whole tenant, never one data subject: a
// subject-scoped ("this is your data") export is not built (see doc.go's
// ExportService bullet). Unlike RetentionService and ErasureService,
// Export needs no system context: it only ever reads the caller's own ctx
// tenant, through each participant's Export callback (typically backed by
// that participant's own tenant-scoped dbkit.Repository[T] read), so it
// never bypasses tenant isolation and grants nothing extra. That ctx
// tenant is the single data boundary an export may ever cross: Export
// refuses a ctx carrying no tenant (pkgcore.ErrNoTenant) and refuses a
// tenant argument that differs from the ctx tenant
// (ErrExportTenantMismatch), before gathering, storing or delivering
// anything -- see Export's own doc comment.
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
// the tenant ctx carries -- the bundle is that whole tenant's data,
// never one subject's -- wraps it into one ExportManifest, marshals it
// to JSON, stores it through pkgcore.ObjectStore under a fresh, module-
// namespaced key, and delivers it as a short-lived, single-view
// go/sharing.Share pointing at that key, returning the minted share's id
// and one-time token to the caller in ExportResult.Delivery
// (deliverExport's own doc comment for the expiry/view-limit choice). It
// always returns a non-nil *ExportResult carrying whatever it actually
// completed, even alongside a non-nil error -- never a zero-value return.
//
// Delivery requires a SharingCreator to have been wired through
// Module.WithSharing; without one, Export refuses outright with
// ErrSharingRequired before gathering anything, since a manifest this
// module cannot deliver is not a completed export.
//
// Like Sweep and Erase, one participant's Export failing does not stop
// the gathering of the rest: the failure lands in ExportManifest.Errors
// as a classification (the participant's Name keyed to
// participantErrorMarker -- never the callback's error text, which the
// delivered manifest must not carry; see participantErrorMarker's doc
// comment for where the text goes instead), and the manifest is still
// built, marshaled, stored and delivered -- a partial export is still
// useful evidence of what was gathered and what was not. Export returns
// ErrExportPartialFailure whenever ExportManifest.HasErrors() is true, so
// a caller decides whether to re-run Export once the failing participant
// is healthy again rather than assuming a complete gather.
//
// A failure to deliver the already-stored manifest through go/sharing --
// as opposed to a participant failing to contribute data -- is reported as
// ErrExportDeliveryFailed instead: ExportResult.ObjectKey and .Manifest
// are still populated (the gather and store already succeeded), but
// .Delivery is the zero value, since no share was minted for the export
// to be retrieved through. The stored object itself is deleted before
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
	eachParticipant(s.retention.Participants(),
		func(p pkgcore.RetentionParticipant) (any, bool, error) {
			if p.Export == nil {
				return nil, false, nil
			}
			data, exportErr := p.Export(ctx, tenant)
			return data, true, exportErr
		},
		func(p pkgcore.RetentionParticipant, data any, exportErr error) {
			if exportErr != nil {
				// The participant's error text is logged here -- behind
				// go/observability's redaction layer -- never written into the
				// manifest's Errors entry, which is the classification-only
				// participantErrorMarker: the manifest is the export
				// deliverable, delivered to its recipient over an
				// unauthenticated, single-view share link whose holder is
				// entitled to the export's data, not platform-internal failure
				// text (see participantErrorMarker's doc comment, and the
				// erasure path's identical classification rule). The manifest
				// itself must still say WHO failed and THAT it failed -- the
				// marker is that record.
				if manifest.Errors == nil {
					manifest.Errors = make(map[string]string)
				}
				manifest.Errors[p.Name] = participantErrorMarker
				observability.FromContext(ctx).Error("compliance: export participant failed",
					"participant", p.Name, "error", exportErr)
				return
			}
			manifest.Participants[p.Name] = data
		})

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
		// The transport error's text is logged here -- behind
		// go/observability's redaction layer -- and carried to this call's
		// own caller as ErrExportDeliveryFailed's wrapped cause below,
		// never written into the audit record's FailureReason, which
		// emitExportAudit classifies as "delivery failed" instead (the
		// audit row is effectively permanent; see participantErrorMarker's
		// doc comment).
		observability.FromContext(ctx).Error("compliance: export delivery failed",
			"error", deliverErr)
		// A manifest that could not be delivered must not stay stored: it
		// is an un-shareable copy of the tenant's complete data with no
		// legitimate consumer path, and leaving it behind
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
		return result, ErrExportPartialFailure.WithParam("participants", ParticipantFailureReason(manifest.Errors))
	}
	return result, nil
}

// deliverExport hands the manifest stored under key off to go/sharing: a
// single-view (exportDeliveryMaxViews), exportDeliveryExpiry(ctx, tenant)-
// lived share naming key as its opaque ResourceRef. Sensitive is always
// true -- an export is, by construction, one tenant's whole data bundle,
// potentially many subjects' records, so it always qualifies for
// sharing's own sensitive-resource confirmation audit (the
// sharing.share.create_sensitive action), independently of and in
// addition to this module's own AuditActionExportRequest event.
//
// No password is set -- see exportDeliveryMaxViews's own doc comment for
// why a single-view link over a 256-bit token is the chosen mechanism
// instead.
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
// the link's recipient a share already expired (or long past) at mint
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
// itself enforces for a tenant with no configured default of its own
// (MaxExplicitShareLifetime's own doc comment: a tenant's own sharing
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
// export from the requester's point of view.
//
// Changes["errors"] carries a classification only -- each failed
// participant's name keyed to participantErrorMarker -- deliberately
// never the participant error's text: dbkit/audit's Diff content contract
// (emit.go) forbids sensitive content in the changes column, which is
// effectively permanent. The same classification applies to a delivery
// failure's FailureReason ("delivery failed", never the transport error's
// text): the transport error's homes are the structured log at the
// delivery-failure site in Export (behind go/observability's redaction
// layer) and the returned ErrExportDeliveryFailed's own wrapped cause,
// not the audit row. The participant error text's home is the structured
// log at the gather site; ExportManifest.Errors itself already holds the
// classification (see its field doc), and this map rebuilds it explicitly
// so the audit record never depends on whatever the manifest happens to
// carry.
func (s *ExportService) emitExportAudit(ctx context.Context, tenant pkgcore.TenantID, key string, manifest ExportManifest, delivery ExportDelivery, deliverErr error) error {
	changes := map[string]any{
		"object_key":   key,
		"participants": participantNames(manifest.Participants),
	}
	if manifest.HasErrors() {
		changes["errors"] = participantFailureClassification(manifest.Errors, participantErrorMarker)
	}
	success := !manifest.HasErrors()
	failureReason := ParticipantFailureReason(manifest.Errors)
	if deliverErr != nil {
		success = false
		failureReason = "delivery failed"
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
