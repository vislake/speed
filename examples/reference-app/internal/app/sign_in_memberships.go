package app

import (
	"context"
	"errors"
	"sync"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// signInTenantEnumerationPurpose is the pkgcore.SystemPurpose this app
// declares for the one system-context grant its sign-in path takes:
// TenantsOf's cross-tenant "which tenants does this user belong to" read
// of org's memberships table. The declaration mirrors a module's own
// Register-time declaration of the purposes it takes system contexts for
// (pkgcore.RegisterSystemPurpose is idempotent), and the grant itself is
// taken through tenancy.WithSystemContext, whose
// tenancy.system_context.entered event is the audit trail every use of
// the escape hatch must carry. BuildServer registers the purpose at the
// same point it attaches the store's org-backed half.
const signInTenantEnumerationPurpose pkgcore.SystemPurpose = "reference-app.sign_in_tenant_enumeration"

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
// WithMembershipReader seam to (BuildServer), and therefore the answer to
// the two questions authn must ask about an account before it may act in a
// tenant (go/authn/service.go's own MembershipReader doc comment): "is this
// (user, tenant) pair an active membership" and "which tenants does this
// user belong to, at all".
//
// # Customer memberships are org's rows, read live
//
// Both customer-tenant questions are answered from org's real, persistent
// memberships table -- MemberService.Get under the tenant context this
// store rebuilds from the tenant asked about for the pair question, and
// MemberService.TenantsOf under a system context for the enumeration
// question. One source of truth: an account that really accepted an org
// invitation through org's own HTTP flow, a demo account the boot-time seed
// placed into org (demo_users.go's addDemoOrgMembership), and a
// self-registered account whose registration provisioned its own clinic
// (self_service.go), are members because their rows exist -- in this
// process and in the next one. A restart against the same database loses
// neither; an in-process roster alone could never give that property,
// since a membership created by a previous process would otherwise answer
// "not a member" forever after that process exited.
//
// # The enumeration question is org's own query
//
// The "which tenants does this user belong to" half is answered by org's
// MemberService.TenantsOf (go/org's own cross-tenant query) under the
// audited system context below -- never by a host-maintained scan set of
// candidate tenants: org's own memberships table already carries every
// membership fact, so a clinic created after boot is answered from its org
// row with no host list to update, and a restart needs no re-discovery
// pass at all.
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
// shortcut (cmd/server/server_test.go's registerAndAuthenticate and friends), which
// is also what makes a customer-tenant entry in this roster mean: a test
// rig has declared "this account may act in this tenant" without going
// through org, and authn honors it only because org itself has no row for
// the pair to answer with -- the reader prefers org's row over this roster
// whenever both could answer, so a real org fact always wins over a stale
// shortcut.
//
// # The system context the enumeration takes, and its audit trail
//
// TenantsOf is a cross-tenant read, and org serves it only to a system
// context (org.system_context_required otherwise -- the same gate dbkit's
// own HardDelete carries). This store obtains that grant through
// tenancy.WithSystemContext, the audited wrapper, with the purpose this
// app declares (signInTenantEnumerationPurpose) and the account itself as
// the actor -- the account asking, at its own no-tenant sign-in, which
// tenants it belongs to, a question the platform's design reserves for the
// authn side of the house. Every grant publishes a
// tenancy.system_context.entered event, so the enumeration is audited on
// every use exactly like the escape hatch's own contract demands.
type signInMemberships struct {
	mu sync.Mutex
	// granted holds the in-process grants: rbac.SystemDomain seats plus
	// test-only customer-tenant shortcuts. Customer memberships created by
	// real paths never land here -- they are org rows, answered above.
	granted map[string][]pkgcore.TenantID

	// org is the module service customer-tenant questions are asked of.
	// Nil until BuildServer calls attach -- a signInMemberships built
	// before that (testConfig's constructor) simply answers from granted
	// alone, which is fine because BuildServer always attaches before any
	// request can reach authn.
	org *org.MemberService
	// bus is the event bus the audited system-context grant for the
	// enumeration question is published on (tenancy.WithSystemContext's
	// parameter). Set by the same attach that sets org; the two halves of
	// the store's org-backed answer arrive together.
	bus pkgcore.EventBus
}

// NewSignInMemberships returns an empty membership store.
func NewSignInMemberships() *signInMemberships {
	return &signInMemberships{granted: make(map[string][]pkgcore.TenantID)}
}

// attach binds the org-backed half of the store: the MemberService whose
// rows answer customer-tenant questions, and the bus every audited
// system-context grant for the enumeration question is published on.
// BuildServer calls attach once, after Bootstrap has composed the module
// set (the bus exists only then) and before any request -- or demo seed
// sign-in -- can reach authn; until then the store answers from granted
// alone.
func (m *signInMemberships) attach(orgMembers *org.MemberService, bus pkgcore.EventBus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.org = orgMembers
	m.bus = bus
}

// Grant records userID as an active member of tenant in the in-process
// roster. It is idempotent: granting the same pair twice does not
// duplicate the entry.
//
// Who may call Grant: a production path that must give a user a real
// customer-tenant membership creates an org row instead (the demo seed's
// addDemoOrgMembership; org's own invitation accept), and this roster is
// only ever the fallback answer for pairs org has no row for. The two
// legitimate Grant callers are the platform-staff seed
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
// answers a genuinely unanswerable membership question as a refusal
// instead of a fabricated answer: password sign-in folds it into the
// uniform 401 authn.invalid_credentials, indistinguishable from a wrong
// password; the distinguishable 403 authn.tenant_membership_unavailable
// remains only on paths answering an already-authenticated caller,
// tenant switching and refresh. A question
// about rbac.SystemDomain skips org entirely and is answered from the
// roster alone.
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
// The customer-tenant half of the answer is org's own cross-tenant query
// -- MemberService.TenantsOf (go/org/membership_tenants.go), one indexed
// read of the memberships table that sees every tenant, boot-configured or
// provisioned at run time, this process's or a previous one's. The org
// answer is taken under the audited system context above and then folded
// together with this store's roster entries, de-duplicated, so a test
// shortcut never answers twice and a real org row always answers first --
// exactly the precedence ActiveMembership gives the org rows.
func (m *signInMemberships) TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	tenants := make([]pkgcore.TenantID, 0, 4)
	seen := make(map[pkgcore.TenantID]struct{})

	if m.org != nil {
		if m.bus == nil {
			// attach always sets org and bus together; an org-backed store
			// with no bus could not audit its system-context grant, so the
			// question is refused rather than answered unrecorded.
			return nil, errors.New("reference-app: membership store has no event bus for its system-context grant")
		}
		sysCtx, err := tenancy.WithSystemContext(ctx, m.bus, pkgcore.SystemReason{
			Actor:   userID,
			Purpose: signInTenantEnumerationPurpose,
		})
		if err != nil {
			return nil, err
		}
		orgTenants, err := m.org.TenantsOf(sysCtx, userID)
		if err != nil {
			return nil, err
		}
		for _, tenant := range orgTenants {
			tenants = append(tenants, tenant)
			seen[tenant] = struct{}{}
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
