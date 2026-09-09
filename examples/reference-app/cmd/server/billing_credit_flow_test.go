package main

// billing_credit_flow_test.go is the mandated end-to-end proof:
// go/billing's CreditService, reserved and settled by
// internal/smilesim's own wiring (internal/smilesim/service.go's "Credit
// accounting" section), driven through the REAL composed HTTP stack -- the
// same BuildServer/httptest.Server rig smilesim_flow_test.go's own suite
// uses -- across the three scenarios the credit leg of the billing
// surface requires:
//
//   - a tenant with a sufficient granted balance (the demo seed
//     internal/app/demo_credits.go's seedDemoCredits grants tenant-acme at
//     boot, unconditionally, before any test runs) generates an image
//     successfully and is genuinely debited afterward -- read back through
//     a real CreditService.Balance call over a SECOND database connection
//     (creditBalanceFor below), never merely trusted from the job's own
//     reported "succeeded" status;
//   - a tenant with NO granted balance (one outside cfg.HostTenants, so
//     seedDemoCredits never touches it, and CreditService.Balance
//     materializes a fresh all-zero row on first read) is refused BEFORE
//     the fake AI-gateway endpoint is ever reached -- asserted on the fake
//     server's own request counter, staying at zero;
//   - a generation that fails (a fake image endpoint that always answers
//     500, standing in for a vendor outage) leaves the reserved credits
//     refunded back to the tenant's pre-reservation balance once the job
//     dead-letters, rather than stuck in Reserved or lost from Available.
//
// What this suite deliberately does NOT prove, and never claims to: an
// actual credit-pack purchase through a real Stripe/Alipay/WeChat sandbox.
// No live credentials for any of the three providers exist in this
// environment, so that leg stays explicitly out of scope --
// internal/app/demo_credits.go's seedDemoCredits is the deliberate,
// documented stand-in (a Grant, never a payment) that gives this suite
// something real to reserve against.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// openBillingCredits opens a SECOND dbkit connection to cfg.SQLitePath and
// returns the billing.CreditService reached through it -- the identical
// "BuildServer hands out neither its *gorm.DB nor a module's own service,
// so a second connection is the only reach a test has into storage"
// pattern server_test.go's own TestBuildServer_NoteCreate_PersistsAuditEvent
// documents for go/dbkit/audit's table, applied here to go/billing's own
// tables instead. No migration call is needed on this second connection:
// BuildServer's own migrationRegistry.Apply already applied go/billing's
// migrations to cfg.SQLitePath before this test's server ever started
// serving.
func openBillingCredits(t *testing.T, cfg app.ServerConfig) *billing.CreditService {
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
	return billing.NewModule(db, nil).Credits()
}

// creditBalanceFor reads tenantID's real billing.CreditBalance through a
// fresh second connection (openBillingCredits) -- never trusted from the
// job's own reported status.
func creditBalanceFor(t *testing.T, cfg app.ServerConfig, tenantID pkgcore.TenantID) billing.CreditBalance {
	t.Helper()

	bal, err := openBillingCredits(t, cfg).Balance(pkgcore.WithTenant(context.Background(), tenantID))
	if err != nil {
		t.Fatalf("read credit balance of %q: %v", tenantID, err)
	}
	return *bal
}

// newAlwaysFailingImageServer answers every POST /images/edits with a
// fixed 500 -- standing in for a vendor outage or a rejected request, the
// simplest way to make go/ai-gateway's own image-generation job fail on
// every attempt so it exhausts its retries and reaches jobs.StatusDeadLetter
// deterministically. requests counts every call this server received, so a
// test can assert it was never reached at all (the insufficient-balance
// scenario) as well as assert on how many times it was (the failure
// scenario, which retries before dead-lettering).
type newAlwaysFailingImageServerResult struct {
	*httptest.Server
	requests int
}

func newAlwaysFailingImageServer(t *testing.T) *newAlwaysFailingImageServerResult {
	t.Helper()

	f := &newAlwaysFailingImageServerResult{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		http.Error(w, "simulated vendor outage", http.StatusInternalServerError)
	}))
	t.Cleanup(f.Close)
	return f
}

