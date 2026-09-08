package main

// entitlements_flow_test.go proves this app's two AI gateway halves --
// internal/consult's chat route and internal/smilesim's image-generation
// route -- run behind go/billing's real Entitlements seam end to end,
// driven through the REAL composed HTTP stack exactly like
// consult_flow_test.go and billing_credit_flow_test.go drive their
// proofs.
//
// What the seam gates, and what this file proves about it:
//
//   - cmd/server/server.go wires aigateway.WithEntitlements over
//     billingModule.Entitlements() (an EntitlementsFunc closure -- the
//     sanctioned adapter shape go/ai-gateway/seams.go documents), so
//     go/ai-gateway's own checkEntitlement now judges every Chat /
//     ChatStream / GenerateImage call against a REAL go/billing
//     EntitlementsService.Check before any provider is reached.
//   - Check keys on "model:" + the logical model the route asks for
//     ("model:chat:default" for every consult Suggest call,
//     "model:image:smile-simulation" for every smile-simulation
//     generation), which is exactly the grant set cmd/server's
//     seedDemoEntitlements stamps on the demo Plan every boot resolves
//     (demo_entitlements.go) -- so a demo tenant's requests pass, and a
//     tenant whose Active subscription was canceled is refused.
//   - The refusal arrives as go/ai-gateway's own coded error
//     (ErrEntitlementDenied, Forbidden, "aigateway.entitlement_denied",
//     carrying model and reason params) and travels out through the
//     routes' existing apperr-based envelope writers unchanged
//     (writeConsultError/writeSmileSimError -- no new host-side
//     translation was needed).
//
// The four tests below are one happy path and one refusal path per half.
// Both refusal legs cancel tenant-acme's demo subscription mid-test
// through a SECOND database connection's real SubscriptionService.Cancel
// call -- never a database row hand-edited, never a mock service. That
// cancellation takes effect immediately with no cache to invalidate:
// EntitlementsService.Check reads the tenant's subscription row fresh on
// every call (go/billing/subscription.go's own doc comment), and the
// boot-time seed runs exactly once per process (server.go), so a
// mid-test cancel is never undone while this process lives -- the honest
// stand-in for "this tenant's subscription lapsed between requests",
// exactly the state change a real payment-channel failure would drive.
//
// Why tenant-acme and never a fresh tenant: seedDemoEntitlements runs
// unconditionally at boot for every tenant in cfg.HostTenants, so every
// test's fresh temp-file database starts with tenant-acme and
// tenant-globex subscribed. A tenant outside that map would be refused
// by this app's rbac layer (rbac.permission_denied on the note create or
// storage upload this suite needs) before any entitlement question was
// ever reached -- proving nothing about billing at all; the same reason
// billing_credit_flow_test.go's own refusal leg documents for reusing
// tenant-acme.
//
// The subscription state this suite depends on is a demo seed, and the
// purchase leg that would create a subscription for real stays out of
// scope -- see demo_entitlements.go's package doc comment.
import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// openBillingModule opens a SECOND dbkit connection to cfg.SQLitePath and
// returns the billing.Module reached through it -- openBillingCredits'
// (billing_credit_flow_test.go) identical pattern, returning the whole
// module rather than just Credits() because this suite needs
// Subscriptions() to drive the lifecycle (and could reach Entitlements()
// directly for an assertion, should one ever need the judgment itself
// rather than its HTTP consequence). No migration call is needed on this
// second connection: buildServer's own migrationRegistry.Apply already
// applied go/billing's migrations to cfg.SQLitePath before any test's
// server ever started serving.
func openBillingModule(t *testing.T, cfg serverConfig) *billing.Module {
	t.Helper()

	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})
	return billing.NewModule(db, nil)
}

// cancelActiveDemoSubscription cancels tenantID's Active demo
// subscription through a real SubscriptionService.Cancel call on a second
// connection -- the mid-test stand-in for the tenant's subscription
// lapsing. SubscriptionStatusCanceled is terminal (subscription.go's own
// status doc), and the boot-time seed never runs again within this
// process, so the refusal the tests below assert stays in force for the
// rest of the test.
func cancelActiveDemoSubscription(t *testing.T, cfg serverConfig, tenantID pkgcore.TenantID) {
	t.Helper()

	ctx := pkgcore.WithTenant(context.Background(), tenantID)
	active, err := openBillingModule(t, cfg).Subscriptions().Active(ctx)
	if err != nil {
		t.Fatalf("read the active subscription of %q: %v", tenantID, err)
	}
	if active == nil {
		t.Fatalf("tenant %q holds no Active demo subscription to cancel -- did seedDemoEntitlements fail to run at boot?", tenantID)
	}
	if _, err := openBillingModule(t, cfg).Subscriptions().Cancel(ctx, active.ID); err != nil {
		t.Fatalf("cancel the demo subscription of %q: %v", tenantID, err)
	}
}

