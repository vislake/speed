package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin/api"
	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestUsageService_NeitherModuleWired_Refused pins the one boot-shaped
// refusal at request time: a dashboard with nothing at all to stitch
// (neither go/metering nor go/billing ever wired) is a wiring gap, not a
// partial answer.
func TestUsageService_NeitherModuleWired_Refused(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewUsageService(nil, nil, NewTenantService(NewTenantRepository(db)))

	_, err := svc.Summary(context.Background(), "operator-1")
	if !apperr.HasCode(err, ErrUsageModulesNotWired.Code) {
		t.Fatalf("Summary() with neither module wired error = %v, want %s", err, ErrUsageModulesNotWired.Code)
	}
}

// TestUsageService_Summary_StitchesMeteringAndBilling is the dashboard's
// core proof: real go/metering and go/billing data for a real,
// ledger-registered tenant is stitched into that tenant's own row, with
// no new aggregate table of admin's own -- Summary reads straight through
// to metering.SummaryRepository.List and billing.CreditService.Balance/
// billing.SubscriptionService.Active.
func TestUsageService_Summary_StitchesMeteringAndBilling(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-usage-flow")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Usage Flow Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	// A real metering event, folded into a real UsageSummary row.
	if err := env.Metering.Aggregator().Ingest(context.Background(), metering.UsageEvent{
		TenantID:       string(tenant),
		Feature:        "ai.generation",
		Quantity:       3,
		IdempotencyKey: "usage-flow-key-1",
	}); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	// A real credit grant, materializing a real CreditBalance row.
	billingCtx := pkgcore.WithTenant(context.Background(), tenant)
	if _, err := env.Billing.Credits().Grant(billingCtx, billing.GrantInput{Amount: 500, Reason: "usage-flow-test"}); err != nil {
		t.Fatalf("Grant() error = %v", err)
	}

	svc := NewUsageService(env.Metering, env.Billing, env.Admin.Tenants())

	rows, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}

	var row *UsageSummaryRow
	for i := range rows {
		if rows[i].TenantID == string(tenant) {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, tenant)
	}

	if len(row.MeteringSummaries) != 1 || row.MeteringSummaries[0].Feature != "ai.generation" || row.MeteringSummaries[0].Quantity != 3 {
		t.Errorf("row.MeteringSummaries = %+v, want exactly one ai.generation row of quantity 3", row.MeteringSummaries)
	}
	if row.CreditBalance == nil || row.CreditBalance.Available != 500 {
		t.Errorf("row.CreditBalance = %+v, want Available=500", row.CreditBalance)
	}
	// No subscription was ever created for this tenant.
	if row.ActiveSubscription != nil {
		t.Errorf("row.ActiveSubscription = %+v, want nil (none created)", row.ActiveSubscription)
	}
}

// TestUsageService_Summary_OnlyMeteringWired proves the two modules are
// independently optional: wiring only go/metering leaves every row's
// CreditBalance/ActiveSubscription absent (nil), never refusing the
// whole call, and never fabricating billing data that was never wired.
func TestUsageService_Summary_OnlyMeteringWired(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-usage-metering-only")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Metering Only Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	svc := NewUsageService(env.Metering, nil, env.Admin.Tenants())

	rows, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}

	found := false
	for _, row := range rows {
		if row.TenantID != string(tenant) {
			continue
		}
		found = true
		if row.MeteringSummaries == nil {
			t.Errorf("row.MeteringSummaries = nil, want a non-nil (possibly empty) slice since go/metering is wired")
		}
		if row.CreditBalance != nil {
			t.Errorf("row.CreditBalance = %+v, want nil: go/billing was never wired", row.CreditBalance)
		}
		if row.ActiveSubscription != nil {
			t.Errorf("row.ActiveSubscription = %+v, want nil: go/billing was never wired", row.ActiveSubscription)
		}
	}
	if !found {
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, tenant)
	}
}