// auditEventsForTenant opens a SECOND dbkit connection to cfg.SQLitePath
// and reads tenantID's audit trail back through a real
// audit.Repository.ListByTenant call -- the identical "BuildServer hands
// out neither its *gorm.DB nor a module's own service, so a second
// connection is the only reach a test has into storage" pattern
// openBillingCredits above and server_test.go's own
// TestBuildServer_NoteCreate_PersistsAuditEvent both already use, applied
// here to prove go/billing's own audit.Emit wiring rather than notes'. No migration call is needed on this second
// connection: BuildServer's own migrationRegistry.Apply already applied
// go/dbkit/audit's migrations (auditModule shares this app's one database
// connection -- see internal/app/server.go's own auditModule construction comment)
// before this test's server ever started serving.
func auditEventsForTenant(t *testing.T, cfg app.ServerConfig, tenantID pkgcore.TenantID) []audit.AuditEvent {
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

	events, err := audit.NewRepository(db).ListByTenant(context.Background(), string(tenantID))
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", tenantID, err)
	}
	return events
}

// findAuditEvent returns the first event in events whose Action and
// Resource().ID match action and resourceID, and true -- or the zero
// value and false when none does. Several credit-ledger audit rows can
// share one tenant (this file's own tests seed a demo Grant at boot, on
// top of whatever reserve/confirm/refund the test itself drives), so
// asserting on "at least one event with this exact action and
// transaction id" is what actually proves the wiring, not a
// brittle exact-count or exact-order assumption over the whole table.
func findAuditEvent(events []audit.AuditEvent, action, resourceID string) (audit.AuditEvent, bool) {
	for _, evt := range events {
		if evt.Action == action && evt.Resource().ID == resourceID {
			return evt, true
		}
	}
	return audit.AuditEvent{}, false
}

// TestSmileSimulation_SuccessfulReserveConfirm_PersistsAuditEvents is the
// mandated proof: a real smilesim
// credit reserve/confirm cycle, driven through the REAL composed HTTP
// stack exactly like TestSmileSimulation_SufficientCredits_DebitsBalance
// above, produces real, readable go/dbkit/audit rows -- read back through
// a SECOND connection to the same SQLite file, never a mock or an
// in-memory event assertion (go/billing's own credit_service_test.go
// already covers that narrower unit-level claim). This is deliberately
// the same "explicit audit.Emit call, not dbkit's AuditBus write-capture
// plugin" wiring choice notes' own TestBuildServer_NoteCreate_
// PersistsAuditEvent proves for notes -- see internal/app/server.go's auditModule
// construction comment and the known same-file SQLITE_BUSY limitation
// for why: billingModule and auditModule share one database
// connection, and audit.Emit's own write only ever runs after
// CreditService's own mutating transaction has already committed
// (credit_service.go's emitCreditAudit), so no nested-transaction
// deadlock is possible here either.
func TestSmileSimulation_SuccessfulReserveConfirm_PersistsAuditEvents(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "credit-audit-owner")

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

	// internal/smilesim's own idempotency key is "smilesim:" + a fresh
	// uuid per generation request (service.go's own Simulate doc comment)
	// -- reused for every settlement call on this same generation, so the
	// PreDeduct and Confirm audit rows below share exactly one
	// credit_transaction id, which is also this test's own txID.
	events := auditEventsForTenant(t, cfg, tenantID)

	// The demo seed at boot (internal/app/demo_credits.go's seedDemoCredits)
	// is itself a real CreditService.Grant call, so a Grant-actioned row
	// for tenant-acme is expected to exist too -- this test only asserts
	// on the reserve/confirm pair a genuine transaction id ties together,
	// never on the table's total row count.
	reserveEvt, ok := findFirstCreditAuditEvent(events, billing.AuditActionCreditDeductReserve)
	if !ok {
		t.Fatalf("no %s audit event found for tenant %q; events = %+v", billing.AuditActionCreditDeductReserve, tenantID, events)
	}
	txID := reserveEvt.Resource().ID
	if txID == "" {
		t.Fatal("reserve audit event's Resource().ID is empty, want the credit_transaction id")
	}
	assertCreditAuditEvent(t, reserveEvt, tenantID, "credit_transaction", txID)

	confirmEvt, ok := findAuditEvent(events, billing.AuditActionCreditDeductConfirm, txID)
	if !ok {
		t.Fatalf("no %s audit event for credit_transaction %q; events = %+v", billing.AuditActionCreditDeductConfirm, txID, events)
	}
	assertCreditAuditEvent(t, confirmEvt, tenantID, "credit_transaction", txID)
}

