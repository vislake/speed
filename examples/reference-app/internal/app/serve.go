package app

// This file runs the host's pre-serve step: the demo account seeds, which
// register through the composed handler, and the self-service signup chain
// that must be installed after them.

import (
	"context"
	"net/http"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// runPreServe runs the pre-serve assembly step: it runs after the HTTP face
// exists (so the seeds can register through the same composed handler a
// browser reaches) and before anything listens. The host's pre-serve
// component (handed the app component's face) calls it.
func (b *serverBuild) runPreServe(ctx context.Context, view assemblyView, handler http.Handler) error {
	// The demo-user seeds run last, once the composed handler exists: they
	// register the demo accounts through the same register route a browser
	// would use, which needs the whole chain above it. Each seed is opt-in
	// under its OWN variable (APP_DEMO_USERS_PASSWORD for the three
	// customer-tenant demo accounts, APP_DEMO_PLATFORM_STAFF_PASSWORD for
	// the rbac.SystemDomain platform administrator) and runs independently
	// of the other: the platform administrator must never be seeded from
	// the ordinary demo users' password variable. An empty variable skips
	// the seed.
	if b.cfg.DemoUsersPassword != "" {
		if seedErr := demo.SeedDemoUsers(ctx, handler, b.authnModule.Service(), b.rbacService, b.orgModule, b.cfg.HostTenants, b.cfg.DemoUsersPassword); seedErr != nil {
			return seedErr
		}
	}
	// SeedDemoPlatformStaff is admin's own first-consumer demo account: a
	// real registered user whose ONLY membership is rbac.SystemDomain,
	// holding BuiltinRoleOwner there -- every admin:* permission included,
	// since owner carries every permission any module declared.
	if b.cfg.DemoPlatformStaffPassword != "" {
		if _, seedErr := demo.SeedDemoPlatformStaff(ctx, handler, b.memberships, b.rbacService, b.authnModule.Service(), b.cfg.DemoPlatformStaffPassword); seedErr != nil {
			return seedErr
		}
	}

	// The self-service signup chain (self_service.go) installs AFTER both
	// demo seeds, which is the ordering that keeps the demo path intact:
	// the seeds' registrations run above, before the provisioner's
	// subscription exists, so the demo accounts provision no clinic of
	// their own and keep exactly the memberships and grants the seeds give
	// them; every registration that reaches the composed handler from here
	// on -- a browser's, or a flow test's -- is a self-service registration
	// and gets its own clinic tenant, org root, membership, owner grant,
	// demo-plan subscription and credit seed before its 201 answer leaves
	// (on the in-process bus, where the subscription's provisioning runs
	// synchronously inside the register request itself). The clinic's org
	// root is named after the registrant's own authn display name; the
	// three billing services ride along as the subscription and credit half
	// of the provisioned clinic -- the same services the two demo seeds
	// above just used -- and the standaloneQueue rides along for the
	// failure half of the guarantee: a synchronous provisioning attempt
	// that fails enqueues the retry job that converges the clinic, and the
	// queue's worker starts with the engine's stage 8, so the retry runs on
	// this same process's pool. cfg.FailSelfServiceProvision rides along as
	// the failure-injection hook -- nil under the disabled default, armed
	// either by ConfigFromEnv's own env-driven parse or by a test's
	// ServerConfig.
	if wireErr := wireSelfService(ctx, view.catalog, view.events, b.orgModule, b.rbacService, b.authnModule.Service(), b.billingModule.Plans(), b.billingModule.Subscriptions(), b.billingModule.Credits(), b.standaloneQueue, view.jobs, b.cfg.FailSelfServiceProvision); wireErr != nil {
		return wireErr
	}
	return nil
}
