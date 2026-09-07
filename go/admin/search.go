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
// with D2's audited mechanism (tenancy.WithSystemContext) to answer both
// of D6's halves: the platform-wide identity lookup ("which account does
// this identifier belong to", Users) and the membership composition
// ("which tenants does this person belong to", MembershipsOf).
//
// Both halves take the operator as an explicit actorUserID-style argument
// so every call leaves D2's audit trail naming WHO performed the
// cross-tenant read -- Users under one platform-wide grant (authn's search
// has no tenant in scope at all), MembershipsOf under one grant per
// candidate tenant. It never adds a bypass method to org: MembershipsOf
// calls *org.MemberService's existing, ordinary per-tenant Get method once
// per candidate tenant, exactly as D2 prescribes. The candidate tenant
// list comes from admin's own tenant ledger (D3) -- the one list of "every
// tenant the platform knows about" that exists anywhere in this codebase,
// which is why D3 and D6 were designed together rather than as unrelated
// features.
type SearchService struct {
	authnSvc *authn.Service
	members  *org.MemberService
	tenants  *TenantService

	// bus backs the tenancy.WithSystemContext grant both halves take out:
	// Users one grant covering the whole platform-wide call, MembershipsOf
	// one per candidate tenant. Nil until Module.Register calls attach.
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

// Users is D6's first half: a passthrough to authn.Service.SearchUsers,
// authn's platform-operator search entry point, which has no tenant in
// scope at all (users is identity data, not tenant data).
//
// actorUserID identifies the platform operator making this cross-tenant
// search, for pkgcore.SystemReason.Actor -- never the searched-for user.
// The single SearchUsers call runs under one audited
// tenancy.WithSystemContext grant (D2's mechanism, the identical shape
// MembershipsOf uses per candidate tenant): authn's search needs no
// tenant-isolation escape hatch -- it is platform-wide by design -- but
// the wrapper's audit record is the trail of "which operator searched the
// user directory, under which declared purpose", and a search whose
// answers carry plaintext email and phone (encrypted at rest by authn's
// PII serializer) is exactly the platform-operator cross-tenant read D2's
// audited mechanism exists to cover. On unfixed main the authorization
// reasoning (the admin:search_users permission check is the HTTP handler's
// job, per docs/internal/23-admin.md's D6: it happens at admin's HTTP
// handler layer, and authn.Service itself knows nothing about rbac) was
// mistaken for the whole of the search half's obligations, and the search
// skipped the wrapper entirely -- leaving no trace, unlike the same D6's
// membership-composition half right beside it.
func (s *SearchService) Users(ctx context.Context, actorUserID string, q authn.UserSearchQuery) ([]authn.User, error) {
	sysCtx, err := tenancy.WithSystemContext(ctx, s.bus, pkgcore.SystemReason{
		Actor:   actorUserID,
		Purpose: SystemPurposeAdminCrossTenant,
	})
	if err != nil {
		return nil, err
	}
	return s.authnSvc.SearchUsers(sysCtx, q)
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
// they actually do". The candidate tenant list itself comes from
// TenantService.ListAllIDs, which pages through the FULL ledger rather
// than one capped List call, for the identical no-silent-omission reason
// -- a ledger past 500 rows must not make this answer quietly incomplete.
func (s *SearchService) MembershipsOf(ctx context.Context, userID, actorUserID string) ([]pkgcore.TenantID, error) {
	ledger, err := s.tenants.ListAllIDs(ctx)
	if err != nil {
		return nil, err
	}

	var found []pkgcore.TenantID
	for _, id := range ledger {
		tenantID := pkgcore.TenantID(id)
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