// TestSmileSimulation_FailedGeneration_PersistsRefundAuditEvent is the
// refund half of the mandated proof: a generation that fails
// at the vendor (the same fake-500 image endpoint
// TestSmileSimulation_FailedGeneration_RefundsReservation above drives)
// leaves a real, readable audit.deduct_reserve row followed by a real
// audit.refund row for the SAME credit_transaction id, once the job
// reaches jobs.StatusDeadLetter and internal/smilesim's own
// NotifyOnCompletion settles the reservation with a real
// CreditService.Refund call.
func TestSmileSimulation_FailedGeneration_PersistsRefundAuditEvent(t *testing.T) {
	imgServer := newAlwaysFailingImageServer(t)

	cfg := testConfig(t)
	cfg.AIGatewayImageBaseURL = imgServer.URL
	cfg.AIGatewayImageAPIKey = "sk-test-smilesim-refund-audit-key"
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "credit-refund-audit-owner")

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

	final := waitForSmileSimSucceeded(t, srv, token, simulateOut.JobID, time.Now().Add(20*time.Second))
	if status, _ := final["status"].(string); status != "dead_letter" {
		t.Fatalf("final job status = %v, want \"dead_letter\": %+v", final["status"], final)
	}

	events := auditEventsForTenant(t, cfg, tenantID)
	reserveEvt, ok := findFirstCreditAuditEvent(events, billing.AuditActionCreditDeductReserve)
	if !ok {
		t.Fatalf("no %s audit event found for tenant %q; events = %+v", billing.AuditActionCreditDeductReserve, tenantID, events)
	}
	txID := reserveEvt.Resource().ID
	if txID == "" {
		t.Fatal("reserve audit event's Resource().ID is empty, want the credit_transaction id")
	}

	refundEvt, ok := findAuditEvent(events, billing.AuditActionCreditRefund, txID)
	if !ok {
		t.Fatalf("no %s audit event for credit_transaction %q; events = %+v", billing.AuditActionCreditRefund, txID, events)
	}
	assertCreditAuditEvent(t, refundEvt, tenantID, "credit_transaction", txID)

	// A dead-lettered generation must never also confirm the same
	// reservation -- Confirm and Refund are mutually exclusive terminal
	// states (go/billing's own ErrCreditTransactionAlreadyResolved rule).
	if _, ok := findAuditEvent(events, billing.AuditActionCreditDeductConfirm, txID); ok {
		t.Errorf("a %s audit event exists for credit_transaction %q, want none -- a dead-lettered generation must only ever be refunded", billing.AuditActionCreditDeductConfirm, txID)
	}
}

// findFirstCreditAuditEvent returns the first event in events whose
// Action is action, and true -- or the zero value and false when none
// does. Used only for AuditActionCreditDeductReserve above, where the
// test does not yet know the credit_transaction id it is looking for
// (that id is exactly what this call discovers) -- every subsequent
// lookup in the same test uses the more specific findAuditEvent instead,
// once txID is known.
func findFirstCreditAuditEvent(events []audit.AuditEvent, action string) (audit.AuditEvent, bool) {
	for _, evt := range events {
		if evt.Action == action {
			return evt, true
		}
	}
	return audit.AuditEvent{}, false
}