// failingSingleRowTenantDB returns a *gorm.DB session cloned from db whose
// single-row admin_tenants lookups (TenantRepository.Get's own
// .Where(...).First(&t) call) always fail, while a bulk lookup on the same
// table (TenantRepository.List's own .Find(&rows), which
// TenantService.ListAllRows pages through) is completely unaffected. It
// makes "does this read path re-fetch a tenant row per tenant?"
// deterministically observable: a walk built on the ledger listing must
// answer every row's display name from that same listing, never through a
// single-row re-read the forced failure would blank out.
func failingSingleRowTenantDB(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	session := db.Session(&gorm.Session{NewDB: true})
	err := session.Callback().Query().Before("gorm:query").Register("admin_test:fail_single_row_tenant_get", func(tx *gorm.DB) {
		if tx.Statement.Table != "admin_tenants" {
			return
		}
		dest := reflect.ValueOf(tx.Statement.Dest)
		if dest.Kind() == reflect.Ptr && dest.Elem().Kind() == reflect.Struct {
			tx.Error = errors.New("forced single-row admin_tenants read failure (test)")
		}
	})
	if err != nil {
		t.Fatalf("register test callback: %v", err)
	}
	return session
}

// TestUsageService_Summary_DisplayNameComesFromTheLedgerListing pins that
// a summary row's display name is read from the same ledger listing that
// produces the row (TenantService.forEachLedgerTenant's own paged walk),
// never through a per-tenant re-read of the tenant row: with every
// single-row admin_tenants read forced to fail, Summary must still
// succeed and must still render the ledger's recorded name -- exactly the
// evidence a per-tenant Get-based read could not produce.
func TestUsageService_Summary_DisplayNameComesFromTheLedgerListing(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-displayname-listing-flow")
	if err := env.Admin.Tenants().Create(context.Background(), &Tenant{
		TenantID:    string(tenant),
		DisplayName: "DisplayName Listing Co",
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	failingTenants := NewTenantService(NewTenantRepository(failingSingleRowTenantDB(t, env.DB)))
	failingTenants.attachAudit(env.Registry.EventBus(), env.Registry.AuditActions, nil)
	svc := NewUsageService(env.Metering, nil, failingTenants)

	rows, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v, want success (no per-tenant row re-read exists for the forced failure to hit)", err)
	}

	var row *UsageSummaryRow
	for i := range rows {
		if rows[i].TenantID == string(tenant) {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, tenant)
	}
	if row.DisplayName != "DisplayName Listing Co" {
		t.Errorf("row.DisplayName = %q, want the ledger listing's own recorded name -- the name must come from the same listing that produced the row", row.DisplayName)
	}
}

// TestUsageService_Summary_MaterializesExactlyOneZeroBalanceRowPerRowlessTenant
// pins the write shape Summary's own doc comment states: the billing leg
// reads balances through billing.CreditService.Balance, whose documented
// materialize-on-first-read contract creates one zero-valued
// billing_credit_balances row per ledger tenant that has no row yet -- so
// the dashboard is not strictly read-only, and the materialization is the
// whole of what it writes. Around that: a tenant that already has a row is
// never written (its stored row and its answer are exactly what its own
// credit history produced), a repeated Summary writes nothing further, and
// no credit-transaction row is ever created.
func TestUsageService_Summary_MaterializesExactlyOneZeroBalanceRowPerRowlessTenant(t *testing.T) {
	env := buildTestAdminModule(t)

	const (
		tenantWithBalance = pkgcore.TenantID("tenant-usage-write-balanced")
		tenantRowless     = pkgcore.TenantID("tenant-usage-write-rowless")
	)
	for _, tc := range []struct {
		id   pkgcore.TenantID
		name string
	}{
		{tenantWithBalance, "Has Balance Co"},
		{tenantRowless, "Never Touched Credits Co"},
	} {
		if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tc.id), tc.name, "workspace"); err != nil {
			t.Fatalf("CreateRoot(%q) error = %v", tc.id, err)
		}
	}

	// A real credit grant for tenantWithBalance: a real row worth 500, and
	// the ledger transaction behind it. tenantRowless has no row at all --
	// Summary is about to be the first thing that could ever materialize
	// one for it.
	if _, err := env.Billing.Credits().Grant(pkgcore.WithTenant(context.Background(), tenantWithBalance), billing.GrantInput{Amount: 500, Reason: "usage-write-test"}); err != nil {
		t.Fatalf("Grant() error = %v", err)
	}

	// The test's own seat on the billing_credit_balances table, through
	// billing's exported repository type over the shared test db -- the
	// fixture-read shape testAdminEnv.DB's own doc comment sanctions,
	// never a raw query.
	balances := billing.NewCreditBalanceRepository(env.DB)
	readBalance := func(id pkgcore.TenantID) *billing.CreditBalance {
		t.Helper()
		row, err := balances.FindByID(pkgcore.WithTenant(context.Background(), id), string(id))
		if err != nil {
			t.Fatalf("FindByID(%q) error = %v, want a stored balance row", id, err)
		}
		return row
	}
	balanceRowExists := func(id pkgcore.TenantID) bool {
		t.Helper()
		_, err := balances.FindByID(pkgcore.WithTenant(context.Background(), id), string(id))
		return err == nil
	}
	transactionCount := func(id pkgcore.TenantID) int {
		t.Helper()
		rows, err := billing.NewCreditTransactionRepository(env.DB).ListByTenant(pkgcore.WithTenant(context.Background(), id))
		if err != nil {
			t.Fatalf("ListByTenant(%q) error = %v", id, err)
		}
		return len(rows)
	}

	if !balanceRowExists(tenantWithBalance) {
		t.Fatalf("precondition: tenant %q should have a stored balance row after Grant", tenantWithBalance)
	}
	if balanceRowExists(tenantRowless) {
		t.Fatalf("precondition: tenant %q should have no stored balance row yet", tenantRowless)
	}

	svc := NewUsageService(env.Metering, env.Billing, env.Admin.Tenants())

	first, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	findRow := func(rows []UsageSummaryRow, id pkgcore.TenantID) *UsageSummaryRow {
		t.Helper()
		for i := range rows {
			if rows[i].TenantID == string(id) {
				return &rows[i]
			}
		}
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, id)
		return nil
	}

	// The documented write happened: tenantRowless now has the zero-valued
	// row Balance materialized, and its answer row is that zero balance --
	// the only way billing's own read surface could ever answer a tenant
	// that has never touched credits.
	rowlessRow := findRow(first, tenantRowless)
	if rowlessRow.CreditBalance == nil {
		t.Fatalf("row for %q: CreditBalance = nil, want the materialized zero balance (billing is wired)", tenantRowless)
	}
	if rowlessRow.CreditBalance.Available != 0 || rowlessRow.CreditBalance.Reserved != 0 {
		t.Errorf("row for %q: CreditBalance = %+v, want Available=0 Reserved=0", tenantRowless, rowlessRow.CreditBalance)
	}
	stored := readBalance(tenantRowless)
	if stored.Available != 0 || stored.Reserved != 0 {
		t.Errorf("stored balance row for %q = %+v, want the zero-valued row Summary materialized", tenantRowless, stored)
	}

	// A tenant that already had a row was never written: its stored row and
	// its answer are exactly what Grant created, unchanged by the summary.
	balancedRow := findRow(first, tenantWithBalance)
	if balancedRow.CreditBalance == nil || balancedRow.CreditBalance.Available != 500 || balancedRow.CreditBalance.Reserved != 0 {
		t.Errorf("row for %q: CreditBalance = %+v, want Available=500 Reserved=0 (the stored row, unchanged)", tenantWithBalance, balancedRow.CreditBalance)
	}
	storedBalanced := readBalance(tenantWithBalance)
	if storedBalanced.Available != 500 || storedBalanced.Reserved != 0 {
		t.Errorf("stored balance row for %q = %+v, want Available=500 (unchanged by Summary)", tenantWithBalance, storedBalanced)
	}

	// Nothing else was written: the rowless tenant's credit-transaction
	// ledger is still empty after the summary that materialized its balance
	// row.
	if n := transactionCount(tenantRowless); n != 0 {
		t.Errorf("credit transactions for %q after Summary = %d, want 0 (Summary writes no ledger rows)", tenantRowless, n)
	}

	// A repeated Summary writes nothing further and answers identically:
	// the same stored rows, the same answer rows.
	second, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("second Summary() error = %v", err)
	}
	if n := transactionCount(tenantRowless); n != 0 {
		t.Errorf("credit transactions for %q after second Summary = %d, want 0", tenantRowless, n)
	}
	for _, id := range []pkgcore.TenantID{tenantWithBalance, tenantRowless} {
		firstRow, secondRow := findRow(first, id), findRow(second, id)
		if !reflect.DeepEqual(firstRow.CreditBalance, secondRow.CreditBalance) {
			t.Errorf("tenant %q: CreditBalance changed between summaries: %+v -> %+v", id, firstRow.CreditBalance, secondRow.CreditBalance)
		}
	}
}