// decodeErrorEnvelope decodes resp's body as this app's structured
// {code, params} error envelope (the shape writeConsultError and
// writeSmileSimError both produce) and returns the two halves.
func decodeErrorEnvelope(t *testing.T, resp *http.Response) (string, map[string]any) {
	t.Helper()

	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return out.Code, out.Params
}

// assertEntitlementDeniedEnvelope asserts the shape every refusal leg
// below expects: the gateway's own coded 403 with the model and reason
// params go/ai-gateway's checkEntitlement stamps on it (gateway.go), read
// back through the routes' ordinary envelope writers -- nothing added on
// the host side for this seam.
func assertEntitlementDeniedEnvelope(t *testing.T, resp *http.Response, logicalModel, reason string) {
	t.Helper()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden, the ErrEntitlementDenied status)", resp.StatusCode, http.StatusForbidden)
	}
	code, params := decodeErrorEnvelope(t, resp)
	if code != aigateway.ErrEntitlementDenied.Code {
		t.Errorf("error code = %q, want %q", code, aigateway.ErrEntitlementDenied.Code)
	}
	if got, _ := params["model"].(string); got != logicalModel {
		t.Errorf("error param model = %q, want %q -- the gate must name the logical model the route asked for", got, logicalModel)
	}
	if got, _ := params["reason"].(string); got != reason {
		t.Errorf("error param reason = %q, want %q", got, reason)
	}
}

// TestConsultSuggest_SeededSubscriptionPasses_ReachesChatProvider is the
// chat half's happy path: tenant-acme holds the Active demo subscription
// seedDemoEntitlements gave it at boot, so its consult Suggest request
// clears go/ai-gateway's entitlement gate and genuinely reaches the fake
// OpenAI-compatible chat endpoint -- asserted on the fake server's own
// recorded request (the routed vendor model, never the logical key),
// exactly the way consult_flow_test.go proves the rest of the chat
// pipeline.
func TestConsultSuggest_SeededSubscriptionPasses_ReachesChatProvider(t *testing.T) {
	const wantSuggestion = "Ask about the discoloration timeline."
	aiServer := newFakeOpenAICompatibleServer(t, wantSuggestion)
	srv, cfg := buildConsultTestServer(t, aiServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "ent-chat-owner")

	const noteText = "Patient reports a dark spot on an upper incisor."
	noteID := createNoteAs(t, srv, token, noteText)

	resp := consultSuggestRequest(t, srv, token, noteID)
	defer resp.Body.Close()

	var out struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want %d; body = %+v", consultSuggestPath, resp.StatusCode, http.StatusOK, out)
	}
	if out.Suggestion != wantSuggestion {
		t.Fatalf("suggestion = %q, want the fake server's scripted reply %q", out.Suggestion, wantSuggestion)
	}

	// The fake server was genuinely reached: the entitlement gate passed
	// and the request carried the routed vendor model, never the logical
	// "chat:default" feature key the gate judged.
	if aiServer.lastReqBody == nil {
		t.Fatal("the fake OpenAI-compatible server never received a request -- the seeded subscription must pass the entitlement gate")
	}
	if got, _ := aiServer.lastReqBody["model"].(string); got != "gpt-4o-mini" {
		t.Fatalf("fake server saw model = %q, want the routed vendor model \"gpt-4o-mini\"", got)
	}
}

