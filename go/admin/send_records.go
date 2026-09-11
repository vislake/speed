package admin

import (
	"context"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// SendRecordSearchService is the notification send-record search,
// single-tenant when one tenant is named and cross-tenant when none is,
// every read composed with the audited system-context mechanism
// (tenancy.WithSystemContext) -- mirroring AuditService.Query exactly,
// including for the single-tenant path.
//
// It adds no method to notification: notification.SendRecordRepository's
// ListByFilter is single-tenant, and this service enters a system context
// scoped to the one named tenant (or loops once per candidate tenant from
// the ledger, entering one per tenant) -- never a ListAcrossTenants
// bypass on notification's own repository.
type SendRecordSearchService struct {
	deliveries *notification.DeliveryService
	tenants    *TenantService

	// emitter carries the bus the single-tenant path's
	// tenancy.WithSystemContext grant audits onto. The cross-tenant path
	// takes its per-tenant grants through the tenants service's own
	// emitter (TenantService.forEachLedgerTenant). Unattached until
	// Module.Register calls attach.
	emitter auditEmitter
}

// NewSendRecordSearchService returns a SendRecordSearchService reading
// send records through deliveries.SendRecords(), with candidate tenants
// (for the cross-tenant path) drawn from tenants (admin's own ledger).
func NewSendRecordSearchService(deliveries *notification.DeliveryService, tenants *TenantService) *SendRecordSearchService {
	return &SendRecordSearchService{deliveries: deliveries, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext
// -- this service's only use of the audit seam.
func (s *SendRecordSearchService) attach(bus pkgcore.EventBus) { s.emitter.attachBus(bus) }

// Query returns send records matching filter (Channel/Status/From/To/
// Limit/Offset). When tenantID is non-empty, this is the single-tenant
// path: ONE tenancy.WithSystemContext grant scoped to that tenant around a
// single ListByFilter call -- never a direct, wrapper-less read.
// notification's send_records table is platform data whose rows any
// tenant's deliveries settle into, and a read of one tenant's records by a
// platform operator is exactly the cross-tenant operation the audited
// system-context mechanism exists to cover, so the single-tenant path must
// leave the same tenancy.system_context.entered audit trail every other
// admin cross-tenant read does. When tenantID is empty, this is the
// cross-tenant path: every tenant in admin's own ledger is searched in
// turn under the same mechanism -- TenantService.forEachLedgerTenant's own
// per-tenant grant, exactly like SearchService.MembershipsOf -- and the
// results are concatenated in ledger order. Limit/Offset then apply PER
// TENANT (SendRecordRepository.ListByFilter's own contract), not to the
// concatenated cross-tenant result as a whole, an inherited limitation
// mirroring compliance.AuditQuery's own identical pagination shape, not
// re-solved here.
//
// actorUserID identifies the platform operator making this read, for
// pkgcore.SystemReason.Actor on every path -- the audit trail of who
// entered the system context is exactly what makes each read attributable.
func (s *SendRecordSearchService) Query(ctx context.Context, actorUserID, tenantID string, filter notification.SendRecordFilter) ([]notification.SendRecord, error) {
	if tenantID != "" {
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, pkgcore.TenantID(tenantID)),
			s.emitter.bus,
			pkgcore.SystemReason{
				Actor:   actorUserID,
				Purpose: SystemPurposeAdminCrossTenant,
			},
		)
		if err != nil {
			return nil, err
		}
		filter.TenantID = tenantID
		return s.deliveries.SendRecords().ListByFilter(tenantCtx, filter)
	}

	var all []notification.SendRecord
	err := s.tenants.forEachLedgerTenant(ctx, actorUserID, func(tenantCtx context.Context, row Tenant) error {
		perTenant := filter
		perTenant.TenantID = row.TenantID
		records, err := s.deliveries.SendRecords().ListByFilter(tenantCtx, perTenant)
		if err != nil {
			return err
		}
		all = append(all, records...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}
