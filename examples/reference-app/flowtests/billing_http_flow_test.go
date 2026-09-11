package flowtests

// billing_http_flow_test.go drives go/billing's own HTTP surface -- the
// module fragment's two read operations, balance and recent transactions
// (go/billing/api/openapi.yaml), mounted by the module's Register at
// /api/v1/billing and gated by this app's router on billing:credit:read
// -- through the REAL composed HTTP stack, exactly the way
// storage_flow_test.go drives go/storage's and billing_credit_flow_test.go
// drives the service-level credit journey. It is the mandatory-first-
// consumer proof for go/billing's first HTTP surface, and it is where
// four surface regressions are pinned:
//
//   - (a) a caller whose smile simulation consumed credits service-side
//     (internal/smilesim's PreDeduct/Confirm around the real gateway
//     journey) reads back the post-consumption balance AND the ledger
//     rows over the composed stack -- the reads this HTTP surface
//     exists to serve;
//   - (b) the tenant boundary holds over HTTP: each caller reads only
//     their own tenant's balance and rows through the routes, and a
//     caller holding no billing permission is refused by the gate
//     (rbac.permission_denied) rather than served;
//   - (c) a refused or malformed query answers a mapped bilingual code
//     (billing.invalid_limit / billing.invalid_request) in the {code,
//     params} envelope, never a raw error;
//   - (d) the existing billing service-level suite does not regress (it
//     runs unchanged; this file only adds read-back legs on top of the
//     journey billing_credit_flow_test.go already proves).
//
// The refund-observability requirement the fragment was designed for is
// pinned here too: a reservation released back to the balance (a real
// CreditService.Refund, driven service-side the way internal/smilesim's
// settleCredit drives it for a dead-lettered job) is observable through
// the routes as its deduct row's status becoming "refunded" -- a ledger
// row with its reserve/refund classification, never a silent balance
// change.
//
// Wire shapes below are decoded into structs that mirror the JSON on the
// wire, never into the spec-generated types of go/billing's api package,
// following org_flow_test.go's own wire-shape discipline.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// The billing fragment's two wire paths, composed from the same
// BillingRoutePath constant DemoRouteRules' table entry names -- the
// flow tests' anchor on the wire, never a second copy of path truth.
const (
	billingBalancePath       = demo.BillingRoutePath + "/credits/balance"
	billingTransactionsPath  = demo.BillingRoutePath + "/credits/transactions"
	billingCreditTestReserve = "billing-http-flow:reserve-1"
)

// testBillingBalance mirrors the fragment's BillingCreditBalance wire
// shape.
type testBillingBalance struct {
	Available int64  `json:"available"`
	Reserved  int64  `json:"reserved"`
	UpdatedAt string `json:"updatedAt"`
}

