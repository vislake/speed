package admin

import (
	"context"
	"time"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// AuditFilter is Query's input, the admin HTTP shell's translation of its
// request query parameters into compliance.QueryFilter -- see D7's own
// doc comment: admin's audit-search page does only an HTTP shell plus
// pagination-parameter translation.
type AuditFilter struct {
	// TenantID narrows to one tenant when non-empty; empty means "every
	// tenant this operator is entitled to see", the cross-tenant path.
	TenantID string
	Actor    string
	Resource string
	Action   string
	From     time.Time
	To       time.Time
	Success  *bool
}

// AuditService is D7's whole contribution: a thin HTTP-facing wrapper over
// compliance.AuditQuery, which already implements the entire read side
// (Query/QueryAcrossTenants/Get). admin adds no filtering or storage logic
// of its own -- it only decides which of AuditQuery's two read paths to
// call, and supplies the tenant list for the cross-tenant path from its
// own D3 ledger.
type AuditService struct {
	query   *compliance.AuditQuery
	tenants *TenantService

	// bus backs the tenancy.WithSystemContext grant every call takes out.
	// Nil until Module.Register calls attach.
	bus pkgcore.EventBus
}

// NewAuditService returns an AuditService reading through query, with
// candidate tenants for the cross-tenant path drawn from tenants.
func NewAuditService(query *compliance.AuditQuery, tenants *TenantService) *AuditService {
	return &AuditService{query: query, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext.
func (s *AuditService) attach(bus pkgcore.EventBus) { s.bus = bus }

// Query answers GET /api/v1/admin/audit-events. actorUserID identifies the
// platform operator making the request, for pkgcore.SystemReason.Actor.
//
// When filter.TenantID is set, this is a single-tenant read: it enters a
// system context scoped to that one tenant and calls AuditQuery.Query --
// exactly D2's mechanism, a per-tenant call under tenancy.WithSystemContext. When it
// is empty, this is the cross-tenant read: it lists every tenant admin's
// own ledger (D3) knows about, plus the empty-string platform-level
// pseudo-tenant AuditQuery.QueryAcrossTenants' own convention names, and
// calls QueryAcrossTenants with that full list under one system context.
func (s *AuditService) Query(ctx context.Context, actorUserID string, filter AuditFilter) ([]audit.AuditEvent, error) {
	qf := compliance.QueryFilter{
		Actor:    filter.Actor,
		Resource: filter.Resource,
		Action:   filter.Action,
		From:     filter.From,
		To:       filter.To,
		Success:  filter.Success,
	}

	reason := pkgcore.SystemReason{
		Actor:   actorUserID,
		Purpose: SystemPurposeAdminCrossTenant,
	}

	if filter.TenantID != "" {
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, pkgcore.TenantID(filter.TenantID)),
			s.bus, reason,
		)
		if err != nil {
			return nil, err
		}
		return s.query.Query(tenantCtx, qf)
	}

	// ListAllIDs pages through the FULL ledger rather than one call capped
	// at maxTenantListLimit -- a cross-tenant audit search is meant to
	// cover "every tenant admin knows about", and a ledger past 500 rows
	// must not make that silently partial.
	ledger, err := s.tenants.ListAllIDs(ctx)
	if err != nil {
		return nil, err
	}
	// The empty string names platform-level events (AuditQuery.
	// QueryAcrossTenants' own documented convention), which is never a row
	// of admin's own tenant ledger -- it is included explicitly here.
	tenantIDs := make([]string, 0, len(ledger)+1)
	tenantIDs = append(tenantIDs, "")
	tenantIDs = append(tenantIDs, ledger...)

	sysCtx, err := tenancy.WithSystemContext(ctx, s.bus, reason)
	if err != nil {
		return nil, err
	}
	return s.query.QueryAcrossTenants(sysCtx, tenantIDs, qf)
}

// Get answers a single audit event lookup by id -- a thin passthrough to
// AuditQuery.Get, which performs no tenant-scope check of its own (an id
// is an opaque, application-generated UUID, never a value a caller could
// plausibly guess or enumerate -- see AuditQuery.Get's own doc comment).
func (s *AuditService) Get(ctx context.Context, id string) (*audit.AuditEvent, error) {
	return s.query.Get(ctx, id)
}
