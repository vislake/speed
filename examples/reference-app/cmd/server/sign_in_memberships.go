package main

import (
	"context"
	"sort"
	"sync"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
)

// orgCodeIs reports whether err is a go/org application error carrying
// code -- the matching convention go/org/errors.go's own index documents:
// org's exported errors are *apperr.Error builders whose WithParam and
// WithCause derivations return a NEW instance, so a caller matches a
// decorated error with apperr.As and a Code comparison, never with == or
// errors.Is against the declared var.
func orgCodeIs(err error, code string) bool {
	orgErr, ok := apperr.As(err)
	return ok && orgErr.Code == code
}

// signInMemberships is the authn.MembershipReader this app wires authn's
// WithMembershipReader seam to (buildServer), and therefore the answer to
// the two questions authn must ask about an account before it may act in a
// tenant (go/authn/service.go's own MembershipReader doc comment): "is this
// (user, tenant) pair an active membership" and "which tenants does this
// user belong to, at all".
//
// # Customer memberships are org's rows, read live
//
// Every customer-tenant membership question is answered from org's real,
// persistent memberships table -- MemberService.Get under the tenant
// context this store rebuilds from the tenant asked about, the same "host
// glue reads org's rows" shape go/admin's impersonation-target membership
// check (impersonation_service.go's MembershipChecker) already established.
// One source of truth: an account that really accepted an org invitation
// through org's own HTTP flow, and a demo account the boot-time seed placed
// into org (demo_users.go's addDemoOrgMembership), are members because
// their rows exist -- in this process and in the next one. A restart
// against the same database loses neither, which is exactly the property an
// in-process roster could never give: before this store read org, a real
// accepted invitation (or a seeded demo account) that predated the current
// process answered "not a member" forever after the process that created it
// exited, authn refusing every sign-in with 403
// authn.tenant_membership_required no matter how real the row was.
//
// What is NOT answered from org is the rbac.SystemDomain pseudo-tenant
// ("system", rbac/subject.go): the platform-operations domain has no org
// tree and deliberately no memberships rows -- platform staff and the
// admin probe accounts this app's flows register are members of it by
// grant, not by any org row, and org is the wrong place for that fact by
// design (the pseudo-tenant exists precisely because it is NOT a customer
// organization). Those grants are the entire remaining content of the
// in-process roster below. In a production-shaped boot they hold exactly
// one entry -- the demo platform-staff account seedDemoPlatformStaff
// registers and grants (demo_admin.go) -- and that seed re-asserts the
// grant on every boot, so the staff account's sign-in survives a restart
// like everyone else's. Everything else ever granted here is test-only
// shortcut (server_test.go's registerAndAuthenticate and friends), which
// is also what makes a customer-tenant entry in this roster mean: a test
// rig has declared "this account may act in this tenant" without going
// through org, and authn honors it only because org itself has no row for
// the pair to answer with -- the reader prefers org's row over this roster
// whenever both could answer, so a real org fact always wins over a stale
// shortcut.
type signInMemberships struct {
	mu sync.Mutex
	// granted holds the in-process grants: rbac.SystemDomain seats plus
	// test-only customer-tenant shortcuts. Customer memberships created by
	// real paths never land here -- they are org rows, answered above.
	granted map[string][]pkgcore.TenantID

	// org is the module service customer-tenant questions are asked of.
	// Nil until buildServer calls attach -- a signInMemberships built
	// before that (testConfig's constructor) simply answers from granted
	// alone, which is fine because buildServer always attaches before any
	// request can reach authn.
	org *org.MemberService
	// universe is every tenant this host knows (the configured host
	// tenants), sorted, the scan set for the "which tenants does this user
	// belong to" question -- see TenantsOf's doc comment for why the
	// question has to be asked per tenant.
	universe []pkgcore.TenantID
}

// newSignInMemberships returns an empty membership store.
func newSignInMemberships() *signInMemberships {
	return &signInMemberships{granted: make(map[string][]pkgcore.TenantID)}
}