// TestHandler_AdminGetUsageSummary_ComposedRowsOverHTTP is the usage
// dashboard's composed-HTTP proof: GET /api/v1/admin/usage-summary
// through the real handler renders one row per ledger tenant with
// go/metering's and go/billing's data stitched in -- exercised through the
// Module-level WithMetering/WithBilling wiring buildTestAdminModule now
// applies (the identical options a real host's internal/app/server.go
// passes), not through a hand-built UsageService. A tenant with recorded
// usage and a credit grant answers its metering summaries and balance on
// the wire; a tenant that never touched either answers an empty
// summaries list and a materialized zero balance -- and neither row
// fabricates a subscription none exists.
func TestHandler_AdminGetUsageSummary_ComposedRowsOverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	ctx := context.Background()

	const busyTenant = pkgcore.TenantID("tenant-usage-http-busy")
	// The manual-registration path (Tenants().Create), not the
	// event-driven lazy one: a manually registered row carries the display
	// name this test asserts rides the dashboard row.
	if err := env.Admin.Tenants().Create(ctx, &Tenant{TenantID: string(busyTenant), DisplayName: "Usage HTTP Busy Co"}); err != nil {
		t.Fatalf("Tenants().Create() error = %v", err)
	}
	if err := env.Metering.Aggregator().Ingest(ctx, metering.UsageEvent{
		TenantID: string(busyTenant), Feature: "ai.generation", Quantity: 3, IdempotencyKey: "usage-http-key-1",
	}); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if _, err := env.Billing.Credits().Grant(pkgcore.WithTenant(ctx, busyTenant), billing.GrantInput{Amount: 500, Reason: "usage-http-test"}); err != nil {
		t.Fatalf("Grant() error = %v", err)
	}

	const idleTenant = pkgcore.TenantID("tenant-usage-http-idle")
	if err := env.Admin.Tenants().Create(ctx, &Tenant{TenantID: string(idleTenant), DisplayName: "Usage HTTP Idle Co"}); err != nil {
		t.Fatalf("Tenants().Create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-summary", nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-usage-http"}))
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var resp api.AdminUsageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	rows := make(map[string]api.AdminUsageSummaryRow, len(resp.Rows))
	for _, row := range resp.Rows {
		rows[row.TenantID] = row
	}

	busy, ok := rows[string(busyTenant)]
	if !ok {
		t.Fatalf("response rows = %+v, want a row for %q", resp.Rows, busyTenant)
	}
	if busy.DisplayName != "Usage HTTP Busy Co" {
		t.Fatalf("busy row DisplayName = %q, want the ledger's display name", busy.DisplayName)
	}
	if busy.MeteringSummaries == nil || len(*busy.MeteringSummaries) != 1 || (*busy.MeteringSummaries)[0].Feature != "ai.generation" || (*busy.MeteringSummaries)[0].Quantity != 3 {
		t.Fatalf("busy row MeteringSummaries = %+v, want the recorded ai.generation x3 summary", busy.MeteringSummaries)
	}
	if busy.CreditBalance == nil || busy.CreditBalance.Available != 500 {
		t.Fatalf("busy row CreditBalance = %+v, want the granted 500", busy.CreditBalance)
	}
	if busy.ActiveSubscription != nil {
		t.Fatalf("busy row ActiveSubscription = %+v, want nil -- no subscription was ever created", busy.ActiveSubscription)
	}

	idle, ok := rows[string(idleTenant)]
	if !ok {
		t.Fatalf("response rows = %+v, want a row for %q", resp.Rows, idleTenant)
	}
	if idle.MeteringSummaries == nil || len(*idle.MeteringSummaries) != 0 {
		t.Fatalf("idle row MeteringSummaries = %+v, want a non-nil empty list -- go/metering is wired and this tenant has no usage", idle.MeteringSummaries)
	}
	if idle.CreditBalance == nil || idle.CreditBalance.Available != 0 {
		t.Fatalf("idle row CreditBalance = %+v, want the materialized zero balance", idle.CreditBalance)
	}
	if idle.ActiveSubscription != nil {
		t.Fatalf("idle row ActiveSubscription = %+v, want nil", idle.ActiveSubscription)
	}
}
