package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	obs "github.com/vislake/speed/go/observability"
)

// demoUsersPasswordEnv gates the boot-time demo-user seed. Unset -- the
// default `go run ./cmd/server` ships with -- means no demo accounts are
// registered and demoUserHeader remains the only way to act as a demo
// user, exactly as before this file existed. Setting it to a passphrase
// registers the three demo accounts below on every boot, so a browser
// visitor can sign in as any of them with a real account, a real
// membership and a real rbac grant -- no header involved.
//
// The passphrase is not secret in the same sense as the config keys are,
// but it is also not a hardcoded default: it exists to gate a DEMO
// affordance behind an operator's deliberate choice, and the constant
// shape (SPEED_*_PASSWORD) mirrors the config-key pair configFromEnv
// reads for the same reason -- an env var is visible, auditable and
// per-deployment in a way a compiled-in default is not.
//
// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
// value: gosec's hardcoded-credential heuristic matches on the substring
// "Password" in the identifier alone, the same false positive go/authn's
// and go/sharing's own identically-shaped name constants are already
// excepted from elsewhere in this codebase.
const demoUsersPasswordEnv = "SPEED_DEMO_USERS_PASSWORD"

// The three demo accounts seedDemoUsers registers when
// SPEED_DEMO_USERS_PASSWORD is set. Each is the real-account twin of the
// demo_subject.go actor id its grant model mirrors: demo-owner /
// demo-reader / demo-acme-only are header user ids with no database row
// behind them, while these accounts exist as real authn users whose
// memberships and roles are granted under the user id authn assigns at
// registration.
const (
	// demoOwnerEmail is the real account behind demoOwnerUserID's grant
	// model: membership and the built-in owner role in every configured
	// tenant, so a browser signed in as it can do everything the demo
	// header user can.
	demoOwnerEmail = "demo-owner@example.com"

	// demoReaderEmail is the real account behind demoReaderUserID's grant
	// model: notes:read and nothing else, in every configured tenant.
	demoReaderEmail = "demo-reader@example.com"

	// demoAcmeOnlyEmail is the real account behind demoSingleTenantUserID's
	// grant model: notes:read in demoSingleTenantID and nowhere else.
	demoAcmeOnlyEmail = "demo-acme-only@example.com"
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
	// restricts it to demoSingleTenantID, mirroring how seedDemoGrants
	// restricts demoSingleTenantUserID.
	inEveryTenant bool
	// roleKey is the role assigned wherever the account is granted,
	// mirroring the twin actor id's role in seedDemoGrants.
	roleKey string
}

// demoSeedAccounts is the three-actor model of seedDemoGrants expressed
// over real accounts. seedDemoGrants keeps seeding the fixed header ids --
// the pre-auth flows still act through them -- and this table is what
// makes the same demonstrations reachable through real sign-ins; when the
// header goes away (see demoUserHeader), only the seedDemoGrants half is
// deleted and this table remains.
var demoSeedAccounts = []demoSeedAccount{
	{actor: demoOwnerUserID, email: demoOwnerEmail, inEveryTenant: true, roleKey: rbac.BuiltinRoleOwner},
	{actor: demoReaderUserID, email: demoReaderEmail, inEveryTenant: true, roleKey: demoReaderRoleKey},
	{actor: demoSingleTenantUserID, email: demoAcmeOnlyEmail, inEveryTenant: false, roleKey: demoReaderRoleKey},
}