// testBillingTransaction mirrors the fragment's BillingCreditTransaction
// wire shape.
type testBillingTransaction struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"createdAt"`
}

// testBillingTransactionPage mirrors the fragment's
// BillingListCreditTransactionsResponse wire shape.
type testBillingTransactionPage struct {
	Transactions []testBillingTransaction `json:"transactions"`
}

// billingRequest issues method against path on srv as the demo actor user
// -- the X-Demo-User value the rbac gate decides the request against
// (DemoOwnerUserID for the owner-role actor every success leg below uses;
// DemoReaderUserID for the gate-closes leg) -- in the tenant the bearer
// token's account belongs to. It is the billing sibling of
// storage_flow_test.go's storageRequest. The caller owns the returned
// response's body.
func billingRequest(t *testing.T, srv *httptest.Server, method, path, token, user string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(demo.DemoUserHeader, user)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (user=%q): %v", method, path, user, err)
	}
	return resp
}

// decodeBillingBalance reads resp, requires its status to be wantStatus,
// and decodes its body as the balance wire shape.
func decodeBillingBalance(t *testing.T, resp *http.Response, wantStatus int, what string) testBillingBalance {
	t.Helper()
	defer resp.Body.Close()

	var out testBillingBalance
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; decoded body = %+v", what, resp.StatusCode, wantStatus, out)
	}
	return out
}

// decodeBillingTransactions reads resp, requires its status to be
// wantStatus, and decodes its body as the transactions-page wire shape.
func decodeBillingTransactions(t *testing.T, resp *http.Response, wantStatus int, what string) testBillingTransactionPage {
	t.Helper()
	defer resp.Body.Close()

	var out testBillingTransactionPage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; decoded body = %+v", what, resp.StatusCode, wantStatus, out)
	}
	return out
}

// assertBillingError reads resp and requires it to be the billing
// surface's structured error: status wantStatus and the envelope's code
// exactly wantCode -- not merely "some 4xx", and never a raw error body.
func assertBillingError(t *testing.T, resp *http.Response, wantStatus int, wantCode, what string) map[string]any {
	t.Helper()
	defer resp.Body.Close()

	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode error body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %+v", what, resp.StatusCode, wantStatus, out)
	}
	if out.Code != wantCode {
		t.Fatalf("%s: error code = %q, want %q; body = %+v", what, out.Code, wantCode, out)
	}
	return out.Params
}

// findBillingTransaction returns the first transaction in page matching
// the predicate, and whether one matched.
func findBillingTransaction(page testBillingTransactionPage, match func(testBillingTransaction) bool) (testBillingTransaction, bool) {
	for _, tx := range page.Transactions {
		if match(tx) {
			return tx, true
		}
	}
	return testBillingTransaction{}, false
}

// TestBillingHttpSurface_SmilesimConsumptionReadsBackOverRoutes is
// regression (a): a caller whose smile simulation consumed credits --
// internal/smilesim's real PreDeduct/Confirm around a real gateway
// journey, driven entirely over the composed HTTP stack -- reads back the
// post-consumption balance and the ledger rows through the billing
// fragment's own routes.
func TestBillingHttpSurface_SmilesimConsumptionReadsBackOverRoutes(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "billing-http-owner")

	// The demo seed at boot granted tenant-acme DemoSimulationCreditGrant
	// credits -- the balance the route must report before anything is
	// spent.
	before := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, token, demo.DemoOwnerUserID),
		http.StatusOK, "balance before simulating")
	if before.Available != demo.DemoSimulationCreditGrant || before.Reserved != 0 {
		t.Fatalf("balance before simulating = %+v, want available %d / reserved 0 (the boot-time demo seed)", before, demo.DemoSimulationCreditGrant)
	}

	// One genuine smile simulation through the composed stack: upload a
	// photo, enqueue, wait for the job to succeed -- the reservation
	// internal/smilesim opened at simulate time is settled by its own
	// Confirm once the job is terminal.
	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	simulateBody := map[string]string{"photo_object_id": completedPhoto.ID}
	simulateJSON, err := json.Marshal(simulateBody)
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateJSON)
	var simulateOut struct {
		JobID string `json:"job_id"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&simulateOut); decodeErr != nil {
		resp.Body.Close()
		t.Fatalf("decode simulate response: %v", decodeErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s status = %d, want %d", smileSimulatePath, resp.StatusCode, http.StatusAccepted)
	}

	final := waitForSmileSimSucceeded(t, srv, token, simulateOut.JobID, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("final job status = %v, want \"succeeded\": %+v", final["status"], final)
	}

	// The post-consumption balance, read back through the billing route:
	// exactly the seed minus CreditsPerSimulation, reserved back at zero
	// (Confirm released the reservation -- it did not leave the credits
	// stranded in the reserved bucket).
	after := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, token, demo.DemoOwnerUserID),
		http.StatusOK, "balance after simulating")
	if want := before.Available - smilesim.CreditsPerSimulation; after.Available != want {
		t.Errorf("balance after a successful generation = %+v, want available %d (seed %d minus CreditsPerSimulation %d)", after, want, before.Available, smilesim.CreditsPerSimulation)
	}
	if after.Reserved != 0 {
		t.Errorf("balance after a CONFIRMED generation has reserved = %d, want 0 -- Confirm must release the reservation", after.Reserved)
	}

	// The ledger rows behind that number, through the transactions route:
	// the boot-time grant (type grant, status confirmed) and the
	// simulation's deduct row (type deduct, status confirmed -- the
	// reservation became a permanent spend), and nothing else.
	page := decodeBillingTransactions(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath, token, demo.DemoOwnerUserID),
		http.StatusOK, "transactions after simulating")
	if len(page.Transactions) != 2 {
		t.Fatalf("transactions carry %d rows, want exactly 2 (seed grant + confirmed deduct); rows = %+v", len(page.Transactions), page.Transactions)
	}
	grant, ok := findBillingTransaction(page, func(tx testBillingTransaction) bool {
		return tx.Type == "grant" && tx.Amount == demo.DemoSimulationCreditGrant
	})
	if !ok {
		t.Errorf("no demo-seed grant row (type grant, amount %d) in %+v", demo.DemoSimulationCreditGrant, page.Transactions)
	} else if grant.Status != "confirmed" || grant.Reason != demo.DemoCreditGrantReason {
		t.Errorf("grant row = %+v, want status confirmed and reason %q", grant, demo.DemoCreditGrantReason)
	}
	deduct, ok := findBillingTransaction(page, func(tx testBillingTransaction) bool {
		return tx.Type == "deduct" && tx.Amount == smilesim.CreditsPerSimulation
	})
	if !ok {
		t.Fatalf("no simulation deduct row (type deduct, amount %d) in %+v", smilesim.CreditsPerSimulation, page.Transactions)
	}
	if deduct.Status != "confirmed" {
		t.Errorf("simulation deduct row status = %q, want %q -- the reservation was settled as a permanent spend", deduct.Status, "confirmed")
	}
	if !strings.HasPrefix(deduct.Reason, "smilesim:") {
		t.Errorf("simulation deduct row reason = %q, want a smilesim:-prefixed reservation note", deduct.Reason)
	}
	if deduct.ID == "" || deduct.CreatedAt == "" {
		t.Errorf("simulation deduct row = %+v, want a populated id and createdAt", deduct)
	}
}

