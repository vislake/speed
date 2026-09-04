package admin

import (
	"context"
	"encoding/json"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/dbkit/audit"
)

// TenantService is D3's runtime: the operator-facing tenant ledger, kept
// current by two independent paths -- the event-driven lazy population
// subscriber below, and the manual CRUD operators drive through admin's own
// HTTP surface.
type TenantService struct {
	repo *TenantRepository

	// bus and auditActions are nil until Module.Register calls attachAudit
	// -- every method that needs them (SetStatus) tolerates that by
	// skipping the audit side effect rather than panicking, so a host that
	// has not finished wiring never crashes a request; it just runs
	// without an audit trail for that one call, exactly like go/pki's
	// Handler.recordAudit.
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
}

// NewTenantService returns a TenantService over repo.
func NewTenantService(repo *TenantRepository) *TenantService {
	return &TenantService{repo: repo}
}

// attachAudit gives the service what SetStatus needs to record an audit
// event: the bus to publish on and the registry's frozen-by-use-time audit
// action catalog, both read from the host's *pkgcore.Registry during
// Module.Register.
func (s *TenantService) attachAudit(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar) {
	s.bus = bus
	s.auditActions = actions
}

// Create is D3's manual-registration path: an operator registers a tenant
// before any business write has happened (docs/internal/23-admin.md
// section 3, D3's second bullet).
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

// ListAllIDs returns EVERY tenant id in the ledger, paging through
// TenantRepository.List's own Cursor mechanism rather than issuing a
// single call capped at maxTenantListLimit.
//
// This exists because D6's MembershipsOf and D7's cross-tenant
// AuditService.Query both need "every tenant the platform knows about" as
// their candidate list, and a single List(ctx, TenantFilter{Limit:
// maxTenantListLimit}) call silently drops every ledger row past the
// 500th once the platform has grown beyond that -- an omission neither
// caller's own contract allows (MembershipsOf's doc comment: a failure
// aborts the call rather than silently omitting a tenant; the same
// no-silent-omission expectation applies to an audit search meant to
// cover "every tenant"). Paging here, once, is what lets both callers
// keep their own single-call simplicity while still seeing the whole
// ledger.
func (s *TenantService) ListAllIDs(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		rows, err := s.repo.List(ctx, TenantFilter{Limit: maxTenantListLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			ids = append(ids, row.TenantID)
		}
		if len(rows) < maxTenantListLimit {
			break
		}
		cursor = rows[len(rows)-1].TenantID
	}
	return ids, nil
}

// SetStatus applies patch (rename, suspend/resume, notes) to tenantID's
// ledger row and, when the write succeeds, records an explicit
// admin.tenant.status_changed audit event carrying actor as the Actor --
// this is round 1's audit trail for a tenant-ledger edit; D4's enforcement
// of a suspended status against real traffic is round 2's work.
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
// error) when the host has not attached a bus yet.
func (s *TenantService) recordAudit(ctx context.Context, actor pkgcore.Actor, tenantID string, patch TenantPatch) {
	if s.bus == nil {
		return
	}
	after := map[string]any{}
	if patch.Status != nil {
		after["status"] = string(*patch.Status)
	}
	if patch.DisplayName != nil {
		after["display_name"] = *patch.DisplayName
	}
	auditCtx := pkgcore.WithActor(ctx, actor)
	err := audit.Emit(auditCtx, s.bus, s.auditActions, audit.Input{
		Action: AuditActionTenantStatusChanged,
		Resource: audit.Resource{
			Type: "admin.tenant",
			ID:   tenantID,
		},
		Result: audit.Result{Success: true},
		Changes: &audit.Diff{
			After: after,
		},
	})
	if err != nil {
		obs.FromContext(ctx).Warn("admin failed to record a tenant status-change audit event",
			"tenant_id", tenantID, "error", err)
	}
}

// handleOrgNodeCreated is D3's event-driven lazy population subscriber: it
// listens for org's real org.node.created event and, when the created node
// is a tenant's ROOT node -- org.OrgNode.IsRoot()'s own discriminator,
// ParentID == "" (NOT node depth, which docs/internal/23-admin.md's own
// draft assumed before this round verified the real field against
// go/org/model.go -- see this round's final report for the correction) --
// lazily creates an active, blank-display-name ledger row for the event's
// tenant if none exists yet.
//
// Like org's own handleUserCreated (go/org/events.go), this subscriber is
// resilient by construction:
//
//  1. Nobody publishes the event -- the subscription simply never fires.
//  2. The payload is not a shape this handler recognizes -- logged at Warn,
//     nil returned (never an error: an in-process bus propagates a handler's
//     error back to org's own Publish call, and admin's confusion about a
//     payload must never fail org's own node-creation write).
//  3. The event carries no tenant (evt.TenantID == "") -- skipped; there is
//     nothing to register a ledger row for.
//  4. The created node is not a root node -- skipped; D3's ledger only
//     cares about a tenant's first node, the closest thing to a
//     "tenant was created" signal the platform has.
//  5. The created node IS a root node -- EnsureExists lazily creates the
//     row, idempotently: a redelivered event, or a tenant already
//     registered manually, never fails and never overwrites an operator's
//     own edits.
//
// admin is explicitly permitted to import org's own package directly (root
// CLAUDE.md's admin-sits-at-the-top rule, and docs/internal/23-admin.md
// section 1's identical statement), so this decodes directly into org's
// own org.NodeCreated struct rather than probing a JSON map by hand the
// way org's own cross-module subscriber must for authn's event -- but it
// still round-trips through JSON rather than a direct type assertion,
// because a cross-replica delivery over pkgcore's Redis EventBus arrives
// as a map[string]any, never as the publisher's own struct.
func (s *TenantService) handleOrgNodeCreated(ctx context.Context, evt pkgcore.Event) error {
	log := obs.FromContext(ctx)

	if evt.TenantID == "" {
		log.Debug("admin ignored an org.node.created event with no tenant",
			"event_type", evt.Type)
		return nil
	}

	var payload org.NodeCreated
	if err := decodeEventPayload(evt.Payload, &payload); err != nil {
		log.Warn("admin ignored an org.node.created event with an unrecognized payload",
			"event_type", evt.Type, "error", err)
		return nil
	}
	if payload.ParentID != "" {
		// Not a root node: D3's ledger only lazily-registers on a
		// tenant's first (root) node.
		return nil
	}

	if _, err := s.repo.EnsureExists(ctx, string(evt.TenantID)); err != nil {
		log.Warn("admin could not lazily register a tenant ledger row",
			"tenant_id", evt.TenantID, "error", err)
	}
	return nil
}

// decodeEventPayload round-trips payload through JSON into out, which
// works uniformly whether payload arrived as the publisher's own struct
// (a same-replica, in-process delivery) or as a map[string]any (a
// cross-replica delivery over pkgcore's Redis EventBus) -- mirroring
// org.userIDFromPayload's identical technique, generalized to decode a
// whole struct rather than probe one field.
func decodeEventPayload(payload any, out any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}
