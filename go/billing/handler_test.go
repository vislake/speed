package billing

// handler_test.go drives the module's HTTP surface -- the Handler built
// from api/openapi.yaml's four read operations, the credits pair
// (balance, recent transactions) and the invoice pair (list, get) -- over
// a real migrated SQLite database, with the tenant injected into the
// request context the way tenancy.Middleware would have resolved it in a
// composed host. It is the unit half of this round's proof; the
// composed-stack half lives in
// examples/reference-app/cmd/server/billing_http_flow_test.go.
//
// The assertions bind to the wire contract the spec promises -- status
// codes, the {code, params} envelope, camelCase JSON field names -- never
// to implementation internals, mirroring go/storage/handler_test.go's own
// shape for its handler.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing/api"
)

// newHandlerForTest returns a Handler over a fresh migrated SQLite
// database whose CreditService and InvoiceRepository are fully wired for
// direct calls -- so a test can seed state service-side (Grant/PreDeduct/
// Refund/Expire and CreateInvoice/MarkPaid/Void under a tenant context,
// each repository over the same database the Handler reads) and then read
// it back through the HTTP surface, the same two-layer shape the composed
// flow tests use.
func newHandlerForTest(t *testing.T) (*Handler, *CreditService, *InvoiceRepository) {
	t.Helper()
	db := newTestDB(t)
	credits := NewCreditService(db)
	invoices := NewInvoiceRepository(db)
	return NewHandler(credits, invoices), credits, invoices
}

