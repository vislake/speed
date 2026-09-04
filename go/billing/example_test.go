package billing_test

// Runnable documentation for billing's public API, mirroring
// go/dbkit/example_test.go's, go/pki/example_test.go's and
// go/metering/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to billing's public API that breaks
// the documented usage fails the build rather than only rotting in prose.
//
// It walks the round's whole shape in one pass: a platform-wide Plan with
// a Boolean and a Quota grant, a Subscription activated onto it,
// Entitlements.Check answering both grant kinds, and the credits ledger's
// reserve -> confirm pattern -- the two independent paths
// docs/internal/06-billing-and-metering.md's own split describes.
//
// This module has no reference-app consumer yet (see AGENTS.md's Known
// limitations); this Example is the compensating obligation the round's
// own instructions call for in that situation, the identical shape
// go/pki's X.509 layer already uses for the same reason.

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
	fmt.Println("api_calls allowed:", quotaDecision.Allowed, "remaining:", quotaDecision.Remaining)

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