// assertCreditAuditEvent checks the fields every credit-ledger audit row
// must carry regardless of which of the five
// actions produced it: the acting tenant, the Resource shape
// emitCreditAudit always uses, and a successful Result (every call site
// only calls audit.Emit after its own mutating transaction has already
// committed -- see credit_service.go's emitCreditAudit doc comment).
func assertCreditAuditEvent(t *testing.T, evt audit.AuditEvent, tenantID pkgcore.TenantID, wantResourceType, wantResourceID string) {
	t.Helper()
	if evt.TenantID != string(tenantID) {
		t.Errorf("AuditEvent.TenantID = %q, want %q", evt.TenantID, tenantID)
	}
	if evt.Resource().Type != wantResourceType {
		t.Errorf("AuditEvent.Resource().Type = %q, want %q", evt.Resource().Type, wantResourceType)
	}
	if evt.Resource().ID != wantResourceID {
		t.Errorf("AuditEvent.Resource().ID = %q, want %q", evt.Resource().ID, wantResourceID)
	}
	if !evt.Result().Success {
		t.Errorf("AuditEvent.Result().Success = false, want true (action %q)", evt.Action)
	}
}

// TestSmileSimulation_SufficientCredits_DebitsBalance is scenario (a): a
// tenant whose demo-seeded balance covers smilesim.CreditsPerSimulation
// generates an image successfully through the real composed HTTP stack,
// and its balance is genuinely debited afterward.
func TestSmileSimulation_SufficientCredits_DebitsBalance(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "credit-sufficient-owner")

	before := creditBalanceFor(t, cfg, tenantID)
	if before.Available < smilesim.CreditsPerSimulation {
		t.Fatalf("tenant-acme's balance before simulating = %+v, want Available >= %d (seedDemoCredits should have granted it at boot)", before, smilesim.CreditsPerSimulation)
	}

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

	// A repeated poll (waitForSmileSimSucceeded's own loop already issued
	// at least one once the job turned terminal) must not double-settle --
	// this final read happens strictly after every poll this test made.
	after := creditBalanceFor(t, cfg, tenantID)
	if want := before.Available - smilesim.CreditsPerSimulation; after.Available != want {
		t.Errorf("Available after a successful generation = %d, want %d (before.Available %d minus CreditsPerSimulation %d)",
			after.Available, want, before.Available, smilesim.CreditsPerSimulation)
	}
	if after.Reserved != before.Reserved {
		t.Errorf("Reserved after a CONFIRMED generation = %d, want unchanged from %d -- Confirm must release the reservation, not leave it held", after.Reserved, before.Reserved)
	}
}