// TestBillingHttpSurface_TenantBoundaryHoldsOverRoutes is regression
// (b): through the routes, each caller reads only their own tenant's
// balance and ledger rows -- tenant-acme's spend is invisible to
// tenant-globex, whose own seeded balance reads back unchanged -- and a
// caller holding no billing permission (the demo reader, whose role
// carries notes:read alone) is refused by the router gate with
// rbac.permission_denied rather than served. The reads themselves go
// through the module's tenant-filtered services; this test proves the
// boundary holds end to end, over HTTP, the AssertIsolated discipline
// exercised through the routes.
func TestBillingHttpSurface_TenantBoundaryHoldsOverRoutes(t *testing.T) {
	srv, cfg := buildSmileSimTestServer(t, newFakeOpenAIImageServer(t))

	const acme pkgcore.TenantID = "tenant-acme"
	const globex pkgcore.TenantID = "tenant-globex"
	acmeToken := registerAndAuthenticate(t, srv, cfg, acme, "billing-boundary-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, globex, "billing-boundary-globex")

	// Both demo tenants were seeded at boot with the same grant -- the
	// identical starting number that makes the later divergence meaningful.
	for _, tc := range []struct {
		tenant pkgcore.TenantID
		token  string
	}{
		{acme, acmeToken},
		{globex, globexToken},
	} {
		bal := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, tc.token, demo.DemoOwnerUserID),
			http.StatusOK, "seeded balance of "+string(tc.tenant))
		if bal.Available != demo.DemoSimulationCreditGrant || bal.Reserved != 0 {
			t.Fatalf("%s's seeded balance = %+v, want available %d / reserved 0", tc.tenant, bal, demo.DemoSimulationCreditGrant)
		}
	}

	// tenant-acme spends 300 of its own credits through a real
	// CreditService.Expire on a second connection (the same reach
	// billing_credit_flow_test.go's creditBalanceFor uses) -- a real
	// ledger operation under acme's own context, never a row hand-edited.
	credits := openBillingCredits(t, cfg)
	if _, err := credits.Expire(pkgcore.WithTenant(t.Context(), acme), billing.PreDeductInput{
		Amount: 300, Reason: "test:drain-acme",
	}); err != nil {
		t.Fatalf("Expire (drain tenant-acme): %v", err)
	}

	// acme's own route read now reports the drained balance; globex's
	// still reports the untouched seed -- a caller can only ever see its
	// own tenant's credits.
	acmeBal := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, acmeToken, demo.DemoOwnerUserID),
		http.StatusOK, "balance of tenant-acme after draining")
	if want := demo.DemoSimulationCreditGrant - 300; acmeBal.Available != want {
		t.Errorf("tenant-acme balance after draining = available %d, want %d", acmeBal.Available, want)
	}
	globexBal := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, globexToken, demo.DemoOwnerUserID),
		http.StatusOK, "balance of tenant-globex after tenant-acme spent")
	if globexBal.Available != demo.DemoSimulationCreditGrant {
		t.Errorf("tenant-globex balance = available %d, want the untouched seed %d -- another tenant's spend must be invisible", globexBal.Available, demo.DemoSimulationCreditGrant)
	}

	// The ledger boundary holds the same way: acme's transactions carry
	// its own expire row; globex's carry only its own single seed grant,
	// with no trace of acme's spend.
	acmePage := decodeBillingTransactions(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath, acmeToken, demo.DemoOwnerUserID),
		http.StatusOK, "transactions of tenant-acme")
	if _, ok := findBillingTransaction(acmePage, func(tx testBillingTransaction) bool {
		return tx.Type == "expire" && tx.Amount == 300 && tx.Reason == "test:drain-acme"
	}); !ok {
		t.Errorf("tenant-acme's transactions lack its own expire row; rows = %+v", acmePage.Transactions)
	}
	globexPage := decodeBillingTransactions(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath, globexToken, demo.DemoOwnerUserID),
		http.StatusOK, "transactions of tenant-globex")
	if len(globexPage.Transactions) != 1 {
		t.Fatalf("tenant-globex's transactions carry %d rows, want exactly its own seed grant; rows = %+v", len(globexPage.Transactions), globexPage.Transactions)
	}
	if row := globexPage.Transactions[0]; row.Type != "grant" || row.Amount != demo.DemoSimulationCreditGrant {
		t.Errorf("tenant-globex's only row = %+v, want its own seed grant", row)
	}

	// The gate closes on a caller without the permission: the demo reader
	// holds notes:read and nothing else, so the same balance read answers
	// the coded rbac refusal -- deny by default holds over HTTP too.
	assertBillingError(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, acmeToken, demo.DemoReaderUserID),
		http.StatusForbidden, "rbac.permission_denied", "balance read as the demo reader")
}

