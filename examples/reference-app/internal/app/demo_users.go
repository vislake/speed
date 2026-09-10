package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/authn"
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

// seedDemoUsers registers demoSeedAccounts through the composed handler's
// real register route and grants each registered account the membership
// and role its model declares, per configured tenant and under its own
// tenant context (roles and bindings are tenant data -- nothing here reads
// or writes across a tenant boundary).
//
// BuildServer calls it AFTER seedDemoGrants, which is what guarantees the
// roles this function AssignRole-s are already defined in every tenant. It
// runs only when the operator set APP_DEMO_USERS_PASSWORD; an empty
// password skips the seed, leaving the demo headers as the only demo
// identity.
//
// Registration goes through the HTTP surface on purpose: seedDemoUsers is
// a consumer of authn like any other, and taking the same register path a
// browser takes exercises the real handler, the real password policy, the
// real rate limiter and the real users table rather than a second,
// parallel account-creation path that could drift from them. A password
// the policy refuses therefore fails startup, naming authn's answer code.
//
// The seed is idempotent, and now the idempotence reaches all the way to
// the grants, not just the registrations: the account's memberships live
// in org's own memberships table and its roles in rbac's bindings table,
// both durable, both written under the user id authn assigned -- so a
// second boot against the same database finds the registration already
// there, recovers the user id authn assigned the first time through the
// exact email match of authn.Service.SearchUsers (the platform-operator
// lookup go/admin's own user search goes through), and re-runs the grant
// leg, which is safe to repeat by construction: the org membership is
// ensured idempotently (addDemoOrgMembership) and AssignRole is
// idempotent. A boot that finds a demo account pre-existing therefore
// restores whatever the boot that created it left half-done -- a crash
// between registration and grants can no longer strand a memberless demo
// account -- and a restart against the same database loses none of the
// accounts' sign-in power.
//
// One deliberately ordered detail: the existence question is asked of
// authn.Service.SearchUsers BEFORE the register route, never answered by
// POSTing and reading the already-registered conflict back. Both answers
// disclose the same fact to the same caller -- the boot-time demo seed is
// operator configuration, the same trust level as the registration it
// performs, which is what makes the platform-operator lookup appropriate
// here without an admin:search_users gate around it (that gate protects
// HTTP callers; this call is the operator's own boot) -- but asking first
// means a restart never POSTs the public register route at all, and
// therefore never debits authn's per-IP register budget
// (limitRegisterByIP, go/authn/ratelimit.go) for accounts that are
// already there. Under the distributed mode the budget lives in the
// shared Redis KVStore and accumulates across restarts, so a seed that
// POSTed on every boot could exhaust the budget within the quota window
// and fail the boot; posting only for genuinely absent accounts leaves
// restarts at zero register traffic whatever the deployment mode.
func seedDemoUsers(ctx context.Context, handler http.Handler, authnService *authn.Service, svc *rbac.Service, orgModule *org.Module, tenants map[string]pkgcore.TenantID, password string) error {
	logger := obs.FromContext(ctx)
	for _, account := range demoSeedAccounts {
		userID, alreadyExists, err := registerDemoUserIfAbsent(ctx, handler, authnService, account.email, password)
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

// registerDemoUserIfAbsent registers one demo account through the register
// route ONLY when it is not already in the users table: the existence
// question is answered first by the exact-email SearchUsers lookup
// (lookupRegisteredDemoUserID), and only an absent account POSTs
// /api/v1/authn/register. This is what keeps a restart from debiting the
// public register budget for accounts a previous boot already created
// (seedDemoUsers' own doc comment gives the budget reasoning in full);
// RegisterDemoUser itself still treats an unexpected already-registered
// conflict answer -- a concurrent first boot that registered the account
// between this boot's lookup and its POST -- as alreadyExists and recovers
// the assigned id, so a first-boot race between
// two replicas cannot double-register or strand an account.
//
// alreadyExists=true reports an account that needs no registration; its
// caller then re-asserts the grant leg under the returned id, the same
// shape both seedDemoUsers and seedDemoPlatformStaff reach after a
// conflict answer.
func registerDemoUserIfAbsent(ctx context.Context, handler http.Handler, authnService *authn.Service, email, password string) (userID string, alreadyExists bool, err error) {
	existingID, exists, err := lookupRegisteredDemoUserID(ctx, authnService, email)
	if err != nil {
		return "", false, fmt.Errorf("reference-app: check whether demo user %q already exists: %w", email, err)
	}
	if exists {
		return existingID, true, nil
	}

	userID, alreadyExists, err = RegisterDemoUser(ctx, handler, email, password)
	if err != nil {
		return "", false, err
	}
	if alreadyExists {
		// A concurrent first boot registered the account between the
		// lookup above and this POST; recover its id.
		userID, err = registeredDemoUserID(ctx, authnService, email)
		if err != nil {
			return "", false, err
		}
	}
	return userID, alreadyExists, nil
}

// lookupRegisteredDemoUserID asks authn.Service.SearchUsers whether the
// exact-email account exists -- the platform-operator lookup whose
// appropriateness for this boot-time, operator-configured seed is argued
// on seedDemoUsers' own doc comment -- answering (id, true) for the
// exactly-one-account case, (_, false) for none, and an error for a
// lookup failure or an impossible many-account collision.
func lookupRegisteredDemoUserID(ctx context.Context, authnService *authn.Service, email string) (string, bool, error) {
	users, err := authnService.SearchUsers(ctx, authn.UserSearchQuery{Email: email})
	if err != nil {
		return "", false, err
	}
	switch len(users) {
	case 0:
		return "", false, nil
	case 1:
		return users[0].ID, true, nil
	default:
		return "", false, fmt.Errorf("SearchUsers answered %d accounts for %q, want 0 or exactly 1", len(users), email)
	}
}

// registeredDemoUserID resolves the user id authn assigned to an email that
// is already registered -- the one thing the register conflict answer never
// discloses. The exact-email SearchUsers match is the platform-operator
// lookup of go/authn/search.go; the seed's boot-time use is documented on
// seedDemoUsers' own doc comment above.
func registeredDemoUserID(ctx context.Context, authnService *authn.Service, email string) (string, error) {
	users, err := authnService.SearchUsers(ctx, authn.UserSearchQuery{Email: email})
	if err != nil {
		return "", fmt.Errorf("look up pre-existing demo registration for %q: %w", email, err)
	}
	if len(users) != 1 {
		return "", fmt.Errorf("look up pre-existing demo registration for %q: SearchUsers answered %d accounts, want exactly 1",
			email, len(users))
	}
	return users[0].ID, nil
}

// RegisterDemoUser registers one demo account by POSTing the register
// payload to the composed handler -- in-process, through the same
// authn.Middleware + tenancy allowlist + mux + handler stack a browser
// request traverses -- and returns the user id authn assigned, or
// alreadyExists=true when the account is already in the users table. The
// callers (registerDemoUserIfAbsent and, through it, both seed functions)
// already asked SearchUsers whether the account exists, so reaching this
// POST at all means the account was genuinely absent a moment ago; the
// already-registered conflict answer here is the concurrent-first-boot
// race.
//
// Classification goes through the CODE, exactly as the register API
// reports it: any non-201 answer other than authn's
// email-already-registered conflict is an error naming the answer code, so
// a policy refusal, a rate limit or anything else fails the boot instead
// of silently producing a half-seeded demo. A rate-limit answer is
// distinguished from the other refusals on purpose rather than folded
// into the generic message: authn's register budget is 10 registrations
// per hour per client IP (limitRegisterByIP, go/authn/ratelimit.go), and
// with this seed POSTing only genuinely absent accounts, an
// authn.rate_limited answer on a first boot means the PUBLIC register
// budget is genuinely exhausted -- by other register traffic, or by
// several first-boots of fresh databases within the hour -- a transient,
// operator-actionable state, not a misconfiguration the generic message
// would send an operator hunting for. It therefore names the limit and
// the remedy.
func RegisterDemoUser(ctx context.Context, handler http.Handler, email, password string) (userID string, alreadyExists bool, err error) {
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", false, fmt.Errorf("reference-app: marshal demo register body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AuthnAPIPath+"/register", bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("reference-app: build demo register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		if resp.StatusCode == http.StatusConflict && envelope.Code == authn.ErrEmailAlreadyRegistered.Code {
			return "", true, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && envelope.Code == authn.ErrRateLimited.Code {
			return "", false, fmt.Errorf(
				"reference-app: registering demo user %q was refused by authn's public register rate limit (HTTP 429 %s, 10 registrations per hour per client IP): the seed only registers genuinely new accounts, so the limit is exhausted by other register traffic -- retry after the sliding window closes, or investigate the register traffic",
				email, authn.ErrRateLimited.Code)
		}
		return "", false, fmt.Errorf(
			"reference-app: registering demo user %q answered HTTP %d with code %q, want 201 or the already-registered conflict",
			email, resp.StatusCode, envelope.Code)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", false, fmt.Errorf("reference-app: decode demo register response: %w", err)
	}
	if created.ID == "" {
		return "", false, fmt.Errorf("reference-app: demo register response carried no id")
	}
	return created.ID, false, nil
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
// It idempotently ensures the tenant's org root node exists (CreateRoot
// on the first account seeded into a given tenant, Root thereafter, both
// under the tenant context ctx already carries) and binds userID to it --
// the reference app seeds no deeper organization tree, so the root is the
// only node there is to place a demo account into. A seat that already
// exists somewhere in the tenant (org.MemberService.Add's own
// ErrMembershipExists answer -- one seat per person per tenant) is left
// exactly where it is, never moved and never duplicated: this helper is
// safe to call on every boot, which is what makes the demo seed's
// idempotence extend to membership rows and not only to user rows.
func addDemoOrgMembership(ctx context.Context, orgModule *org.Module, userID string) error {
	root, err := orgModule.Tree().Root(ctx)
	if err != nil {
		if !orgCodeIs(err, org.ErrNodeNotFound.Code) {
			return fmt.Errorf("look up tenant's org root: %w", err)
		}
		root, err = orgModule.Tree().CreateRoot(ctx, "Demo Tenant", "group")
		if err != nil {
			return fmt.Errorf("create tenant's org root: %w", err)
		}
	}
	if _, err := orgModule.Members().Add(ctx, userID, root.ID); err != nil {
		if !orgCodeIs(err, org.ErrMembershipExists.Code) {
			return fmt.Errorf("add member to org roster: %w", err)
		}
	}
	return nil
}
