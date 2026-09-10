package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/demoseed"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	obs "github.com/vislake/speed/go/observability"
)

// The three demo accounts seedDemoUsers registers when
// APP_DEMO_USERS_PASSWORD is set. Each is the real-account twin of the
// demo_subject.go actor id its grant model mirrors: demo-owner /
// demo-reader / demo-acme-only are header user ids with no database row
// behind them, while these accounts exist as real authn users whose
// memberships and roles are granted under the user id authn assigns at
// registration.
const (
	// DemoOwnerEmail is the real account behind DemoOwnerUserID's grant
	// model: membership and the built-in owner role in every configured
	// tenant, so a browser signed in as it can do everything the demo
	// header user can.
	DemoOwnerEmail = "demo-owner@example.com"

	// DemoReaderEmail is the real account behind DemoReaderUserID's grant
	// model: notes:read and nothing else, in every configured tenant.
	DemoReaderEmail = "demo-reader@example.com"

	// DemoAcmeOnlyEmail is the real account behind DemoSingleTenantUserID's
	// grant model: notes:read in DemoSingleTenantID and nowhere else.
	DemoAcmeOnlyEmail = "demo-acme-only@example.com"
)

// demoSeedDomain is the email domain every demo account this app seeds
// lives on: the three addresses above and demo_admin.go's platform-staff
// account. It is declared to demoseed at construction, where it arms the
// subpackage's demo-only guard -- an account outside the declared domain is
// refused before anything is sent -- and it is the caller-side half of that
// guard: this app writes down that its boot-time seeding is demo seeding.
const demoSeedDomain = "example.com"

// demoSeedAccount pairs one demo account (registered as a real authn user)
// with the grant model of its demo_subject.go actor twin. Registration
// happens at boot; the membership and role grants then follow, under the
// user id authn actually assigned -- never under the twin's header id,
// which has no database row.
type demoSeedAccount struct {
	// actor is the demo_subject.go user id whose grant model this account
	// replicates. It is a label -- for the log lines and for anyone reading
	// this table -- never a key or an id the seed writes anywhere.
	actor string
	// email is what the account is registered with.
	email string
	// inEveryTenant grants the account in every configured tenant. False
	// restricts it to DemoSingleTenantID, mirroring how seedDemoGrants
	// restricts DemoSingleTenantUserID.
	inEveryTenant bool
	// roleKey is the role assigned wherever the account is granted,
	// mirroring the twin actor id's role in seedDemoGrants.
	roleKey string
}

// demoSeedAccounts is the three-actor model of seedDemoGrants expressed
// over real accounts. seedDemoGrants keeps seeding the fixed header ids --
// the pre-auth flows still act through them -- and this table is what
// makes the same demonstrations reachable through real sign-ins; when the
// header goes away (see DemoUserHeader), only the seedDemoGrants half is
// deleted and this table remains.
var demoSeedAccounts = []demoSeedAccount{
	{actor: DemoOwnerUserID, email: DemoOwnerEmail, inEveryTenant: true, roleKey: rbac.BuiltinRoleOwner},
	{actor: DemoReaderUserID, email: DemoReaderEmail, inEveryTenant: true, roleKey: demoReaderRoleKey},
	{actor: DemoSingleTenantUserID, email: DemoAcmeOnlyEmail, inEveryTenant: false, roleKey: demoReaderRoleKey},
}

// seedDemoUsers registers demoSeedAccounts through authn's demoseed helper --
// which POSTs the composed handler's real register route, so registration
// runs the real handler, the real password policy, the real rate limiter and
// the real users table rather than a second, parallel account-creation path
// that could drift from them; a password the policy refuses therefore fails
// startup, naming authn's answer code -- and grants each registered account
// the membership and role its model declares, per configured tenant and
// under its own tenant context (roles and bindings are tenant data --
// nothing here reads or writes across a tenant boundary).
//
// BuildServer calls it AFTER seedDemoGrants, which is what guarantees the
// roles this function AssignRole-s are already defined in every tenant. It
// runs only when the operator set APP_DEMO_USERS_PASSWORD; an empty
// password skips the seed, leaving the demo headers as the only demo
// identity.
//
// The seed is idempotent through and through: demoseed answers an
// already-registered account from authn's own exact-email lookup without
// posting the register route at all, so a restart debits no register budget
// (demoseed's own doc comment carries the budget reasoning), and the grant
// leg re-runs safely -- the org membership is ensured idempotently
// (addDemoOrgMembership) and AssignRole is idempotent. A boot that finds a
// demo account pre-existing therefore restores whatever the boot that
// created it left half-done -- a crash between registration and grants can
// no longer strand a memberless demo account -- and a restart against the
// same database loses none of the accounts' sign-in power.
func seedDemoUsers(ctx context.Context, handler http.Handler, authnService *authn.Service, svc *rbac.Service, orgModule *org.Module, tenants map[string]pkgcore.TenantID, password string) error {
	logger := obs.FromContext(ctx)
	// The lookup is authn's platform-operator search, appropriate here for
	// the reason demoseed's doc comment gives: this is the operator's own
	// boot-time configuration, the same trust level as the registration it
	// performs -- whereas the same search behind an HTTP route is gated.
	seeder, err := demoseed.NewSeeder(handler, authnService.SearchUsers, demoSeedDomain)
	if err != nil {
		return fmt.Errorf("reference-app: seed demo users: %w", err)
	}
	for _, account := range demoSeedAccounts {
		userID, alreadyExists, err := seeder.Register(ctx, account.email, password)
		if err != nil {
			return fmt.Errorf("reference-app: seed demo users: %w", err)
		}
		if alreadyExists {
			// A previous boot against this database created the account;
			// re-run the grant leg under its original id so a restart (or
			// an interrupted first boot) cannot leave it stranded.
			logger.Info("demo user already registered; re-asserting its org memberships and roles",
				"demo_user", account.actor,
				"user_id", userID)
		}
		if err := grantDemoSeedAccount(ctx, account, userID, svc, orgModule, tenants); err != nil {
			return fmt.Errorf("reference-app: seed demo users: %w", err)
		}
		if !alreadyExists {
			logger.Info("seeded demo user",
				"demo_user", account.actor,
				"user_id", userID)
		}
	}
	return nil
}