// TestBillingHttpSurface_RefundIsALedgerRowNotASilentChange pins the
// property the fragment's read surface exists to serve: a reservation
// released back to the balance -- driven service-side through a real
// CreditService.Refund, exactly the way internal/smilesim's settleCredit
// releases a dead-lettered generation's reservation -- is observable
// through the routes as that deduct row's status becoming "refunded".
// The balance moves 990 -> 1000 over the route reads, and the ledger row
// behind the movement is right there in the transactions list: the
// refund is never a silent balance change.
func TestBillingHttpSurface_RefundIsALedgerRowNotASilentChange(t *testing.T) {
	srv, cfg := buildSmileSimTestServer(t, newFakeOpenAIImageServer(t))

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "billing-refund-owner")

	// Reserve 10 credits service-side through a real CreditService call
	// on a second connection, under the tenant's own context -- the shape
	// internal/smilesim's Simulate performs for every generation.
	credits := openBillingCredits(t, cfg)
	tenantCtx := pkgcore.WithTenant(t.Context(), tenantID)
	if _, err := credits.PreDeduct(tenantCtx, billing.PreDeductInput{
		Amount:         10,
		IdempotencyKey: billingCreditTestReserve,
		Reason:         "test:reserve",
	}); err != nil {
		t.Fatalf("PreDeduct: %v", err)
	}

	// While the reservation is outstanding, the routes show the reserve:
	// 10 credits moved from available into reserved, and a pending deduct
	// row at the head of the ledger.
	reserved := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, token, demo.DemoOwnerUserID),
		http.StatusOK, "balance while reserved")
	if reserved.Available != demo.DemoSimulationCreditGrant-10 || reserved.Reserved != 10 {
		t.Errorf("balance while reserved = %+v, want available %d / reserved 10", reserved, demo.DemoSimulationCreditGrant-10)
	}
	pendingPage := decodeBillingTransactions(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath, token, demo.DemoOwnerUserID),
		http.StatusOK, "transactions while reserved")
	pendingRow, ok := findBillingTransaction(pendingPage, func(tx testBillingTransaction) bool { return tx.ID == billingCreditTestReserve })
	if !ok {
		t.Fatalf("no pending row for %q in %+v", billingCreditTestReserve, pendingPage.Transactions)
	}
	if pendingRow.Type != "deduct" || pendingRow.Status != "pending" || pendingRow.Amount != 10 {
		t.Errorf("pending row = %+v, want type deduct / status pending / amount 10", pendingRow)
	}

	// The reservation fails and is released back -- CreditService.Refund
	// on the same idempotency key, the settlement a dead-lettered smile
	// job receives.
	if _, err := credits.Refund(tenantCtx, billingCreditTestReserve); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	// The balance is back to the full seed, and the SAME ledger row now
	// reports status "refunded" -- the refund is observable as the row's
	// classification, never as a row that vanished and never as a change
	// with no ledger trace.
	restored := decodeBillingBalance(t, billingRequest(t, srv, http.MethodGet, billingBalancePath, token, demo.DemoOwnerUserID),
		http.StatusOK, "balance after refund")
	if restored.Available != demo.DemoSimulationCreditGrant || restored.Reserved != 0 {
		t.Errorf("balance after refund = %+v, want the full seed back (available %d / reserved 0)", restored, demo.DemoSimulationCreditGrant)
	}
	refundPage := decodeBillingTransactions(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath, token, demo.DemoOwnerUserID),
		http.StatusOK, "transactions after refund")
	refundedRow, ok := findBillingTransaction(refundPage, func(tx testBillingTransaction) bool { return tx.ID == billingCreditTestReserve })
	if !ok {
		t.Fatalf("no row for %q after the refund in %+v", billingCreditTestReserve, refundPage.Transactions)
	}
	if refundedRow.Status != "refunded" {
		t.Errorf("refunded row status = %q, want %q -- the refund must be visible as the deduct row's refunded classification", refundedRow.Status, "refunded")
	}
	if refundedRow.Amount != 10 || refundedRow.Reason != "test:reserve" {
		t.Errorf("refunded row = %+v, want amount 10 and the reservation's own reason intact", refundedRow)
	}
}

