package flowtests

// usage_summary_flow_test.go is the product-directive-C1 proof that
// admin's D9 usage/billing dashboard (go/admin/usage.go, docs/internal/
// 23-admin.md) is genuinely wired into this app: GET
// /api/v1/admin/usage-summary answers 200 with REAL go/metering data for
// the system-domain operator. Before this round's wiring it answered 500
// with the coded body admin.usage_modules_not_wired
// (go/admin/errors.go's ErrUsageModulesNotWired) -- BuildServer passed
// adminModule neither admin.WithMetering nor admin.WithBilling (that
// refusal and its fail-before state are the subject of
// TestAdminFlow_Round2Routes_ReachableForPlatformStaff's D9 leg).
//
// The usage the endpoint reads is recorded by the REAL recording path, not
// a test-only injection: a genuine consultation-suggestion call (the
// consult_flow_test.go shape) travels through go/ai-gateway's
// Gateway.Chat, whose wired UsageRecorder seam reports the call's token
// usage onto meteringModule.Recorder() -- the analytics-grade (fail-open)
// tier, the only one the seam's Record(ctx, event) shape can carry (the
// billing-grade Enqueue needs the caller's own transaction). Recorder
// events are buffered on a channel and folded by the module's background
// flush loop into real metering_usage_summaries rows through
// Aggregator.Ingest -- asynchronous by construction, so the test polls the
// endpoint until the recorded row is visible rather than guessing a sleep,
// and a wiring that never started the flush loop fails the poll honestly.
// D9's dashboard then reads those rows back per tenant in admin's D3
// ledger, under D2's tenancy.WithSystemContext mechanism.
//
// Billing is wired through admin.WithBilling too, so every row carries
// creditBalance/activeSubscription as well. With both modules wired there
// is no unwired dimension left for a composed run to show absent; that
// half of D9's contract -- a wired module's dimension present on every
// row, an unwired one's fields absent (nil), never a refusal -- stays
// unit-pinned in go/admin/usage_test.go, which this file's assertions
// deliberately do not duplicate.
//
// The system-domain operator driving the endpoint is the seeded
// demo-platform-staff account (internal/app/demo_admin.go), the one account this app
// grants admin:* permissions to under rbac.SystemDomain; demo-owner is
// deliberately a tenant-domain owner whose admin access is pinned 403 by
// TestAdminFlow_OrdinaryTenantOwner_CannotAccessAdminConsole.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// The wire shapes this suite decodes, field-named after admin's own
// generated JSON (go/admin/api/openapi.yaml's AdminUsageSummaryResponse)
// rather than importing its generated types -- the same posture every
// other flow test in this package takes toward the module it drives.
type (
	adminUsageSummaryResponse struct {
		Rows []adminUsageSummaryRow `json:"rows"`
	}
	adminUsageSummaryRow struct {
		TenantID           string                      `json:"tenantId"`
		DisplayName        string                      `json:"displayName"`
		MeteringSummaries  *[]adminUsageFeatureSummary `json:"meteringSummaries"`
		CreditBalance      *adminUsageCreditBalance    `json:"creditBalance"`
		ActiveSubscription *adminUsageSubscription     `json:"activeSubscription"`
	}
	adminUsageFeatureSummary struct {
		Feature     string  `json:"feature"`
		PeriodStart string  `json:"periodStart"`
		PeriodEnd   string  `json:"periodEnd"`
		Quantity    float64 `json:"quantity"`
	}
	adminUsageCreditBalance struct {
		Available int `json:"available"`
		Reserved  int `json:"reserved"`
	}
	adminUsageSubscription struct {
		ID     string `json:"id"`
		PlanID string `json:"planId"`
		Status string `json:"status"`
	}
)

// buildUsageSummaryTestServer composes buildAdminTestServer's real output
// (demo accounts, the demo platform-staff account, a capturing mailer)
// with the ai-gateway platform credential pointed at aiServer -- the same
// test-only-override shape buildConsultTestServer uses, so every
// consult/smilesim AI call this suite drives records usage through the
// real gateway onto the real wired metering module.
func buildUsageSummaryTestServer(t *testing.T, aiServer *fakeOpenAICompatibleServer) (*httptest.Server, app.ServerConfig) {
	t.Helper()

	srv, cfg, _ := buildAdminTestServer(t, func(c *app.ServerConfig) {
		c.AIGatewayBaseURL = aiServer.URL
		c.AIGatewayAPIKey = "sk-test-usage-key"
	})
	return srv, cfg
}