// seedDemoUsers registers demoSeedAccounts through the composed handler's
// real register route and grants each registered account the membership
// and role its model declares, per configured tenant and under its own
// tenant context (roles and bindings are tenant data -- nothing here reads
// or writes across a tenant boundary).
//
// buildServer calls it AFTER seedDemoGrants, which is what guarantees the
// roles this function AssignRole-s are already defined in every tenant. It
// runs only when the operator set SPEED_DEMO_USERS_PASSWORD; an empty
// password leaves the demo-header world exactly as it was.
//
// Registration goes through the HTTP surface on purpose: seedDemoUsers is
// a consumer of authn like any other, and taking the same register path a
// browser takes exercises the real handler, the real password policy, the
// real rate limiter and the real users table rather than a second,
// parallel account-creation path that could drift from them. A password
// the policy refuses therefore fails startup, naming authn's answer code.
//
// The seed is idempotent only up to a point, and the point is deliberate:
// registering an account that already exists is reported as a conflict
// (authn's exists-answers never disclose more than the code), and the
// account's memberships live in stores that do not survive a restart --
// demoMemberships is in-process, and the role bindings sit in the database
// under the user id the FIRST boot registered. A second boot against the
// same database therefore cannot reach into the past and grant what the
// first boot granted: it logs a warning and leaves that account alone,
// fail-closed, and the operator who wants the demo accounts back starts
// from a fresh database (SPEED_DB_PATH). That honest skip is preferred
// over pretending a skip is a seed.
func seedDemoUsers(ctx context.Context, handler http.Handler, memberships *demoMemberships, svc *rbac.Service, tenants map[string]pkgcore.TenantID, password string) error {
	logger := obs.FromContext(ctx)
	for _, account := range demoSeedAccounts {
		userID, alreadyExists, err := registerDemoUser(ctx, handler, account.email, password)
		if err != nil {
			return fmt.Errorf("reference-app: seed demo users: %w", err)
		}
		if alreadyExists {
			// A previous boot against this database created the account.
			// Its memberships live in the in-process store that boot
			// owned, and its rbac grants sit in the database under the
			// user id only that boot knew -- neither can be recovered
			// from here, and inventing grants would misrepresent state.
			logger.Warn("demo user already exists; leaving it unseeded so it fails closed",
				"demo_user", account.actor)
			continue
		}
		if err := grantDemoSeedAccount(ctx, account, userID, memberships, svc, tenants); err != nil {
			return fmt.Errorf("reference-app: seed demo users: %w", err)
		}
		logger.Info("seeded demo user",
			"demo_user", account.actor,
			"user_id", userID)
	}
	return nil
}

// registerDemoUser registers one demo account by POSTing the register
// payload to the composed handler -- in-process, through the same
// authn.Middleware + tenancy allowlist + mux + handler stack a browser
// request traverses -- and returns the user id authn assigned, or
// alreadyExists=true when the account is already in the users table.
//
// Classification goes through the CODE, exactly as the register API
// reports it: any non-201 answer other than authn's
// email-already-registered conflict is an error naming the answer code, so
// a policy refusal, a rate limit or anything else fails the boot instead
// of silently producing a half-seeded demo.
func registerDemoUser(ctx context.Context, handler http.Handler, email, password string) (userID string, alreadyExists bool, err error) {
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", false, fmt.Errorf("reference-app: marshal demo register body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authnAPIPath+"/register", bytes.NewReader(payload))
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

// grantDemoSeedAccount records the membership and role of one freshly
// registered demo account, mirroring seedDemoGrants' per-tenant model: the
// membership goes to the same seam authn asks about at sign-in
// (demoMemberships), the role to rbac, each under the tenant's own
// context. Which tenants an account reaches is the account's own decision
// (inEveryTenant), never "all tenants map iteration happens to visit" --
// the same reason seedDemoGrants pins demoSingleTenantID as a literal.
func grantDemoSeedAccount(ctx context.Context, account demoSeedAccount, userID string, memberships *demoMemberships, svc *rbac.Service, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if !account.inEveryTenant && tenantID != demoSingleTenantID {
			continue
		}
		if _, done := seeded[tenantID]; done {
			// Two demo hosts can map to one tenant; seed it once.
			continue
		}
		seeded[tenantID] = struct{}{}

		memberships.Grant(userID, tenantID)

		sub := rbac.Subject{TenantID: tenantID, UserID: userID}
		// A tenant-wide Scope, exactly as seedDemoGrants grants with:
		// this example has no organization tree to scope to.
		if err := svc.AssignRole(pkgcore.WithTenant(ctx, tenantID), sub, account.roleKey, rbac.Scope{}); err != nil {
			return fmt.Errorf("reference-app: grant %q to demo account in %q: %w", account.roleKey, tenantID, err)
		}
	}
	return nil
}