// TestBillingHttpSurface_RefusalsAreMappedCodesNotRawErrors is regression
// (c): a refused or malformed query answers a mapped bilingual code in
// the {code, params} envelope, never a raw error. The range half -- a
// limit outside the fragment's 1-100 bound -- is refused by the handler
// with billing.invalid_limit carrying the structured bound; the parse
// half -- a limit that is not an integer at all -- is refused by the
// spec-generated parameter binder with billing.invalid_request carrying
// the failing parameter name, the coded envelope that replaces
// oapi-codegen's default plain-text http.Error.
func TestBillingHttpSurface_RefusalsAreMappedCodesNotRawErrors(t *testing.T) {
	srv, cfg := buildSmileSimTestServer(t, newFakeOpenAIImageServer(t))

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "billing-refusals-owner")

	params := assertBillingError(t, billingRequest(t, srv, http.MethodGet, billingTransactionsPath+"?limit=0", token, demo.DemoOwnerUserID),
		http.StatusBadRequest, "billing.invalid_limit", "transactions with limit=0")
	for _, key := range []string{"limit", "min", "max"} {
		if _, ok := params[key]; !ok {
			t.Errorf("limit=0 envelope params lack %q; params = %v", key, params)
		}
	}
	if _, ok := params["max"]; ok {
		if params["max"] != float64(100) {
			t.Errorf("limit=0 envelope max = %v, want 100", params["max"])
		}
	}

	// The malformed half must answer the JSON envelope -- never
	// oapi-codegen's default plain-text body with the binder's raw parse
	// message in it.
	resp := billingRequest(t, srv, http.MethodGet, billingTransactionsPath+"?limit=abc", token, demo.DemoOwnerUserID)
	malformedParams := assertBillingError(t, resp, http.StatusBadRequest, "billing.invalid_request", "transactions with limit=abc")
	if malformedParams["parameter"] != "limit" {
		t.Errorf("limit=abc envelope params = %v, want the failing parameter named (\"limit\")", malformedParams)
	}
}
