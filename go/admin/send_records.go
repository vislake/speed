package admin

import (
	"context"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// SendRecordSearchService is D10's runtime: notification send-record
// search, cross-tenant when no single tenant is named, composed with D2's
// mechanism exactly like SearchService.MembershipsOf and UsageService.Summary.
//
// It adds no method to notification: notification.SendRecordRepository's
// new ListByFilter (this round's one purely-additive change to
// go/notification) is single-tenant, and this service loops it once per
// candidate tenant under tenancy.WithSystemContext -- never a
// ListAcrossTenants bypass on notification's own repository.
type SendRecordSearchService struct {
	deliveries *notification.DeliveryService
	tenants    *TenantService

	// bus backs the tenancy.WithSystemContext grant the cross-tenant path
	// takes out per candidate tenant. Nil until Module.Register calls
	// attach.
	bus pkgcore.EventBus
}

// NewSendRecordSearchService returns a SendRecordSearchService reading
// send records through deliveries.SendRecords(), with candidate tenants
// (for the cross-tenant path) drawn from tenants (admin's own D3 ledger).
func NewSendRecordSearchService(deliveries *notification.DeliveryService, tenants *TenantService) *SendRecordSearchService {
	return &SendRecordSearchService{deliveries: deliveries, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext.
func (s *SendRecordSearchService) attach(bus pkgcore.EventBus) { s.bus = bus }

// Query returns send records matching filter (Channel/Status/From/To/
// Limit/Offset). When tenantID is non-empty, this is the single-tenant
// path: one direct ListByFilter call. When tenantID is empty, this is the
// cross-tenant path: every tenant in admin's own D3 ledger is searched in
// turn under D2's tenancy.WithSystemContext mechanism and the results are
// concatenated in ledger order -- Limit/Offset then apply PER TENANT
// (SendRecordRepository.ListByFilter's own contract), not to the
// concatenated cross-tenant result as a whole, an inherited limitation
// mirroring compliance.AuditQuery's own identical pagination shape, not
// re-solved here.
//
// actorUserID identifies the platform operator making this read, for
// pkgcore.SystemReason.Actor on the cross-tenant path -- unused on the
// single-tenant path, which needs no system context at all.
func (s *SendRecordSearchService) Query(ctx context.Context, actorUserID, tenantID string, filter notification.SendRecordFilter) ([]notification.SendRecord, error) {
	if tenantID != "" {
		filter.TenantID = tenantID
		return s.deliveries.SendRecords().ListByFilter(ctx, filter)
	}

	ledger, err := s.tenants.ListAllIDs(ctx)
	if err != nil {
		return nil, err
	}

	var all []notification.SendRecord
	for _, id := range ledger {
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, pkgcore.TenantID(id)),
			s.bus,
			pkgcore.SystemReason{
				Actor:   actorUserID,
				Purpose: SystemPurposeAdminCrossTenant,
			},
		)
		if err != nil {
			return nil, err
		}
		perTenant := filter
		perTenant.TenantID = id
		records, err := s.deliveries.SendRecords().ListByFilter(tenantCtx, perTenant)
		if err != nil {
			return nil, err
		}
		all = append(all, records...)
	}
	return all, nil
}
