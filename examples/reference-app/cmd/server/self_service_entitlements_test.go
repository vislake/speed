package main

// self_service_entitlements_test.go closes the entitlement half of the
// self-service clinic story with real runs: the provisioning chain
// self_service.go drives must leave a newly provisioned clinic with a
// subscription and a credit balance, not just org, membership and owner
// grant. The demo seeding (demo_entitlements.go's
// seedDemoEntitlements, demo_credits.go's seedDemoCredits) grants only
// the boot-configured demo tenants (cfg.HostTenants); a clinic a
// self-service registration provisions must be granted by provision
// itself, or its owner's very first smile simulation is refused at
// internal/smilesim's entitlement pre-flight (aigateway.entitlement_denied,
// on a zero balance).
//
// Provision subscribes the new clinic to the demo Plan and seeds its
// credit balance through the SAME shared per-tenant helpers the
// boot-time demo seeding itself calls
// (ensureDemoSubscription and grantDemoCredits), so the two paths cannot
// drift apart. Each test
// below drives the REAL composed HTTP stack (buildServer behind
// httptest), never a mock, and reads balances and subscriptions back
// through SECOND database connections -- the deterministic read shapes
// billing_credit_flow_test.go and entitlements_flow_test.go establish.
//
//   - TestSelfServiceSignup_ClinicOwner_RunsASmileSimulationToCompletion
//     is regression (a): a freshly self-registered clinic's owner can run
//     a smile simulation to completion through the real stack -- without
//     provision's grants the simulate request answers 403
//     aigateway.entitlement_denied.
//   - TestSelfServiceSignup_ClinicOwner_HoldsTheDemoSubscriptionAndSeedBalance
//     is regression (b): the same clinic's provisioning granted an Active
//     subscription to the demo Plan and a demoSimulationCreditGrant
//     balance -- no subscription row and a zero balance is the
//     un-granted shape.
//   - TestSelfServiceSignup_ClinicProvisioningRetry_GrantsSubscriptionAndCreditsToo
//     is regression (d): the failure-injection/retry shape
//     self_service_test.go's own suite pins must converge the NEW steps
//     too -- a clinic whose synchronous provisioning attempt was injected
//     to fail gains its subscription and credits from the retry job,
//     exactly as it gains its org membership.
//   - TestSelfServiceSignup_DemoTenants_KeepTheirBootSeededSubscriptionAndBalance
//     is regression (c): the demo tenants' boot seeding is invariant
//     under the provisioning path's use of the same shared per-tenant
//     helpers -- one Active demo subscription and exactly one
//     demoSimulationCreditGrant balance each, on the unchanged demo Plan
//     with its unchanged Boolean grants.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// selfServiceEntitlementEmails are the accounts each journey below
// registers -- unique per test so a fresh database never collides, and
// shared with neither self_service_test.go's nor any other suite's
// accounts.
const (
	selfServiceEntitlementSimulateEmail = "entitlement-clinic-founder@example.com"
	selfServiceEntitlementRowEmail      = "entitlement-row-founder@example.com"
	selfServiceEntitlementRetryEmail    = "entitlement-retry-founder@example.com"
)

