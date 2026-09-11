package admin

import (
	"context"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

// TenantService is the tenant ledger's runtime: the operator-facing
// tenant ledger, kept current by two independent paths -- the event-driven
// lazy population subscriber below, and the manual CRUD operators drive
// through admin's own HTTP surface.
type TenantService struct {
	repo *TenantRepository

	// emitter carries the audit seam this service's two record-time paths
	// read: SetStatus's explicit record (recordAudit) and the per-tenant
	// system-context grants forEachLedgerTenant takes out. Its seats are
	// unattached until Module.Register calls attachAudit -- SetStatus
	// tolerates that by skipping the audit side effect rather than
	// panicking, so a host that has not finished wiring never crashes a
	// request; it just runs without an audit trail for that one call,
	// exactly like go/pki's Handler.recordAudit. The actor display name
	// recordAudit records resolves through emitter.resolveActor (see
	// resolveActorName's own doc comment for the policy); WithAuthn is a
	// mandatory production option, so a correctly wired assembly always
	// has that seat, and a nil-seam unit fixture records id-only actors.
	emitter auditEmitter
}

// NewTenantService returns a TenantService over repo.
func NewTenantService(repo *TenantRepository) *TenantService {
	return &TenantService{repo: repo}
}

// attachAudit gives the service the audit seam SetStatus and
// forEachLedgerTenant need, read from the host's *pkgcore.ComponentRegistry
// (and authn module) during Module.Register -- see auditEmitter's own doc
// comment for what the three seats are.
func (s *TenantService) attachAudit(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, authnSvc *authn.Service) {
	s.emitter.attach(bus, actions, authnSvc)
}

// Create is the manual-registration path: an operator registers a tenant
// before any business write has happened.
func (s *TenantService) Create(ctx context.Context, t *Tenant) error {
	return s.repo.Create(ctx, t)
}

// Get returns the ledger row for tenantID.
func (s *TenantService) Get(ctx context.Context, tenantID string) (*Tenant, error) {
	return s.repo.Get(ctx, tenantID)
}

// List returns ledger rows matching filter.
func (s *TenantService) List(ctx context.Context, filter TenantFilter) ([]Tenant, error) {
	return s.repo.List(ctx, filter)
}

// ListAllRows returns EVERY tenant row in the ledger, paging through
// TenantRepository.List's own Cursor mechanism rather than issuing a
// single call capped at maxTenantListLimit.
//
// This exists because every cross-tenant read of the ledger needs "every
// tenant the platform knows about" as its candidate list -- the
// membership composition, the cross-tenant audit query, the send-record
// search and the usage dashboard -- and a single List(ctx,
// TenantFilter{Limit: maxTenantListLimit}) call silently drops every
// ledger row past that limit once the platform has grown beyond it, an
// omission none of those callers' own contracts allows (a failure aborts
// a cross-tenant read rather than silently omitting a tenant). Paging
// here, once, is what lets every caller keep its own single-call
// simplicity while still seeing the whole ledger.
func (s *TenantService) ListAllRows(ctx context.Context) ([]Tenant, error) {
	var rows []Tenant
	cursor := ""
	for {
		page, err := s.repo.List(ctx, TenantFilter{Limit: maxTenantListLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		rows = append(rows, page...)
		if len(page) < maxTenantListLimit {
			break
		}
		cursor = page[len(page)-1].TenantID
	}
	return rows, nil
}

// ListAllIDs returns EVERY tenant id in the ledger -- the id projection
// of ListAllRows' own paged walk, for callers that need only the ids.
func (s *TenantService) ListAllIDs(ctx context.Context) ([]string, error) {
	rows, err := s.ListAllRows(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range rows {
		ids = append(ids, rows[i].TenantID)
	}
	return ids, nil
}

// forEachLedgerTenant runs fn once per tenant in admin's own ledger,
// under that tenant's own audited system-context grant
// (tenancy.WithSystemContext through the emitter's bus): the one shape
// this module's per-tenant cross-tenant reads share. The membership
// composition, the cross-tenant send-record search and the usage
// dashboard all fan out over the ledger this way, and both the ledger
// itself and the bus every grant audits onto are this service's own, so
// the fan-out lives here rather than being re-assembled at each caller.
//
// actorUserID identifies the platform operator every per-tenant grant is
// attributable to, for pkgcore.SystemReason.Actor. Rows arrive in
// ListAllRows' own order (the paged List order), and a listing failure, a
// grant failure or an fn failure aborts the walk with the error returned
// unchanged: a cross-tenant read must report a storage outage as an
// error rather than silently answering for fewer tenants than the ledger
// holds.
//
// The row -- not just its id -- reaches fn, so a caller that needs a
// ledger column (the usage dashboard's DisplayName) reads the same
// listing that produced the row instead of re-reading the tenant
// afterwards.
func (s *TenantService) forEachLedgerTenant(ctx context.Context, actorUserID string, fn func(tenantCtx context.Context, row Tenant) error) error {
	rows, err := s.ListAllRows(ctx)
	if err != nil {
		return err
	}
	for i := range rows {
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, pkgcore.TenantID(rows[i].TenantID)),
			s.emitter.bus,
			pkgcore.SystemReason{
				Actor:   actorUserID,
				Purpose: SystemPurposeAdminCrossTenant,
			},
		)
		if err != nil {
			return err
		}
		if err := fn(tenantCtx, rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// SetStatus applies patch (rename, suspend/resume, notes) to tenantID's
// ledger row and, when the write succeeds, records an explicit
// admin.tenant.status_changed audit event carrying actor as the Actor --
// every ledger edit an operator makes lands on the audit trail. The
// enforcement of a suspended status against real traffic happens
// separately, through tenancy's TenantStatusResolver seam (Status's own
// doc comment).
//
// A failure to publish the audit event is logged and swallowed, never
// returned: the ledger write already committed by the time this runs, so
// surfacing the audit failure as this call's own error would report a
// failure that did not happen -- the same choice go/pki's Handler.recordAudit
// and notes' recordNoteCreatedAudit make.
func (s *TenantService) SetStatus(ctx context.Context, tenantID string, patch TenantPatch, actor pkgcore.Actor) (*Tenant, error) {
	t, err := s.repo.Update(ctx, tenantID, patch)
	if err != nil {
		return nil, err
	}
	s.recordAudit(ctx, actor, tenantID, patch)
	return t, nil
}

// recordAudit emits admin.tenant.status_changed. It is a no-op (not an
// error) while the host has not attached the emitter's bus -- the
// publish itself is skipped (emitTenantStatusChangeAudit's own guard);
// the record's construction above it is pure.
//
// The caller-supplied actor (built by handler.go's callerUserID from the
// operator's verified Principal user id alone) is resolved against the
// users table here, at record time, so the row carries the operator's
// display name rather than an id-only actor -- resolveActorName's own doc
// comment has the full policy, including what stays id-only and why. A
// publish failure is Warn-logged and swallowed, never returned: the
// ledger write already committed by the time this runs
// (emitTenantStatusChangeAudit's own contract).
func (s *TenantService) recordAudit(ctx context.Context, actor pkgcore.Actor, tenantID string, patch TenantPatch) {
	after := map[string]any{}
	if patch.Status != nil {
		after["status"] = string(*patch.Status)
	}
	if patch.DisplayName != nil {
		after["display_name"] = *patch.DisplayName
	}
	s.emitter.emitTenantStatusChangeAudit(
		pkgcore.WithActor(ctx, s.emitter.resolveActor(ctx, actor)),
		[]any{"tenant_id", tenantID},
		audit.Input{
			Action: AuditActionTenantStatusChanged,
			Resource: audit.Resource{
				Type: "admin.tenant",
				ID:   tenantID,
			},
			Result: audit.Result{Success: true},
			Changes: &audit.Diff{
				After: after,
			},
		},
	)
}

// Status implements tenancy.TenantStatusResolver: the ledger row's stored
// status is what gives "suspend a tenant" real teeth, once a host wires
// tenancy.WithTenantStatusResolver(adminModule.Tenants()) into its own
// tenancy.Middleware call.
//
// A tenant absent from the ledger entirely -- one whose event-driven lazy
// registration has not landed yet, or one nobody has bothered to record
// here at all -- is reported TenantStatusActive, never suspended: the
// ledger is an operator CONVENIENCE (this file's own TenantService doc
// comment), never the authoritative source of tenant existence, so its
// own absence must never itself become a reason to refuse a request --
// that would turn "the ledger has not caught up yet" into an outage for a
// perfectly legitimate, brand-new tenant. Any other repository failure (a
// genuine database error) is propagated unchanged, so tenancy.Middleware
// fails the request closed with ErrTenantStatusUnavailable rather than
// assuming the tenant is active on an unreachable ledger.
//
// A present row's stored status is returned as-is, with no translation:
// the ledger's Status column is typed with tenancy.TenantStatus itself,
// so there is no admin-local vocabulary that could misread a stored value.
// The seam's own gate refuses any status other than
// tenancy.TenantStatusActive by default, so a future third ledger state a
// tenant must not be served under is refused automatically rather than
// silently read as "assume active".
func (s *TenantService) Status(ctx context.Context, tenant pkgcore.TenantID) (tenancy.TenantStatus, error) {
	t, err := s.repo.Get(ctx, string(tenant))
	if err != nil {
		if apperr.HasCode(err, ErrTenantNotFound.Code) {
			return tenancy.TenantStatusActive, nil
		}
		return "", err
	}
	return t.Status, nil
}

// compile-time check that *TenantService satisfies
// tenancy.TenantStatusResolver.
var _ tenancy.TenantStatusResolver = (*TenantService)(nil)

// handleOrgNodeCreated is the event-driven lazy population subscriber: it
// listens for org's real org.node.created event and, when the created node
// is a tenant's ROOT node -- org.OrgNode.IsRoot()'s own discriminator,
// ParentID == "" (never node depth) -- lazily creates an active,
// blank-display-name ledger row for the event's tenant if none exists yet.
//
// The subscriber is resilient by construction:
//
//  1. Nobody publishes the event -- the subscription simply never fires.
//  2. The payload is not a shape this handler recognizes -- logged at Warn,
//     nil returned (never an error: an in-process bus propagates a handler's
//     error back to org's own Publish call, and admin's confusion about a
//     payload must never fail org's own node-creation write).
//  3. The event carries no tenant (evt.TenantID == "") -- skipped; there is
//     nothing to register a ledger row for.
//  4. The created node is not a root node -- skipped; the ledger only
//     cares about a tenant's first node, the closest thing to a
//     "tenant was created" signal the platform has.
//  5. The created node IS a root node -- EnsureExists lazily creates the
//     row, idempotently: a redelivered event, or a tenant already
//     registered manually, never fails and never overwrites an operator's
//     own edits.
//
// admin sits at the top of the module dependency graph and is the one
// module permitted to import the concrete packages below it directly, so
// this decodes directly into org's own org.NodeCreated struct through
// pkgcore.DecodeEventPayload rather than probing JSON keys by hand the way
// org's own cross-module subscriber must for authn's event -- still through
// the JSON round-trip, never a direct type assertion, because a
// cross-replica delivery over pkgcore's Redis EventBus arrives as a
// map[string]any, never as the publisher's own struct.
func (s *TenantService) handleOrgNodeCreated(ctx context.Context, evt pkgcore.Event) error {
	log := obs.FromContext(ctx)

	if evt.TenantID == "" {
		log.Debug("admin ignored an org.node.created event with no tenant",
			"event_type", evt.Type)
		return nil
	}

	var payload org.NodeCreated
	if err := pkgcore.DecodeEventPayload(evt.Payload, &payload); err != nil {
		log.Warn("admin ignored an org.node.created event with an unrecognized payload",
			"event_type", evt.Type, "error", err)
		return nil
	}
	if payload.ParentID != "" {
		// Not a root node: the ledger only lazily-registers on a
		// tenant's first (root) node.
		return nil
	}

	if _, err := s.repo.EnsureExists(ctx, string(evt.TenantID)); err != nil {
		log.Warn("admin could not lazily register a tenant ledger row",
			"tenant_id", evt.TenantID, "error", err)
	}
	return nil
}
