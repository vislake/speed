package admin

import (
	"context"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

// SearchService is D6's runtime -- cross-tenant user search -- composed
// with D2's mechanism (loop-per-tenant plus tenancy.WithSystemContext) to
// answer D6's second half, "which tenants does this person belong to".
//
// It never adds a bypass method to org: MembershipsOf calls
// *org.MemberService's existing, ordinary per-tenant Get method once per
// candidate tenant, exactly as D2 prescribes. The candidate tenant list
// comes from admin's own tenant ledger (D3) -- the one list of "every
// tenant the platform knows about" that exists anywhere in this codebase,
// which is why D3 and D6 were designed together rather than as unrelated
// features.
type SearchService struct {
	authnSvc *authn.Service
	members  *org.MemberService
	tenants  *TenantService

	// bus backs the tenancy.WithSystemContext grant MembershipsOf takes
	// out per candidate tenant. Nil until Module.Register calls attach.
	bus pkgcore.EventBus
}

// NewSearchService returns a SearchService reading users through authnSvc
// and memberships through members, with candidate tenants drawn from
// tenants (admin's own ledger).
func NewSearchService(authnSvc *authn.Service, members *org.MemberService, tenants *TenantService) *SearchService {
	return &SearchService{authnSvc: authnSvc, members: members, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext.
func (s *SearchService) attach(bus pkgcore.EventBus) { s.bus = bus }

// Users is D6's first half: a thin passthrough to authn.Service.SearchUsers.
// Authorization (the admin:search_users permission) is the HTTP handler's
// job, per docs/internal/23-admin.md's D6: that check happens at admin's
// HTTP handler layer, and authn.Service itself knows nothing about rbac.
func (s *SearchService) Users(ctx context.Context, q authn.UserSearchQuery) ([]authn.User, error) {
	return s.authnSvc.SearchUsers(ctx, q)
}

// MembershipsOf is D6's second half: every tenant (from admin's own D3
// ledger) that userID currently has an active membership in.
//
// actorUserID identifies the platform operator making this cross-tenant
// read, for pkgcore.SystemReason.Actor -- never the searched-for userID.
// A per-tenant membership lookup failing for a reason OTHER than "no
// membership" aborts the whole call rather than silently omitting that
// tenant from the answer, so a storage outage is reported as an error
// instead of masquerading as "this person belongs to fewer tenants than
// they actually do".
func (s *SearchService) MembershipsOf(ctx context.Context, userID, actorUserID string) ([]pkgcore.TenantID, error) {
	ledger, err := s.tenants.List(ctx, TenantFilter{Limit: maxTenantListLimit})
	if err != nil {
		return nil, err
	}

	var found []pkgcore.TenantID
	for _, row := range ledger {
		tenantID := pkgcore.TenantID(row.TenantID)
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, tenantID),
			s.bus,
			pkgcore.SystemReason{
				Actor:   actorUserID,
				Purpose: SystemPurposeAdminCrossTenant,
			},
		)
		if err != nil {
			return nil, err
		}

		_, err = s.members.Get(tenantCtx, userID)
		if err != nil {
			if isMembershipNotFound(err) {
				continue
			}
			return nil, err
		}
		found = append(found, tenantID)
	}
	return found, nil
}

// isMembershipNotFound reports whether err is org.ErrMembershipNotFound,
// classifying by Code through apperr.As rather than by pointer identity,
// for the same reason isGrantNotFound does (impersonation_service.go).
func isMembershipNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == org.ErrMembershipNotFound.Code
}