// assertTenantHoldsActiveDemoSubscription reads tenantID's Active
// subscription through a second connection and requires it to exist on
// the demo Plan -- the row-level shape of "the tenant may use the gated
// AI routes" (billing's EntitlementsService.Check reads exactly this row,
// go/billing/subscription.go).
func assertTenantHoldsActiveDemoSubscription(t *testing.T, cfg serverConfig, tenantID pkgcore.TenantID) {
	t.Helper()

	ctx := pkgcore.WithTenant(context.Background(), tenantID)
	active, err := openBillingModule(t, cfg).Subscriptions().Active(ctx)
	if err != nil {
		t.Fatalf("read the active subscription of %q: %v", tenantID, err)
	}
	if active == nil {
		t.Fatalf("tenant %q holds no Active subscription -- provisioning must subscribe it to the demo Plan", tenantID)
	}
	plan, err := openBillingModule(t, cfg).Plans().Resolve(ctx, "", demoEntitlementPlanKey)
	if err != nil {
		t.Fatalf("resolve the demo entitlement plan: %v", err)
	}
	if active.PlanID != plan.ID {
		t.Fatalf("tenant %q's Active subscription is on plan %q, want the demo Plan %q", tenantID, active.PlanID, plan.ID)
	}

	// The demo Plan's grants are the exact model-access grants the app's
	// routes judge (demo_entitlements.go): one Boolean true per logical
	// model key, never something a drifted copy could weaken.
	for _, featureKey := range []string{"model:" + consult.LogicalModel, "model:" + smilesim.LogicalModel} {
		grant, ok := plan.Grant(featureKey)
		if !ok {
			t.Fatalf("demo Plan grants no %q feature -- the seeded plan must grant every model key the routes gate on", featureKey)
		}
		boolean, isBool := grant.Value.(bool)
		if !isBool || !boolean {
			t.Fatalf("demo Plan's %q grant = %#v, want Boolean true -- quota-kind grants would panic the nil usage reader", featureKey, grant.Value)
		}
	}
}

