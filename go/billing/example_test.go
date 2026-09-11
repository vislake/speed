package billing_test

// Runnable documentation for billing's public API, mirroring
// go/dbkit/example_test.go's, go/pki/example_test.go's and
// go/metering/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to billing's public API that breaks
// the documented usage fails the build rather than only rotting in prose.
//
// It walks the module's whole shape in one pass: a platform-wide Plan with
// a Boolean and a Quota grant, a Subscription activated onto it,
// Entitlements.Check answering both grant kinds, and the credits ledger's
// reserve -> confirm pattern -- the two independent paths the module's own
// split describes.
//
// This module has no reference-app consumer for these surfaces; this
// Example is the compensating obligation for that gap, the identical
// shape go/pki's X.509 layer uses for the same reason.

import (
	"context"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
)

// stubUsageReader is a fixed-zero UsageReader: this example never crosses
// its Quota grant's limit, so what RealtimeCount returns before that point
// does not matter for the output below. A real host wires
// *metering.Aggregator instead (UsageReader's own doc comment).
type stubUsageReader struct{}

func (stubUsageReader) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	return 0, nil
}

// Example wires a Module over a throwaway in-memory SQLite database,
// creates a platform-wide Plan, subscribes a tenant to it, checks both a
// Boolean and a Quota grant through Entitlements.Check, then reserves and
// confirms a credit spend.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:billing_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := billing.NewModule(db, stubUsageReader{})

	// Migrations are versioned SQL, applied through dbkit's registry.
	// There is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// A platform-wide Plan (TenantID left at platformScopeSentinel, i.e.
	// the Plan's own zero value): every tenant that subscribes to "pro"
	// gets these grants unless it has its own tenant-custom override --
	// see PlanStore.Resolve's doc comment for that lookup precedence.
	plan := &billing.Plan{Key: "pro", Name: "Pro"}
	plan.SetPrice(billing.Money{Cents: 4900, Currency: "USD"})
	plan.Interval = string(billing.BillingIntervalMonth)
	if setErr := plan.SetGrants([]billing.Grant{
		{FeatureKey: "priority_support", Value: true},
		{FeatureKey: "api_calls", Value: int64(10), Period: billing.ResetPeriodMonthly, OverageMode: billing.OverageModeBlock},
	}); setErr != nil {
		fmt.Println("set grants:", setErr)
		return
	}
	if createErr := m.Plans().Create(ctx, plan); createErr != nil {
		fmt.Println("create plan:", createErr)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")

	sub, err := m.Subscriptions().Create(tenantCtx, billing.CreateInput{PlanID: plan.ID})
	if err != nil {
		fmt.Println("create subscription:", err)
		return
	}
	if _, activateErr := m.Subscriptions().Activate(tenantCtx, sub.ID); activateErr != nil {
		fmt.Println("activate subscription:", activateErr)
		return
	}

	supportDecision, err := m.Entitlements().Check(tenantCtx, "priority_support", 1)
	if err != nil {
		fmt.Println("check priority_support:", err)
		return
	}
	fmt.Println("priority_support allowed:", supportDecision.Allowed)

	quotaDecision, err := m.Entitlements().Check(tenantCtx, "api_calls", 3)
	if err != nil {
		fmt.Println("check api_calls:", err)
		return
	}
	fmt.Println("api_calls allowed:", quotaDecision.Allowed, "remaining:", *quotaDecision.Remaining)

	// Credits are a separate path: Check above never touched the ledger,
	// and the ledger below never touches the Plan.
	if _, grantErr := m.Credits().Grant(tenantCtx, billing.GrantInput{Amount: 100, Reason: "plan:pro:monthly_included"}); grantErr != nil {
		fmt.Println("grant credits:", grantErr)
		return
	}
	if _, deductErr := m.Credits().PreDeduct(tenantCtx, billing.PreDeductInput{
		Amount:         30,
		IdempotencyKey: "ai_generation:job-1",
		Reason:         "ai_generation:job-1",
	}); deductErr != nil {
		fmt.Println("pre-deduct credits:", deductErr)
		return
	}
	if _, confirmErr := m.Credits().Confirm(tenantCtx, "ai_generation:job-1"); confirmErr != nil {
		fmt.Println("confirm credits:", confirmErr)
		return
	}

	balance, err := m.Credits().Balance(tenantCtx)
	if err != nil {
		fmt.Println("read balance:", err)
		return
	}
	fmt.Println("available:", balance.Available, "reserved:", balance.Reserved)

	// Output:
	// priority_support allowed: true
	// api_calls allowed: true remaining: 7
	// available: 70 reserved: 0
}

