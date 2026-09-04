package metering_test

// Runnable documentation for metering's public API, mirroring
// go/dbkit/example_test.go's and go/pki/example_test.go's convention:
// this example is compiled AND executed by `go test`, so a change to
// metering's public API that breaks the documented usage fails the build
// rather than only rotting in prose.
//
// It covers Record (the analytics-grade tier) and Enqueue plus
// Dispatcher.RunOnce (the billing-grade tier's write and delivery
// halves), converging into the same Aggregator -- the "same pipeline,
// swappable backend" property doc.go's own doc comment describes.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/go/metering"
)

// Example wires a Module over a throwaway in-memory SQLite database,
// records one analytics-grade event and enqueues one billing-grade event,
// then reads both back through the same real-time counter.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:metering_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := metering.NewModule(db)

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

	// Start begins AnalyticsRecorder's background flush loop and
	// Dispatcher's poll loop; a real host calls it once, after
	// Kernel.Bootstrap has returned.
	m.Start(ctx)
	defer m.Stop()

	at := time.Now()

	// Analytics-grade: fire-and-forget, delivered asynchronously by the
	// background flush loop -- the whole point of this tier is that
	// Record never blocks the caller on delivery.
	if recErr := m.Recorder().Record(ctx, metering.UsageEvent{
		TenantID:       "tenant-acme",
		Feature:        "api.calls",
		Quantity:       1,
		IdempotencyKey: "analytics-1",
		OccurredAt:     at,
	}); recErr != nil {
		fmt.Println("record:", recErr)
		return
	}
	// Record's delivery is asynchronous by design -- a real caller would
	// not wait for it. Polling here is what keeps this example's output
	// deterministic rather than racing the background flush loop.
	waitForCount(m, "tenant-acme", "api.calls", at, 1)

	// Billing-grade: Enqueue is called inside the caller's OWN
	// transaction, so it lands atomically with whatever business write
	// shares it -- here, no other write shares it, since this example has
	// no other business data to write.
	enqueueErr := db.Transaction(func(tx *gorm.DB) error {
		_, enqErr := metering.Enqueue(ctx, tx, metering.UsageEvent{
			TenantID:       "tenant-acme",
			Feature:        "ai.generation",
			Quantity:       4,
			IdempotencyKey: "billing-1",
			OccurredAt:     at,
		})
		return enqErr
	})
	if enqueueErr != nil {
		fmt.Println("enqueue:", enqueueErr)
		return
	}
	// RunOnce is called synchronously here for a deterministic example; a
	// real host lets Dispatcher.Start's own poll loop (already running,
	// from m.Start above) drive delivery instead.
	if _, dispatchErr := m.Dispatcher().RunOnce(ctx); dispatchErr != nil {
		fmt.Println("dispatch:", dispatchErr)
		return
	}

	apiCalls, err := m.Aggregator().RealtimeCount("tenant-acme", "api.calls", at)
	if err != nil {
		fmt.Println("realtime count (api.calls):", err)
		return
	}
	generations, err := m.Aggregator().RealtimeCount("tenant-acme", "ai.generation", at)
	if err != nil {
		fmt.Println("realtime count (ai.generation):", err)
		return
	}
	fmt.Println("api.calls:", apiCalls)
	fmt.Println("ai.generation:", generations)

	// Output:
	// api.calls: 1
	// ai.generation: 4
}

// waitForCount polls m's Aggregator until its real-time counter for
// (tenantID, feature, at) reaches want, or gives up after a couple of
// seconds. See its one call site's comment for why polling belongs here.
func waitForCount(m *metering.Module, tenantID, feature string, at time.Time, want float64) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.Aggregator().RealtimeCount(tenantID, feature, at)
		if err == nil && got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