// attach binds the org-backed half of the store: the MemberService whose
// rows answer customer-tenant questions, and the host's own tenant
// universe (the values of cfg.HostTenants -- the only tenants that can
// ever accrue an org memberships row in this app, since every org root
// and invitation lives in a configured tenant). buildServer calls it once,
// right after it has built both; until then the store answers from granted
// alone.
func (m *signInMemberships) attach(orgMembers *org.MemberService, hostTenants map[string]pkgcore.TenantID) {
	seen := make(map[pkgcore.TenantID]struct{}, len(hostTenants))
	universe := make([]pkgcore.TenantID, 0, len(hostTenants))
	for _, tenant := range hostTenants {
		if _, dup := seen[tenant]; dup {
			continue
		}
		seen[tenant] = struct{}{}
		universe = append(universe, tenant)
	}
	sort.Slice(universe, func(i, j int) bool { return universe[i] < universe[j] })
	m.org = orgMembers
	m.universe = universe
}

// Grant records userID as an active member of tenant in the in-process
// roster. It is idempotent: granting the same pair twice does not
// duplicate the entry.
//
// Who may call Grant, and what it means, changed when this store learned
// to read org: a production path that must give a user a real
// customer-tenant membership creates an org row instead (the demo seed's
// addDemoOrgMembership; org's own invitation accept), and this roster is
// only ever the fallback answer for pairs org has no row for. The two
// remaining legitimate Grant callers are the platform-staff seed
// (rbac.SystemDomain, the one membership that has no org row by design)
// and the test shortcuts that declare "this account may act in this
// tenant" for a flow that provisions its own org state over HTTP.
func (m *signInMemberships) Grant(userID string, tenant pkgcore.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.granted[userID] {
		if existing == tenant {
			return
		}
	}
	m.granted[userID] = append(m.granted[userID], tenant)
}

// ActiveMembership implements authn.MembershipReader.
//
// A customer-tenant question is answered by org's own row first: the
// membership exists when MemberService.Get finds one, does not when it
// reports org.ErrMembershipNotFound (in which case the in-process roster
// gets its say -- the test-shortcut case above), and any other error is
// returned rather than guessed at, so authn's own fail-closed handling
// (403 authn.tenant_membership_unavailable) surfaces a genuinely
// unanswerable membership question instead of a fabricated answer. A
// question about rbac.SystemDomain skips org entirely and is answered
// from the roster alone.
func (m *signInMemberships) ActiveMembership(ctx context.Context, userID string, tenant pkgcore.TenantID) (bool, error) {
	if tenant != rbac.SystemDomain && m.org != nil {
		member, err := m.org.Get(pkgcore.WithTenant(ctx, tenant), userID)
		switch {
		case err == nil:
			return member != nil, nil
		case !orgCodeIs(err, org.ErrMembershipNotFound.Code):
			return false, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.granted[userID] {
		if existing == tenant {
			return true, nil
		}
	}
	return false, nil
}

// TenantsOf implements authn.MembershipReader.
//
// org's own schema has no single "every tenant this user belongs to"
// answer: memberships are tenant-scoped rows, so the question has to be
// asked per tenant. This host can afford to ask exactly that way because
// its tenant universe is bounded and known -- the configured host tenants,
// the only tenants that can ever hold an org membership in this app (see
// attach's doc comment) -- which is the "org-backed adapter for a consumer
// with its own bounded tenant set" shape the pre-org store's own doc
// comment already named as the honest option. The roster's entries are
// folded in after the org scan, de-duplicated, so a test shortcut never
// answers twice and a real org row always answers first.
func (m *signInMemberships) TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	tenants := make([]pkgcore.TenantID, 0, 4)
	seen := make(map[pkgcore.TenantID]struct{})

	if m.org != nil {
		for _, tenant := range m.universe {
			_, err := m.org.Get(pkgcore.WithTenant(ctx, tenant), userID)
			switch {
			case err == nil:
				tenants = append(tenants, tenant)
				seen[tenant] = struct{}{}
			case !orgCodeIs(err, org.ErrMembershipNotFound.Code):
				return nil, err
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tenant := range m.granted[userID] {
		if _, dup := seen[tenant]; dup {
			continue
		}
		seen[tenant] = struct{}{}
		tenants = append(tenants, tenant)
	}
	return tenants, nil
}

// compile-time check that *signInMemberships satisfies authn.MembershipReader.
var _ authn.MembershipReader = (*signInMemberships)(nil)