// TestConsultSuggest_CanceledSubscription_RefusedAtEntitlementGate is the
// chat half's refusal path: with tenant-acme's demo subscription canceled
// mid-test through a real SubscriptionService.Cancel on a second
// connection, the SAME request shape the happy path above drove is
// refused with ErrEntitlementDenied -- before the fake chat endpoint is
// ever reached (lastReqBody stays nil), with the coded envelope naming
// the model feature key and the reason.
func TestConsultSuggest_CanceledSubscription_RefusedAtEntitlementGate(t *testing.T) {
	aiServer := newFakeOpenAICompatibleServer(t, "unused")
	srv, cfg := buildConsultTestServer(t, aiServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "ent-chat-refused-owner")

	// A note the request would otherwise summarize successfully -- the
	// refusal below must be about the lapsed subscription, never about a
	// missing or unreadable input.
	noteID := createNoteAs(t, srv, token, "Acme clinical note for the refusal leg.")

	cancelActiveDemoSubscription(t, cfg, "tenant-acme")

	resp := consultSuggestRequest(t, srv, token, noteID)
	assertEntitlementDeniedEnvelope(t, resp, consult.LogicalModel, billing.DecisionReasonNoSubscription)

	if aiServer.lastReqBody != nil {
		t.Fatal("the fake OpenAI-compatible server received a request, want none -- a canceled subscription must refuse before any provider is called")
	}
}

// TestSmileSimulation_SeededSubscriptionPasses_ReachesImageProvider is the
// image half's happy path: tenant-acme's seeded Active subscription clears
// the gate inside Gateway.GenerateImage (image_gateway.go runs
// checkEntitlement synchronously, before anything is enqueued), the
// generation job runs to completion, and the fake image endpoint was
// genuinely reached exactly once.
func TestSmileSimulation_SeededSubscriptionPasses_ReachesImageProvider(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "ent-image-owner")

	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	if completedPhoto.State != "completed" {
		t.Fatalf("photo state = %q, want completed", completedPhoto.State)
	}

	final := smileSimulateAndWait(t, srv, token, completedPhoto, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("final job status = %v, want \"succeeded\": %+v", final["status"], final)
	}

	if imgServer.requests != 1 {
		t.Errorf("fake AI-gateway image endpoint received %d request(s), want exactly 1 -- the seeded subscription must pass the entitlement gate exactly once", imgServer.requests)
	}
	if imgServer.lastModel != "dall-e-3" {
		t.Fatalf("fake server saw model = %q, want the routed vendor model \"dall-e-3\"", imgServer.lastModel)
	}
}

// TestSmileSimulation_CanceledSubscription_RefusedAtEntitlementGate is the
// image half's refusal path. Simulate runs its credit reservation first
// (PreDeduct -- the seeded balance makes it succeed), then
// Gateway.GenerateImage's entitlement gate refuses synchronously, before
// anything is enqueued or any provider is reached. The refusal surfaces
// as the route's ordinary 403 envelope, internal/smilesim refunds the
// just-made reservation, and the fake endpoint's counter stays at zero.
func TestSmileSimulation_CanceledSubscription_RefusedAtEntitlementGate(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "ent-image-refused-owner")

	before := creditBalanceFor(t, cfg, tenantID)
	if before.Available < smilesim.CreditsPerSimulation {
		t.Fatalf("tenant-acme's seeded balance = %+v, want Available >= %d before the reservation this leg makes", before, smilesim.CreditsPerSimulation)
	}

	cancelActiveDemoSubscription(t, cfg, tenantID)

	// A completed photo the simulation would otherwise run on -- the
	// refusal below must be about the lapsed subscription, never about a
	// missing or unreadable input object.
	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	if completedPhoto.State != "completed" {
		t.Fatalf("photo state = %q, want completed", completedPhoto.State)
	}

	simulateBody, err := json.Marshal(map[string]string{"photo_object_id": completedPhoto.ID})
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	assertEntitlementDeniedEnvelope(t, resp, smilesim.LogicalModel, billing.DecisionReasonNoSubscription)

	if imgServer.requests != 0 {
		t.Errorf("fake AI-gateway image endpoint received %d request(s), want 0 -- a canceled subscription must refuse before any provider is called", imgServer.requests)
	}

	// internal/smilesim's own compensation (service.go's Simulate refunds
	// the reservation when GenerateImage refuses) leaves the ledger
	// exactly where it was -- the refused gate must not cost the tenant a
	// credit.
	after := creditBalanceFor(t, cfg, tenantID)
	if after.Available != before.Available || after.Reserved != 0 {
		t.Errorf("balance after a refused generation = %+v, want Available == %d and Reserved == 0 -- the refund must release the reservation in full", after, before.Available)
	}
}