// billingRequest issues method against target on h as tenant, with the
// tenant carried in the request context exactly where tenancy.Middleware
// would have put it in a composed host, and returns the recorder.
func billingRequest(t *testing.T, h *Handler, tenant pkgcore.TenantID, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req = req.WithContext(pkgcore.WithTenant(req.Context(), tenant))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBalance decodes rec as the balance wire shape, requiring
// wantStatus.
func decodeBalance(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.BillingCreditBalance {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("balance status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out api.BillingCreditBalance
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode balance body %s: %v", rec.Body.String(), err)
	}
	return out
}

// decodeTransactions decodes rec as the transactions-list wire shape,
// requiring wantStatus.
func decodeTransactions(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.BillingListCreditTransactionsResponse {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("transactions status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out api.BillingListCreditTransactionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode transactions body %s: %v", rec.Body.String(), err)
	}
	return out
}

// decodeEnvelope decodes rec as the {code, params} error envelope,
// requiring wantStatus, and returns the code and params so a caller can
// assert on the exact mapped code rather than merely "some 4xx".
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.BillingError {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("error status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out api.BillingError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode envelope body %s: %v", rec.Body.String(), err)
	}
	if out.Code == "" {
		t.Fatalf("envelope body %s carries no code", rec.Body.String())
	}
	return out
}

// TestHandler_GetCreditBalance_AnswersTheTenantsOwnNumber grants two
// tenants different balances service-side and reads each back through the
// route: the number a credit view renders is the caller's own tenant's,
// never a cross-tenant sum, and a tenant the seed never touched answers
// an all-zero balance rather than an error -- the zero state a view can
// render, not a missing-resource refusal.
func TestHandler_GetCreditBalance_AnswersTheTenantsOwnNumber(t *testing.T) {
	h, credits, _ := newHandlerForTest(t)
	const tenantA pkgcore.TenantID = "tenant-a"
	const tenantB pkgcore.TenantID = "tenant-b"

	ctxA := pkgcore.WithTenant(t.Context(), tenantA)
	ctxB := pkgcore.WithTenant(t.Context(), tenantB)
	if _, err := credits.Grant(ctxA, GrantInput{Amount: 1000, Reason: "demo:seed"}); err != nil {
		t.Fatalf("grant tenant-a: %v", err)
	}
	if _, err := credits.Grant(ctxB, GrantInput{Amount: 7, Reason: "demo:seed"}); err != nil {
		t.Fatalf("grant tenant-b: %v", err)
	}

	balA := decodeBalance(t, billingRequest(t, h, tenantA, http.MethodGet, apiPath+"/credits/balance"), http.StatusOK)
	if balA.Available != 1000 || balA.Reserved != 0 {
		t.Errorf("tenant-a balance = available %d / reserved %d, want 1000 / 0", balA.Available, balA.Reserved)
	}
	if balA.UpdatedAt.IsZero() {
		t.Error("tenant-a balance updatedAt is zero, want the row's last-touch time")
	}

	balB := decodeBalance(t, billingRequest(t, h, tenantB, http.MethodGet, apiPath+"/credits/balance"), http.StatusOK)
	if balB.Available != 7 || balB.Reserved != 0 {
		t.Errorf("tenant-b balance = available %d / reserved %d, want 7 / 0 -- a route read must never answer another tenant's credits", balB.Available, balB.Reserved)
	}

	balC := decodeBalance(t, billingRequest(t, h, pkgcore.TenantID("tenant-c"), http.MethodGet, apiPath+"/credits/balance"), http.StatusOK)
	if balC.Available != 0 || balC.Reserved != 0 {
		t.Errorf("never-touched tenant balance = available %d / reserved %d, want 0 / 0 (materialized zero, never a 404)", balC.Available, balC.Reserved)
	}
}

// TestHandler_ListCreditTransactions_ClassifiesTheLedgerRows grants,
// reserves, confirms and refunds under one tenant service-side, then reads
// the ledger back through the route and asserts the wire classification --
// the reserve/confirm/refund vocabulary the credit view renders, and the
// proof that a refund is observable as its deduct row's status having
// become "refunded", never as a row that vanishes or a balance-only
// change -- plus the newest-first ordering of the recent window.
func TestHandler_ListCreditTransactions_ClassifiesTheLedgerRows(t *testing.T) {
	h, credits, _ := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"
	ctx := pkgcore.WithTenant(t.Context(), tenant)

	if _, err := credits.Grant(ctx, GrantInput{Amount: 1000, Reason: "demo:seed"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// reserve-a: a reservation settled as a permanent spend (Confirm).
	if _, err := credits.PreDeduct(ctx, PreDeductInput{Amount: 10, IdempotencyKey: "reserve-a", Reason: "smilesim:job-1"}); err != nil {
		t.Fatalf("pre-deduct reserve-a: %v", err)
	}
	if _, err := credits.Confirm(ctx, "reserve-a"); err != nil {
		t.Fatalf("confirm reserve-a: %v", err)
	}
	// reserve-b: a reservation released back to the balance (Refund) --
	// the newest row, so the classification assertions below pin that a
	// refund sits visibly at the head of the ledger.
	if _, err := credits.PreDeduct(ctx, PreDeductInput{Amount: 5, IdempotencyKey: "reserve-b", Reason: "smilesim:job-2"}); err != nil {
		t.Fatalf("pre-deduct reserve-b: %v", err)
	}
	if _, err := credits.Refund(ctx, "reserve-b"); err != nil {
		t.Fatalf("refund reserve-b: %v", err)
	}

	page := decodeTransactions(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/transactions"), http.StatusOK)

	wantTypes := []api.BillingCreditTransactionType{api.Deduct, api.Deduct, api.Grant}
	wantStatuses := []api.BillingCreditTransactionStatus{api.Refunded, api.Confirmed, api.Confirmed}
	wantAmounts := []int64{5, 10, 1000}
	wantIDs := []string{"reserve-b", "reserve-a", ""}
	if len(page.Transactions) != len(wantTypes) {
		t.Fatalf("transactions carry %d rows, want %d; body = %+v", len(page.Transactions), len(wantTypes), page.Transactions)
	}
	for i, row := range page.Transactions {
		if row.Type != wantTypes[i] {
			t.Errorf("row %d type = %q, want %q", i, row.Type, wantTypes[i])
		}
		if row.Status != wantStatuses[i] {
			t.Errorf("row %d status = %q, want %q -- the refund must be observable as the deduct row's refunded status", i, row.Status, wantStatuses[i])
		}
		if row.Amount != wantAmounts[i] {
			t.Errorf("row %d amount = %d, want %d", i, row.Amount, wantAmounts[i])
		}
		if wantIDs[i] != "" && row.ID != wantIDs[i] {
			t.Errorf("row %d id = %q, want %q (a deduct row's id is its idempotency key)", i, row.ID, wantIDs[i])
		}
		if row.CreatedAt.IsZero() {
			t.Errorf("row %d createdAt is zero", i)
		}
	}
	// The refunded row keeps its reason from reservation time: the ledger
	// entry that named the reservation is the same entry that reports the
	// refund.
	if row := page.Transactions[0]; row.Reason != "smilesim:job-2" {
		t.Errorf("refunded row's reason = %q, want %q", row.Reason, "smilesim:job-2")
	}
	// And the balance route agrees with the classification: only the
	// confirmed reservation's 10 credits are gone; the refunded 5 were
	// never permanently spent.
	bal := decodeBalance(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/balance"), http.StatusOK)
	if bal.Available != 990 || bal.Reserved != 0 {
		t.Errorf("balance after one confirmed and one refunded reservation = available %d / reserved %d, want 990 / 0", bal.Available, bal.Reserved)
	}
}

// TestHandler_ListCreditTransactions_NeverLeaksAnotherTenantsLedger
// writes ledger rows for two tenants and reads each listing back through
// the route: a caller only ever sees its own tenant's rows, and a tenant
// with no rows answers an empty array -- never null, and never another
// tenant's ledger.
func TestHandler_ListCreditTransactions_NeverLeaksAnotherTenantsLedger(t *testing.T) {
	h, credits, _ := newHandlerForTest(t)
	const tenantA pkgcore.TenantID = "tenant-a"
	const tenantB pkgcore.TenantID = "tenant-b"

	if _, err := credits.Grant(pkgcore.WithTenant(t.Context(), tenantA), GrantInput{Amount: 1000, Reason: "demo:seed"}); err != nil {
		t.Fatalf("grant tenant-a: %v", err)
	}
	// tenant-b needs its own balance before it can reserve anything -- a
	// real tenant's ledger starts with its own top-up, never another
	// tenant's row.
	if _, err := credits.Grant(pkgcore.WithTenant(t.Context(), tenantB), GrantInput{Amount: 5, Reason: "test:b-seed"}); err != nil {
		t.Fatalf("grant tenant-b: %v", err)
	}
	if _, err := credits.PreDeduct(pkgcore.WithTenant(t.Context(), tenantB), PreDeductInput{Amount: 3, IdempotencyKey: "tenant-b-key", Reason: "test:b"}); err != nil {
		t.Fatalf("pre-deduct tenant-b: %v", err)
	}

	pageA := decodeTransactions(t, billingRequest(t, h, tenantA, http.MethodGet, apiPath+"/credits/transactions"), http.StatusOK)
	if len(pageA.Transactions) != 1 {
		t.Fatalf("tenant-a listing carries %d rows, want exactly its own 1", len(pageA.Transactions))
	}
	if row := pageA.Transactions[0]; row.Type != api.Grant || row.Amount != 1000 {
		t.Errorf("tenant-a's only row = %+v, want its own grant row", row)
	}

	pageB := decodeTransactions(t, billingRequest(t, h, tenantB, http.MethodGet, apiPath+"/credits/transactions"), http.StatusOK)
	if len(pageB.Transactions) != 2 {
		t.Fatalf("tenant-b listing carries %d rows, want exactly its own 2 (never tenant-a's grant)", len(pageB.Transactions))
	}
	if row := pageB.Transactions[0]; row.Type != api.Deduct || row.ID != "tenant-b-key" {
		t.Errorf("tenant-b's newest row = %+v, want its own pending deduct row", row)
	}
	if row := pageB.Transactions[1]; row.Type != api.Grant || row.Amount != 5 {
		t.Errorf("tenant-b's second row = %+v, want its own seed grant row", row)
	}

	pageC := decodeTransactions(t, billingRequest(t, h, pkgcore.TenantID("tenant-c"), http.MethodGet, apiPath+"/credits/transactions"), http.StatusOK)
	if pageC.Transactions == nil {
		t.Fatal("never-touched tenant's transactions is null, want the empty array the schema promises")
	}
	if len(pageC.Transactions) != 0 {
		t.Errorf("never-touched tenant's listing carries %d rows, want 0", len(pageC.Transactions))
	}
}

// TestHandler_ListCreditTransactions_EnforcesTheLimitBound tests the
// limit parameter's contract: it narrows the newest-first window to the
// requested size, defaults to defaultCreditListPageSize when absent, and
// refuses an integer outside the fragment's 1-100 bound with the mapped
// billing.invalid_limit envelope carrying the structured bound -- never a
// raw error.
func TestHandler_ListCreditTransactions_EnforcesTheLimitBound(t *testing.T) {
	h, credits, _ := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"
	ctx := pkgcore.WithTenant(t.Context(), tenant)

	for _, amount := range []int64{100, 200, 300} {
		if _, err := credits.Grant(ctx, GrantInput{Amount: amount, Reason: "test:topup"}); err != nil {
			t.Fatalf("grant %d: %v", amount, err)
		}
	}

	// limit=2 narrows the newest-first window to the two newest rows.
	page := decodeTransactions(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/transactions?limit=2"), http.StatusOK)
	if len(page.Transactions) != 2 {
		t.Fatalf("limit=2 returned %d rows, want 2", len(page.Transactions))
	}
	if page.Transactions[0].Amount != 300 || page.Transactions[1].Amount != 200 {
		t.Errorf("limit=2 rows (newest first) = amounts %d, %d, want 300, 200", page.Transactions[0].Amount, page.Transactions[1].Amount)
	}

	// Absent limit defaults to the full recent window for this small
	// ledger (the default page size comfortably covers it).
	all := decodeTransactions(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/transactions"), http.StatusOK)
	if len(all.Transactions) != 3 {
		t.Errorf("absent limit returned %d rows, want all 3", len(all.Transactions))
	}

	for _, limit := range []string{"0", "101"} {
		rec := billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/transactions?limit="+limit)
		env := decodeEnvelope(t, rec, http.StatusBadRequest)
		if env.Code != "billing.invalid_limit" {
			t.Errorf("limit=%s code = %q, want %q", limit, env.Code, "billing.invalid_limit")
		}
		if env.Params == nil {
			t.Fatalf("limit=%s envelope carries no params, want the structured bound", limit)
		}
		for _, key := range []string{"limit", "min", "max"} {
			if _, ok := (*env.Params)[key]; !ok {
				t.Errorf("limit=%s envelope params lack %q; params = %v", limit, key, *env.Params)
			}
		}
	}
}

// TestHandler_ListCreditTransactions_MalformedLimit_AnswersTheCodedEnvelope
// is the malformed-query half of this round's "never a raw error"
// contract: a limit that is not an integer is refused by the
// spec-generated parameter binder BEFORE the handler runs, and the
// bindingErrorHandler NewHandler installs must answer the module's own
// billing.invalid_request envelope -- never oapi-codegen's default
// plain-text http.Error body with the binder's raw message in it.
func TestHandler_ListCreditTransactions_MalformedLimit_AnswersTheCodedEnvelope(t *testing.T) {
	h, _, _ := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"

	rec := billingRequest(t, h, tenant, http.MethodGet, apiPath+"/credits/transactions?limit=abc")
	env := decodeEnvelope(t, rec, http.StatusBadRequest)
	if env.Code != "billing.invalid_request" {
		t.Errorf("malformed-limit code = %q, want %q", env.Code, "billing.invalid_request")
	}
	if env.Params == nil {
		t.Fatal("malformed-limit envelope carries no params, want the failing parameter named")
	}
	if got, ok := (*env.Params)["parameter"]; !ok || got != "limit" {
		t.Errorf("malformed-limit envelope params = %v, want parameter = \"limit\"", *env.Params)
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("malformed-limit Content-Type = %q, want %q -- the answer must be the JSON envelope, not plain text", ct, jsonContentType)
	}
}

// TestHandler_UnknownMethodAndPath_AreNotTheSurfacesOwnRefusals pins what
// this surface deliberately does NOT promise: an undocumented method on a
// documented path (POST on a read-only fragment) and an unknown path are
// net/http's own answers, not the module's envelope -- there is no
// documented response for either, and no billing code for a request the
// fragment does not define.
func TestHandler_UnknownMethodAndPath_AreNotTheSurfacesOwnRefusals(t *testing.T) {
	h, _, _ := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"

	for _, target := range []string{apiPath + "/credits/nope", "/api/v1/billing"} {
		rec := billingRequest(t, h, tenant, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d (net/http's own 404 for an unknown path)", target, rec.Code, http.StatusNotFound)
		}
	}
}

// decodeInvoices decodes rec as the invoices-list wire shape, requiring
// wantStatus.
func decodeInvoices(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.BillingListInvoicesResponse {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("invoices status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out api.BillingListInvoicesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode invoices body %s: %v", rec.Body.String(), err)
	}
	return out
}

// decodeInvoice decodes rec as the single-invoice wire shape, requiring
// wantStatus.
func decodeInvoice(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.BillingInvoice {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("invoice status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out api.BillingInvoice
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode invoice body %s: %v", rec.Body.String(), err)
	}
	return out
}

// createInvoiceFor seeds one invoice for tenant through the repository's
// plain-Go lifecycle (the same path a host uses -- nothing writes
// invoices over HTTP), with a one-month period starting at periodStart,
// and returns the row.
func createInvoiceFor(t *testing.T, invoices *InvoiceRepository, tenant pkgcore.TenantID, amount Money, periodStart time.Time) *Invoice {
	t.Helper()
	inv, err := invoices.CreateInvoice(pkgcore.WithTenant(t.Context(), tenant), CreateInvoiceInput{
		SubscriptionID: "sub-1",
		Amount:         amount,
		PeriodStart:    periodStart,
		PeriodEnd:      periodStart.AddDate(0, 1, 0),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

// TestHandler_ListInvoices_ServesTheTenantsDocumentsNewestFirst seeds
// three invoices for one tenant through the repository's plain-Go
// lifecycle -- one left open, one marked paid, one voided -- and reads
// the listing back through the route, asserting the newest-first order
// (issue order, whatever the billed periods say), the full wire row of
// each (amount minor units, currency, subscription reference, billed
// cycle, issue/touch times), and the lifecycle's own open/paid/void
// vocabulary on the wire.
func TestHandler_ListInvoices_ServesTheTenantsDocumentsNewestFirst(t *testing.T) {
	h, _, invoices := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"
	ctx := pkgcore.WithTenant(t.Context(), tenant)
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// invoice-1: the oldest, still open.
	inv1 := createInvoiceFor(t, invoices, tenant, Money{Cents: 4900, Currency: "USD"}, sep1)
	// invoice-2: settled in full (MarkPaid).
	inv2 := createInvoiceFor(t, invoices, tenant, Money{Cents: 1200, Currency: "USD"}, sep1.AddDate(0, 1, 0))
	if _, err := invoices.MarkPaid(ctx, inv2.ID); err != nil {
		t.Fatalf("mark invoice-2 paid: %v", err)
	}
	// invoice-3: the newest, voided before payment -- its voided status
	// must not move it out of the newest-first head.
	inv3 := createInvoiceFor(t, invoices, tenant, Money{Cents: 9900, Currency: "CNY"}, sep1.AddDate(0, 2, 0))
	if _, err := invoices.Void(ctx, inv3.ID); err != nil {
		t.Fatalf("void invoice-3: %v", err)
	}

	page := decodeInvoices(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices"), http.StatusOK)
	if len(page.Invoices) != 3 {
		t.Fatalf("invoices carry %d rows, want 3; body = %+v", len(page.Invoices), page.Invoices)
	}
	wantIDs := []string{inv3.ID, inv2.ID, inv1.ID}
	wantStatuses := []api.BillingInvoiceStatus{api.Void, api.Paid, api.Open}
	wantAmounts := []int64{9900, 1200, 4900}
	periods := []time.Time{sep1.AddDate(0, 2, 0), sep1.AddDate(0, 1, 0), sep1}
	for i, row := range page.Invoices {
		if row.ID != wantIDs[i] {
			t.Errorf("row %d id = %q, want %q -- newest first is issue order", i, row.ID, wantIDs[i])
		}
		if row.Status != wantStatuses[i] {
			t.Errorf("row %d status = %q, want %q -- the lifecycle vocabulary travels verbatim", i, row.Status, wantStatuses[i])
		}
		if row.AmountCents != wantAmounts[i] {
			t.Errorf("row %d amountCents = %d, want %d", i, row.AmountCents, wantAmounts[i])
		}
		if row.SubscriptionID != "sub-1" {
			t.Errorf("row %d subscriptionId = %q, want sub-1", i, row.SubscriptionID)
		}
		if !row.PeriodStart.Equal(periods[i]) || !row.PeriodEnd.Equal(periods[i].AddDate(0, 1, 0)) {
			t.Errorf("row %d period = %v..%v, want the seeded %v cycle", i, row.PeriodStart, row.PeriodEnd, periods[i])
		}
		if row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
			t.Errorf("row %d createdAt/updatedAt = %v/%v, want both set", i, row.CreatedAt, row.UpdatedAt)
		}
	}
	if row := page.Invoices[0]; row.Currency != "CNY" {
		t.Errorf("newest row's currency = %q, want CNY -- each row's own currency travels verbatim", row.Currency)
	}
}

// TestHandler_ListAndGetInvoices_NeverLeakAnotherTenantsDocuments writes
// invoices for two tenants and reads each tenant's listing back through
// the route: a caller only ever sees its own tenant's documents, and a
// tenant with none answers an empty array -- never null, and never
// another tenant's rows. The get route's half of the same proof: an id
// naming another tenant's invoice answers the identical
// billing.invoice_not_found 404 as an id that never existed, so the
// route discloses nothing about whether the id exists at all.
func TestHandler_ListAndGetInvoices_NeverLeakAnotherTenantsDocuments(t *testing.T) {
	h, _, invoices := newHandlerForTest(t)
	const tenantA pkgcore.TenantID = "tenant-a"
	const tenantB pkgcore.TenantID = "tenant-b"

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	invA := createInvoiceFor(t, invoices, tenantA, Money{Cents: 4900, Currency: "USD"}, start)
	invB := createInvoiceFor(t, invoices, tenantB, Money{Cents: 250, Currency: "CNY"}, start)

	pageA := decodeInvoices(t, billingRequest(t, h, tenantA, http.MethodGet, apiPath+"/invoices"), http.StatusOK)
	if len(pageA.Invoices) != 1 || pageA.Invoices[0].ID != invA.ID {
		t.Fatalf("tenant-a listing = %+v, want exactly its own %q", pageA.Invoices, invA.ID)
	}
	if row := pageA.Invoices[0]; row.Currency != "USD" {
		t.Errorf("tenant-a row's currency = %q, want USD (never tenant-b's CNY row)", row.Currency)
	}

	pageB := decodeInvoices(t, billingRequest(t, h, tenantB, http.MethodGet, apiPath+"/invoices"), http.StatusOK)
	if len(pageB.Invoices) != 1 || pageB.Invoices[0].ID != invB.ID {
		t.Fatalf("tenant-b listing = %+v, want exactly its own %q", pageB.Invoices, invB.ID)
	}

	pageC := decodeInvoices(t, billingRequest(t, h, pkgcore.TenantID("tenant-c"), http.MethodGet, apiPath+"/invoices"), http.StatusOK)
	if pageC.Invoices == nil {
		t.Fatal("never-invoiced tenant's invoices is null, want the empty array the schema promises")
	}
	if len(pageC.Invoices) != 0 {
		t.Errorf("never-invoiced tenant's listing carries %d rows, want 0", len(pageC.Invoices))
	}

	// A get for another tenant's invoice answers the same coded 404 as a
	// never-created id, with the looked-up id as the structured param --
	// tenant-b learns nothing about whether tenant-a's id exists.
	for _, target := range []string{apiPath + "/invoices/" + invA.ID, apiPath + "/invoices/no-such-id"} {
		rec := billingRequest(t, h, tenantB, http.MethodGet, target)
		env := decodeEnvelope(t, rec, http.StatusNotFound)
		if env.Code != "billing.invoice_not_found" {
			t.Errorf("GET %s code = %q, want %q", target, env.Code, "billing.invoice_not_found")
		}
		if env.Params == nil {
			t.Fatalf("GET %s envelope carries no params, want the looked-up id named", target)
		}
	}
}

// TestHandler_GetInvoice_AnswersTheFullShapeOfTheTenantsOwnDocument
// seeds one invoice, reads it back through the get route and asserts
// every field the model carries travels on the wire -- the money split
// and the billed cycle included -- then settles the invoice
// service-side and reads the status transition back: the detail route
// answers the lifecycle's current state, never a stale snapshot.
func TestHandler_GetInvoice_AnswersTheFullShapeOfTheTenantsOwnDocument(t *testing.T) {
	h, _, invoices := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"
	ctx := pkgcore.WithTenant(t.Context(), tenant)
	periodStart := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)

	inv := createInvoiceFor(t, invoices, tenant, Money{Cents: 12345, Currency: "CNY"}, periodStart)

	got := decodeInvoice(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices/"+inv.ID), http.StatusOK)
	if got.ID != inv.ID {
		t.Errorf("id = %q, want %q", got.ID, inv.ID)
	}
	if got.SubscriptionID != "sub-1" {
		t.Errorf("subscriptionId = %q, want sub-1", got.SubscriptionID)
	}
	if got.Status != api.Open {
		t.Errorf("status = %q, want %q", got.Status, api.Open)
	}
	if got.AmountCents != 12345 || got.Currency != "CNY" {
		t.Errorf("amount = %d %s, want 12345 CNY", got.AmountCents, got.Currency)
	}
	if !got.PeriodStart.Equal(periodStart) || !got.PeriodEnd.Equal(periodStart.AddDate(0, 1, 0)) {
		t.Errorf("period = %v..%v, want the seeded cycle", got.PeriodStart, got.PeriodEnd)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("createdAt/updatedAt = %v/%v, want both set", got.CreatedAt, got.UpdatedAt)
	}

	// A status transition made service-side is what the next get answers.
	if _, err := invoices.MarkPaid(ctx, inv.ID); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	settled := decodeInvoice(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices/"+inv.ID), http.StatusOK)
	if settled.Status != api.Paid {
		t.Errorf("status after MarkPaid = %q, want %q", settled.Status, api.Paid)
	}
}

// TestHandler_ListInvoices_EnforcesTheLimitBound tests the list route's
// limit parameter contract, the same contract the transactions route
// pins: it narrows the newest-first window to the requested size,
// defaults to defaultListPageSize when absent, and refuses an integer
// outside the fragment's 1-100 bound with the mapped
// billing.invalid_limit envelope carrying the structured bound -- while
// a limit that is not an integer at all is refused by the
// spec-generated parameter binder before the handler runs and answered
// as billing.invalid_request through the shared bindingErrorHandler,
// never a plain-text http.Error.
func TestHandler_ListInvoices_EnforcesTheLimitBound(t *testing.T) {
	h, _, invoices := newHandlerForTest(t)
	const tenant pkgcore.TenantID = "tenant-a"

	for i := 0; i < 3; i++ {
		createInvoiceFor(t, invoices, tenant, Money{Cents: int64(100 * (i + 1)), Currency: "USD"}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	}

	// limit=2 narrows the newest-first window to the two newest rows.
	page := decodeInvoices(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices?limit=2"), http.StatusOK)
	if len(page.Invoices) != 2 {
		t.Fatalf("limit=2 returned %d rows, want 2", len(page.Invoices))
	}
	if page.Invoices[0].AmountCents != 300 || page.Invoices[1].AmountCents != 200 {
		t.Errorf("limit=2 rows (newest first) = amounts %d, %d, want 300, 200", page.Invoices[0].AmountCents, page.Invoices[1].AmountCents)
	}

	// Absent limit defaults to the full recent window for this small
	// invoice set (the default page size comfortably covers it).
	all := decodeInvoices(t, billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices"), http.StatusOK)
	if len(all.Invoices) != 3 {
		t.Errorf("absent limit returned %d rows, want all 3", len(all.Invoices))
	}

	for _, limit := range []string{"0", "101"} {
		rec := billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices?limit="+limit)
		env := decodeEnvelope(t, rec, http.StatusBadRequest)
		if env.Code != "billing.invalid_limit" {
			t.Errorf("limit=%s code = %q, want %q", limit, env.Code, "billing.invalid_limit")
		}
		if env.Params == nil {
			t.Fatalf("limit=%s envelope carries no params, want the structured bound", limit)
		}
		for _, key := range []string{"limit", "min", "max"} {
			if _, ok := (*env.Params)[key]; !ok {
				t.Errorf("limit=%s envelope params lack %q; params = %v", limit, key, *env.Params)
			}
		}
	}

	// The malformed-limit half of the never-a-raw-error contract, on this
	// route too: a limit that is not an integer answers the coded
	// envelope, not the binder's plain text.
	rec := billingRequest(t, h, tenant, http.MethodGet, apiPath+"/invoices?limit=abc")
	env := decodeEnvelope(t, rec, http.StatusBadRequest)
	if env.Code != "billing.invalid_request" {
		t.Errorf("malformed-limit code = %q, want %q", env.Code, "billing.invalid_request")
	}
}