// waitForUsageSummaryRow polls GET /api/v1/admin/usage-summary as
// staffToken until some ledger tenant's row carries a metering summary for
// wantFeature, returning that tenant's row. Each poll decodes a real 200
// response; the deadline failure prints the last full answer. The poll is
// what makes the test honest about the fold's real timing: the recorder
// flush loop delivers a recorded event promptly but asynchronously, and
// the summary row exists only once Aggregator.Ingest has actually folded
// it -- this helper waits for exactly that, never for an arbitrary sleep
// that could pass before the fold or fail after it.
func waitForUsageSummaryRow(t *testing.T, srv *httptest.Server, staffToken, wantFeature string) adminUsageSummaryRow {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last adminUsageSummaryResponse
	for time.Now().Before(deadline) {
		last = adminUsageSummaryResponse{}
		adminRequest(t, srv, http.MethodGet, app.AdminUsageSummaryPath, staffToken, nil, http.StatusOK, &last, nil)
		for _, row := range last.Rows {
			if row.MeteringSummaries == nil {
				continue
			}
			for _, s := range *row.MeteringSummaries {
				if s.Feature == wantFeature {
					return row
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	lastJSON, _ := json.Marshal(last)
	t.Fatalf("no ledger tenant's usage-summary row showed feature %q within %v; last answer = %s",
		wantFeature, 10*time.Second, lastJSON)
	return adminUsageSummaryRow{}
}

// TestUsageSummary_D9Endpoint_RealRecordedUsage is the C1 acceptance
// proof: a real AI-shaped call records real usage through the wired
// gateway seam, and the system-domain operator's GET /api/v1/admin/
// usage-summary answers 200 with that usage present -- the answer this
// endpoint could not give before the wiring (500 ErrUsageModulesNotWired,
// see the test file's doc comment). It also pins the dimensions the wiring
// added: meteringSummaries present on every ledger row (empty for a tenant
// with no recorded usage, never absent), and the billing dimensions
// (creditBalance, activeSubscription) present because seedDemoCredits and
// seedDemoEntitlements gave the demo tenants real rows.
func TestUsageSummary_D9Endpoint_RealRecordedUsage(t *testing.T) {
	const wantSuggestion = "Monitor the sensitivity and consider a desensitizing agent."
	aiServer := newFakeOpenAICompatibleServer(t, wantSuggestion)
	srv, cfg := buildUsageSummaryTestServer(t, aiServer)
	staffToken := platformStaffToken(t, srv)

	// One real consult call under tenant-acme: create a note, ask for a
	// suggestion. The composed request answers 200 only if the whole
	// gateway chain (credential, routing, entitlement gate) worked, and
	// the fake server's recorded usage totals are the exact quantity this
	// test later expects to find folded into the metering summary.
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "usage-summary-owner")
	const noteText = "Patient reports occasional sharp pain when consuming cold beverages."
	noteID := createNoteAs(t, srv, token, noteText)
	resp := consultSuggestRequest(t, srv, token, noteID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("consult suggest status = %d, want 200; body = %s", resp.StatusCode, respBody)
	}
	if aiServer.lastReqBody == nil {
		t.Fatal("fake OpenAI-compatible server never received the chat request")
	}

	// The D9 read: 200 (never 500 admin.usage_modules_not_wired), with the
	// recorded call's feature present on the recording tenant's row. The
	// fake's wire usage reported total_tokens = 20, which is exactly what
	// Gateway records under ai.chat_tokens -- matching quantity ties the
	// summary row back to the real call above, not to any other source.
	row := waitForUsageSummaryRow(t, srv, staffToken, "ai.chat_tokens")
	if row.TenantID != "tenant-acme" {
		t.Fatalf("row carrying ai.chat_tokens names tenant %q, want tenant-acme", row.TenantID)
	}
	var recordedQuantity float64
	for _, s := range *row.MeteringSummaries {
		if s.Feature == "ai.chat_tokens" {
			recordedQuantity += s.Quantity
		}
	}
	if recordedQuantity != 20 {
		t.Fatalf("metering summary for tenant-acme records ai.chat_tokens quantity %v, want 20 (the fake call's usage.total_tokens)", recordedQuantity)
	}

	// The full dashboard answers once more for the composed-shape
	// assertions: every ledger tenant carries the wired dimensions, and
	// the tenant with no recorded usage shows an empty-but-present
	// meteringSummaries rather than an absent field or an error.
	final := adminUsageSummaryResponse{}
	adminRequest(t, srv, http.MethodGet, app.AdminUsageSummaryPath, staffToken, nil, http.StatusOK, &final, nil)
	rowsByTenant := map[string]adminUsageSummaryRow{}
	for _, r := range final.Rows {
		rowsByTenant[r.TenantID] = r
	}
	acme, ok := rowsByTenant["tenant-acme"]
	if !ok {
		t.Fatalf("usage summary rows = %+v, want a tenant-acme row", final.Rows)
	}
	if acme.CreditBalance == nil {
		t.Error("tenant-acme row has no creditBalance, want non-nil since go/billing is wired (seedDemoCredits seeded a real balance)")
	}
	if acme.ActiveSubscription == nil {
		t.Error("tenant-acme row has no activeSubscription, want non-nil since seedDemoEntitlements subscribed the demo tenants")
	}
	globex, ok := rowsByTenant["tenant-globex"]
	if !ok {
		t.Fatalf("usage summary rows = %+v, want a tenant-globex row", final.Rows)
	}
	if globex.MeteringSummaries == nil {
		t.Error("tenant-globex row has no meteringSummaries field, want a non-nil (possibly empty) list since go/metering is wired -- go/admin/usage_test.go pins the same contract at the unit level")
	} else if len(*globex.MeteringSummaries) != 0 {
		t.Errorf("tenant-globex meteringSummaries = %+v, want empty -- no AI call ran under that tenant", *globex.MeteringSummaries)
	}
	if globex.CreditBalance == nil {
		t.Error("tenant-globex row has no creditBalance, want non-nil since go/billing is wired")
	}
}