// TestSmileSimulation_InsufficientCredits_RefusedBeforeAIGatewayCall is
// scenario (b): a tenant whose balance has been drawn down below
// smilesim.CreditsPerSimulation is refused with a coded error before the
// fake AI-gateway endpoint is ever reached -- imgServer.requests stays at
// zero, proving go/ai-gateway (and therefore any real vendor) was never
// invoked.
//
// This deliberately reuses tenant-acme -- the same demo-seeded tenant the
// sufficient-balance scenario above uses -- rather than a brand-new,
// never-granted-membership tenant: a tenant this app's rbac has never
// granted any role to is refused with rbac.permission_denied on the
// storage upload this test needs BEFORE Simulate is ever reached, which
// would prove nothing about credits at all. Instead, this test drains
// tenant-acme's own demo-seeded balance down to a fixed, insufficient
// remainder via a second connection's real CreditService.Expire call --
// still a real credit-ledger operation, never a database row hand-edited
// directly.
func TestSmileSimulation_InsufficientCredits_RefusedBeforeAIGatewayCall(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "credit-insufficient-owner")

	seeded := creditBalanceFor(t, cfg, tenantID)
	if seeded.Available < smilesim.CreditsPerSimulation {
		t.Fatalf("tenant-acme's demo-seeded balance = %+v, want Available >= %d before draining it", seeded, smilesim.CreditsPerSimulation)
	}
	// Drain everything but a fixed remainder below CreditsPerSimulation, so
	// the refusal below is genuinely about an insufficient balance rather
	// than a coincidentally-exact one.
	const remainder = 1
	drainAmount := seeded.Available - remainder
	credits := openBillingCredits(t, cfg)
	if _, err := credits.Expire(pkgcore.WithTenant(context.Background(), tenantID), billing.ExpireInput{
		Amount: drainAmount, Reason: "test:drain-to-insufficient",
	}); err != nil {
		t.Fatalf("Expire (drain to insufficient): %v", err)
	}
	before := creditBalanceFor(t, cfg, tenantID)
	if before.Available != remainder || before.Available >= smilesim.CreditsPerSimulation {
		t.Fatalf("balance after draining = %+v, want Available == %d (< CreditsPerSimulation %d)", before, remainder, smilesim.CreditsPerSimulation)
	}

	// Upload a photo through the real storage HTTP surface -- Simulate must
	// refuse on the credit check alone, never on a missing input object, so
	// this proves the refusal is genuinely about credits.
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
	var errOut struct {
		Code string `json:"code"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&errOut); decodeErr != nil {
		resp.Body.Close()
		t.Fatalf("decode simulate error response: %v", decodeErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST %s status = %d, want %d (billing.insufficient_credits); body code = %q", smileSimulatePath, resp.StatusCode, http.StatusConflict, errOut.Code)
	}
	if errOut.Code != "billing.insufficient_credits" {
		t.Errorf("error code = %q, want %q", errOut.Code, "billing.insufficient_credits")
	}

	if imgServer.requests != 0 {
		t.Errorf("fake AI-gateway image endpoint received %d request(s), want 0 -- an insufficient balance must refuse before go/ai-gateway is ever called", imgServer.requests)
	}

	after := creditBalanceFor(t, cfg, tenantID)
	if after.Available != remainder || after.Reserved != 0 {
		t.Errorf("balance after a refused reservation = %+v, want Available == %d and Reserved == 0 -- a refused PreDeduct must leave no trace", after, remainder)
	}
}

// TestSmileSimulation_FailedGeneration_RefundsReservation is scenario (c):
// a generation that fails at the vendor (the fake server's own scripted
// 500 on every attempt, exhausting go/jobs' retries) leaves the reservation
// refunded back to the tenant's pre-reservation balance once the job
// reaches jobs.StatusDeadLetter.
func TestSmileSimulation_FailedGeneration_RefundsReservation(t *testing.T) {
	imgServer := newAlwaysFailingImageServer(t)

	cfg := testConfig(t)
	cfg.AIGatewayImageBaseURL = imgServer.URL
	cfg.AIGatewayImageAPIKey = "sk-test-smilesim-fail-key"
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "credit-failure-owner")

	before := creditBalanceFor(t, cfg, tenantID)
	if before.Available < smilesim.CreditsPerSimulation {
		t.Fatalf("tenant-acme's balance before simulating = %+v, want Available >= %d", before, smilesim.CreditsPerSimulation)
	}

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

	// Reserved right after enqueue, BEFORE the job has had any chance to
	// run -- proving the reservation genuinely happened at Simulate time,
	// not merely as a side effect of the eventual refund.
	reserved := creditBalanceFor(t, cfg, tenantID)
	if reserved.Available != before.Available-smilesim.CreditsPerSimulation || reserved.Reserved != before.Reserved+smilesim.CreditsPerSimulation {
		t.Fatalf("balance right after enqueue = %+v, want Available -%d / Reserved +%d from %+v", reserved, smilesim.CreditsPerSimulation, smilesim.CreditsPerSimulation, before)
	}

	// go/jobs' default retry policy (jobs.DefaultMaxRetries=3, exponential
	// backoff off jobs.DefaultBackoffBase=1s) takes several seconds to
	// exhaust every attempt before dead-lettering -- a longer deadline than
	// the success-path tests' own 5s, deliberately.
	final := waitForSmileSimSucceeded(t, srv, token, simulateOut.JobID, time.Now().Add(20*time.Second))
	if status, _ := final["status"].(string); status != "dead_letter" {
		t.Fatalf("final job status = %v, want \"dead_letter\": %+v", final["status"], final)
	}
	if imgServer.requests == 0 {
		t.Error("fake image endpoint received 0 requests, want at least 1 -- the job must genuinely have tried and failed")
	}

	after := creditBalanceFor(t, cfg, tenantID)
	if after.Available != before.Available || after.Reserved != before.Reserved {
		t.Errorf("balance after a dead-lettered generation = %+v, want Available/Reserved back to the pre-reservation balance %+v (Refund must release the reservation in full)", after, before)
	}
}