// TestSelfServiceSignup_ClinicOwner_RunsASmileSimulationToCompletion is
// regression (a): a freshly self-registered clinic's owner drives a real
// smile simulation -- photo upload through storage's HTTP surface as the
// clinic's own principal (no demo header, so the rbac gate decides
// against the clinic's owner grant), simulate through smilesim's own
// route, job polled to success, the fake image provider genuinely reached
// exactly once -- the whole block-D journey a browser would run.
//
// The un-granted shape this test guards: a clinic whose provisioning
// granted no subscription answers 403 with
// aigateway.entitlement_denied (the entitlement pre-flight,
// internal/smilesim/service.go) before any job exists -- the exact
// refusal this test's assertion names in its failure output.
func TestSelfServiceSignup_ClinicOwner_RunsASmileSimulationToCompletion(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	// The browser journey: register, then the browser-shaped sign-in that
	// lands the principal in the registration's own clinic.
	userID := registerFreshAccount(t, srv, selfServiceEntitlementSimulateEmail, selfServicePassword)
	status, code, token, tenant := browserSignIn(t, srv, selfServiceEntitlementSimulateEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the freshly registered clinic owner: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	clinic := clinicTenantOf(userID)
	if tenant != clinic {
		t.Fatalf("sign-in landed the principal in tenant %q, want its own clinic %q", tenant, clinic)
	}

	// Upload and complete a patient photo through storage's own real HTTP
	// surface as the clinic owner -- the same three-step lifecycle
	// storage_flow_test.go's suite drives for the demo tenants, here with
	// no demo-user header, so the rbac gate decides against the verified
	// Principal whose owner grant the provisioning assigned in the clinic
	// tenant.
	photo := jpegWithExif(t)
	declared := declareUpload(t, srv, token, "", int64(len(photo)), "image/jpeg", "")
	if declared.State != "uploading" {
		t.Fatalf("clinic owner's upload declaration state = %q, want %q", declared.State, "uploading")
	}
	uploadBytes(t, srv, token, "", declared.ID, photo)
	completed := completeObject(t, srv, token, "", declared.ID)
	if completed.State != "completed" {
		t.Fatalf("clinic owner's photo state = %q, want completed", completed.State)
	}

	// The simulate request: 202 carrying a job id means the clinic's
	// subscription passed the entitlement pre-flight and its seeded
	// balance covered the reservation -- both absent without the grants. A
	// refusal is decoded and named in the failure so a missing-grant run fails
	// with the defect's own code.
	simulateBody, err := json.Marshal(map[string]string{"photo_object_id": completed.ID})
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	var simulateOut struct {
		JobID string `json:"job_id"`
		Code  string `json:"code"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&simulateOut); decodeErr != nil {
		resp.Body.Close()
		t.Fatalf("decode simulate response: %v", decodeErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s as the clinic owner: status = %d, code = %q, want 202 "+
			"-- the clinic's registration must grant the subscription that opens this route",
			smileSimulatePath, resp.StatusCode, simulateOut.Code)
	}

	final := waitForSmileSimSucceeded(t, srv, token, simulateOut.JobID, time.Now().Add(5*time.Second))
	if finalStatus, _ := final["status"].(string); finalStatus != "succeeded" {
		t.Fatalf("final job status = %v, want \"succeeded\": %+v", final["status"], final)
	}
	if outputObjectID, _ := final["output_object_id"].(string); outputObjectID == "" {
		t.Fatalf("succeeded clinic job carries no output_object_id: %+v", final)
	}

	// The fake provider was genuinely reached exactly once -- the whole
	// pipeline ran for the clinic's own job, nothing short-circuited.
	if imgServer.requests != 1 {
		t.Errorf("fake image endpoint received %d request(s), want exactly 1", imgServer.requests)
	}

	// The run genuinely debited the clinic's seeded balance: exactly one
	// simulation's worth is gone and nothing is left Reserved (the
	// Confirm settled the reservation on the terminal poll).
	after := creditBalanceFor(t, cfg, clinic)
	if want := demoSimulationCreditGrant - smilesim.CreditsPerSimulation; after.Available != want {
		t.Errorf("clinic balance after a successful simulation = %+v, want Available %d (the %d demo seed minus CreditsPerSimulation %d)",
			after, want, demoSimulationCreditGrant, smilesim.CreditsPerSimulation)
	}
	if after.Reserved != 0 {
		t.Errorf("clinic balance after a CONFIRMED generation = %+v, want Reserved 0", after)
	}
}

// TestSelfServiceSignup_ClinicOwner_HoldsTheDemoSubscriptionAndSeedBalance
// is regression (b): the clinic a fresh registration provisions holds the
// demo Plan's Active subscription (the row billing's EntitlementsService
// judges every gated request against) and a credit balance of exactly
// demoSimulationCreditGrant -- the same grant shape and value the demo
// seeding gives a demo clinic. Reads go through second connections, so
// the assertion is on the database's own rows, never on what the server
// process happens to hold in memory.
//
// The un-granted shape this test guards: no subscription row and a
// balance of exactly zero -- the numbers this test's assertions name in
// their failure output.
func TestSelfServiceSignup_ClinicOwner_HoldsTheDemoSubscriptionAndSeedBalance(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)

	userID := registerFreshAccount(t, srv, selfServiceEntitlementRowEmail, selfServicePassword)
	clinic := clinicTenantOf(userID)

	// The subscription half of regression (b): the Active demo
	// subscription and the demo Plan's unchanged Boolean grants.
	assertTenantHoldsActiveDemoSubscription(t, cfg, clinic)

	// The credit half: exactly the demo seed grant -- non-zero, so the
	// clinic's first simulations are payable, and exactly
	// demoSimulationCreditGrant, so the provisioning seeded once and
	// never double-seeded.
	bal := creditBalanceFor(t, cfg, clinic)
	if bal.Available != demoSimulationCreditGrant || bal.Reserved != 0 {
		t.Fatalf("clinic %s credit balance = %+v, want Available %d / Reserved 0 "+
			"(the provisioning must seed the demoSimulationCreditGrant balance, not leave the clinic at zero)",
			clinic, bal, demoSimulationCreditGrant)
	}
}

// TestSelfServiceSignup_ClinicProvisioningRetry_GrantsSubscriptionAndCreditsToo
// is regression (d): the failure-injection/retry shape self_service_test.go's
// own suite pins (an injected synchronous failure, recovered by the retry
// job the queue runs) must converge the provisioning's subscription and
// credit steps as faithfully as it converges the org ones -- the retry is
// the clinic's only recovery, so a subscription or credit step that only
// the synchronous attempt could land would strand the clinic's paid
// halves the same way a missing membership strands its sign-in.
//
// The shape mirrors TestSelfServiceSignup_ProvisioningFailure_RetriedUntilTheClinicExists
// exactly (the same failOnceProvisioning hook armed through
// cfg.failSelfServiceProvision before buildServer), then goes one step
// further: once the retry's org work is visible, the subscription and the
// seeded balance must follow from the SAME retried provision -- polled
// through second connections rather than assumed synchronous.
//
// The stranded shape this test guards: a retry that re-ran provision but
// skipped the subscription and credit steps would leave the balance poll
// below timing out on a permanent zero.
func TestSelfServiceSignup_ClinicProvisioningRetry_GrantsSubscriptionAndCreditsToo(t *testing.T) {
	inject := &failOnceProvisioning{}
	cfg := testConfig(t)
	cfg.failSelfServiceProvision = inject.fail
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer func() {
		srv.Close()
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	}()

	userID := registerFreshAccount(t, srv, selfServiceEntitlementRetryEmail, selfServicePassword)
	if !inject.observed() {
		t.Fatal("the synchronous provisioning attempt never consumed the injected failure, so nothing here exercises the retry")
	}
	clinic := clinicTenantOf(userID)

	// The retry converged the org half: the membership row is the durable
	// record of a provisioning attempt that ran to (near) completion, and
	// after the failed synchronous attempt the completing attempt can
	// only be the retry job's.
	waitForClinicMembership(t, cfg.SQLitePath, clinic, userID)

	// The SAME retried provision must then land the billing half: the
	// credit grant is provision's last step, so a non-zero balance means
	// the whole provision -- subscription included -- ran through. Poll,
	// because the retry job's writes land asynchronously on the queue's
	// own worker.
	deadline := time.Now().Add(15 * time.Second)
	var bal billing.CreditBalance
	for {
		bal = creditBalanceFor(t, cfg, clinic)
		if bal.Available != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clinic %s never received its seeded balance after the retried provisioning (balance stayed %+v) -- "+
				"the retry must converge the credit seed a failed synchronous attempt skipped",
				clinic, bal)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if bal.Available != demoSimulationCreditGrant || bal.Reserved != 0 {
		t.Fatalf("clinic %s credit balance after the retried provisioning = %+v, want Available %d / Reserved 0",
			clinic, bal, demoSimulationCreditGrant)
	}
	assertTenantHoldsActiveDemoSubscription(t, cfg, clinic)
}

// TestSelfServiceSignup_DemoTenants_KeepTheirBootSeededSubscriptionAndBalance
// is regression (c): the demo tenants' boot seeding is invariant under
// the provisioning path's use of the same shared per-tenant helpers --
// each cfg.HostTenants tenant holds one Active subscription to the demo
// Plan (whose Boolean grants are unchanged) and exactly one
// demoSimulationCreditGrant balance, read through second connections.
// The test passes whether the shared helpers or a private path seeded a
// given tenant: its job is the demo tenants' seeded state, not which
// internal path produced it.
func TestSelfServiceSignup_DemoTenants_KeepTheirBootSeededSubscriptionAndBalance(t *testing.T) {
	_, cfg, _ := buildTestServer(t)

	checked := make(map[pkgcore.TenantID]struct{}, len(cfg.HostTenants))
	for _, tenantID := range cfg.HostTenants {
		if _, done := checked[tenantID]; done {
			continue
		}
		checked[tenantID] = struct{}{}

		assertTenantHoldsActiveDemoSubscription(t, cfg, tenantID)

		bal := creditBalanceFor(t, cfg, tenantID)
		if bal.Available != demoSimulationCreditGrant || bal.Reserved != 0 {
			t.Fatalf("demo tenant %s credit balance = %+v, want Available %d / Reserved 0 -- "+
				"the boot-time demo seed must grant each demo tenant exactly one demoSimulationCreditGrant",
				tenantID, bal, demoSimulationCreditGrant)
		}
	}
	if len(checked) < 2 {
		t.Fatalf("cfg.HostTenants holds %d distinct demo tenants, want at least 2 for this regression to mean anything", len(checked))
	}
}