// grantDemoSeedAccount records the membership and role of one demo
// account, mirroring seedDemoGrants' per-tenant model: the membership goes
// to org's own memberships table (addDemoOrgMembership, below) -- the same
// table authn's sign-in path reads through signInMemberships, so this one
// write is what makes the account's sign-in succeed -- and the role to
// rbac, each under the tenant's own context. Which tenants an account
// reaches is the account's own decision (inEveryTenant), never "all
// tenants map iteration happens to visit" -- the same reason seedDemoGrants
// pins DemoSingleTenantID as a literal.
//
// Every step is repeatable, which is what lets seedDemoUsers re-run this
// on a boot that finds the account already registered: the org seat is
// ensured idempotently and AssignRole is idempotent (its own doc
// comment), so a repeat never duplicates a row or a binding and never
// moves an existing seat -- a demo account a flow later re-placed into a
// deeper org node keeps that node.
func grantDemoSeedAccount(ctx context.Context, account demoSeedAccount, userID string, svc *rbac.Service, orgModule *org.Module, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if !account.inEveryTenant && tenantID != DemoSingleTenantID {
			continue
		}
		if _, done := seeded[tenantID]; done {
			// Two demo hosts can map to one tenant; seed it once.
			continue
		}
		seeded[tenantID] = struct{}{}

		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		// The grant's audit Actor: this seed is boot-time automation, so
		// the row names the seed (demoSeedActorID) as a system actor --
		// rbac records every grant, and a host that hands it a bare
		// tenant context would see every one of these land with a blank
		// attribution.
		seedCtx := pkgcore.WithActor(tenantCtx, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: demoSeedActorID})

		if err := addDemoOrgMembership(seedCtx, orgModule, userID); err != nil {
			return fmt.Errorf("reference-app: add demo account to org roster in %q: %w", tenantID, err)
		}

		sub := rbac.Subject{TenantID: tenantID, UserID: userID}
		// A tenant-wide Scope, exactly as seedDemoGrants grants with:
		// this example has no organization tree to scope roles to (only
		// a bare root node, addDemoOrgMembership's own doc comment).
		if err := svc.AssignRole(seedCtx, sub, account.roleKey, rbac.Scope{}); err != nil {
			return fmt.Errorf("reference-app: grant %q to demo account in %q: %w", account.roleKey, tenantID, err)
		}
	}
	return nil
}

// addDemoOrgMembership gives userID a real org.Membership row in the
// caller's tenant (ctx) -- the row authn's sign-in path reads through
// signInMemberships (server.go), go/admin's impersonation-target
// membership check (validateTargetMembership) and every other
// org.MemberService.Get caller consult. The rbac.AssignRole grant above
// is a separate surface (rbac's own authorization decision); org knows
// nothing about it, and the sign-in store knows nothing beyond the row.
//
// It delegates to org's own idempotent EnsureRootSeat: the tenant's root
// node is created on the first account seeded into it, and a seat that
// already exists somewhere in the tenant -- one seat per person per tenant,
// including one a flow later re-placed into a deeper node -- is left
// exactly where it is, never moved and never duplicated. That is what makes
// this helper safe to call on every boot, and it is why the reference app
// seeds no deeper organization tree than a bare root: the root is the only
// node there is to place a demo account into.
func addDemoOrgMembership(ctx context.Context, orgModule *org.Module, userID string) error {
	if _, err := orgModule.Members().EnsureRootSeat(ctx, userID, "Demo Tenant", "group"); err != nil {
		return fmt.Errorf("add member to org roster: %w", err)
	}
	return nil
}