// ExampleCreditService_Transactions reads a tenant's credit ledger back
// through CreditService.Transactions -- the read surface the module's HTTP
// layer (handler.go) serves as the recent-transactions route, and a
// billing-history UI would call directly in-process. The listing is
// newest first, tenant-scoped from the context, and classifies every
// movement through each row's type/status pair: the grant row below is
// the top-up, and the deduct row that was refunded reports the refund as
// its own status -- observable as a ledger row, never a silent balance
// change. (A separate in-memory database keeps this example independent
// of Example's own.)
func ExampleCreditService_Transactions() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:billing_example_transactions?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := billing.NewModule(db, nil)

	// Migrations are versioned SQL, applied through dbkit's registry.
	// There is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")
	credits := m.Credits()

	// A top-up, then a reservation that is refunded rather than
	// confirmed -- the two ledger facts whose read-back this example
	// walks.
	if _, grantErr := credits.Grant(tenantCtx, billing.GrantInput{Amount: 100, Reason: "promo:welcome"}); grantErr != nil {
		fmt.Println("grant credits:", grantErr)
		return
	}
	if _, deductErr := credits.PreDeduct(tenantCtx, billing.PreDeductInput{
		Amount:         30,
		IdempotencyKey: "ai_generation:job-2",
		Reason:         "ai_generation:job-2",
	}); deductErr != nil {
		fmt.Println("pre-deduct credits:", deductErr)
		return
	}
	if _, refundErr := credits.Refund(tenantCtx, "ai_generation:job-2"); refundErr != nil {
		fmt.Println("refund credits:", refundErr)
		return
	}

	rows, err := credits.Transactions(tenantCtx)
	if err != nil {
		fmt.Println("read transactions:", err)
		return
	}
	for i, row := range rows {
		if i == 2 {
			break
		}
		// type/status are the ledger's classification vocabulary: the
		// refunded deduct row IS the refund, and the grant row is the
		// top-up. The id (a deduct row's idempotency key) is
		// nondeterministic here, so it is not printed.
		fmt.Printf("%d: type=%s status=%s amount=%d\n", i, row.Type, row.Status, row.Amount)
	}

	// The balance agrees with the ledger: the refunded reservation never
	// became a spend, so all 100 credits remain available.
	balance, err := credits.Balance(tenantCtx)
	if err != nil {
		fmt.Println("read balance:", err)
		return
	}
	fmt.Println("available:", balance.Available, "reserved:", balance.Reserved)

	// Output:
	// 0: type=deduct status=refunded amount=30
	// 1: type=grant status=confirmed amount=100
	// available: 100 reserved: 0
}

// ExampleCreditService_Expire walks the idempotency contract a
// jobs-driven expiry sweep depends on -- CreditService.Expire's keyed
// mode, the shape that answers a scheduler's retried run (a sweep window
// rerun after a crash or a timeout, under the same deterministic
// per-tenant+period key) with the first run's own row instead of
// deducting twice. The unkeyed form has no such contract and is for
// one-off, operator-driven deductions only (CreditService.Expire's own
// doc comment has the full contrast). Which credits a real sweep expires,
// and when, is the host's product-policy decision, never this method's.
func ExampleCreditService_Expire() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:billing_example_expire?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := billing.NewModule(db, nil)

	// Migrations are versioned SQL, applied through dbkit's registry.
	// There is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")
	credits := m.Credits()

	if _, grantErr := credits.Grant(tenantCtx, billing.GrantInput{Amount: 100, Reason: "plan:pro:monthly_included"}); grantErr != nil {
		fmt.Println("grant credits:", grantErr)
		return
	}

	// One expiry run for the policy period, keyed by the period the sweep
	// is running for. A sweep run is retryable: crashing after this call
	// committed and rerunning the same window must not deduct twice.
	window := "expiry:2026-09-policy"
	first, err := credits.Expire(tenantCtx, billing.PreDeductInput{
		Amount:         40,
		IdempotencyKey: window,
		Reason:         window,
	})
	if err != nil {
		fmt.Println("expire credits:", err)
		return
	}

	// The crashed run's retry: same window, same key. It is answered with
	// the first run's own row -- no second deduction.
	retried, err := credits.Expire(tenantCtx, billing.PreDeductInput{
		Amount:         40,
		IdempotencyKey: window,
		Reason:         window,
	})
	if err != nil {
		fmt.Println("expire credits:", err)
		return
	}
	fmt.Println("retry returned the first run's row:", retried.ID == first.ID)

	balance, err := credits.Balance(tenantCtx)
	if err != nil {
		fmt.Println("read balance:", err)
		return
	}
	fmt.Println("available:", balance.Available, "reserved:", balance.Reserved)

	// Output:
	// retry returned the first run's row: true
	// available: 60 reserved: 0
}

// ExampleSubscriptionService_EnsureActive walks the read-or-establish
// contract a boot seed and a provisioning chain converge on: the first
// call creates and activates a subscription against the given Plan, a
// repeated call returns that same subscription untouched, and a canceled
// subscription is never revived -- the next ensure subscribes the tenant
// anew, on a fresh row. (A separate in-memory database keeps this example
// independent of the others'.)
func ExampleSubscriptionService_EnsureActive() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:billing_example_ensure_active?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := billing.NewModule(db, nil)

	// Migrations are versioned SQL, applied through dbkit's registry.
	// There is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	plan := &billing.Plan{Key: "pro", Name: "Pro"}
	if createErr := m.Plans().Create(ctx, plan); createErr != nil {
		fmt.Println("create plan:", createErr)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")
	subs := m.Subscriptions()

	// No Active subscription yet: EnsureActive creates one against the
	// Plan and activates it.
	first, err := subs.EnsureActive(tenantCtx, billing.CreateInput{PlanID: plan.ID})
	if err != nil {
		fmt.Println("ensure:", err)
		return
	}
	fmt.Println("first ensure:", first.Status)

	// A repeated ensure -- the next boot's seed, a redelivered
	// provisioning attempt -- returns the same subscription untouched.
	again, err := subs.EnsureActive(tenantCtx, billing.CreateInput{PlanID: plan.ID})
	if err != nil {
		fmt.Println("ensure again:", err)
		return
	}
	fmt.Println("second ensure returned the same subscription:", again.ID == first.ID)

	// A canceled subscription stays terminal: the next ensure does not
	// revive it, it subscribes the tenant anew.
	if _, cancelErr := subs.Cancel(tenantCtx, first.ID); cancelErr != nil {
		fmt.Println("cancel:", cancelErr)
		return
	}
	afterCancel, err := subs.EnsureActive(tenantCtx, billing.CreateInput{PlanID: plan.ID})
	if err != nil {
		fmt.Println("ensure after cancel:", err)
		return
	}
	fmt.Println("after cancel:", afterCancel.Status, "on a new subscription:", afterCancel.ID != first.ID)

	// Output:
	// first ensure: active
	// second ensure returned the same subscription: true
	// after cancel: active on a new subscription: true
}
