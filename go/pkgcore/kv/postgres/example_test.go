package postgres_test

// Runnable documentation for this package's constructors, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead
// of silently rotting. The package's component descriptor -- the
// component a composition configuration selects as its module's
// implementation -- is exercised by the package's own component_test.go.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
	kvpostgres "github.com/vislake/speed/go/pkgcore/kv/postgres"
)

// ExampleNewKVStore shows the PostgreSQL-backed counterpart of
// pkgcore.NewMemoryKVStore and kv/redis.NewKVStore: one store per replica,
// all sharing the deployment's PostgreSQL connection pool, so a value one
// replica writes is visible to every other. The DSN points at a closed port
// so this example stays hermetic: pgxpool.New never dials at construction,
// and the store's first operation is what reaches for PostgreSQL. A
// distributed-mode host must call kvpostgres.EnsureSchema once against the
// pool before the first operation, exactly like
// eventbus/postgres.EnsureSchema for its own table.
func ExampleNewKVStore() {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		fmt.Println("unexpected pool construction error:", err)
		return
	}
	defer pool.Close()

	kv := kvpostgres.NewKVStore(pool)
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor's result satisfies the KVStore interface -- the
	// memory-store counterpart -- so it is kept rather than inlined.
	var _ pkgcore.KVStore = kv

	fmt.Println("store wired; its first operation dials the server")
	// Output:
	// store wired; its first operation dials the server
}

// ExampleWithClock shows WithClock's role as the single-clock seam: the
// option accepts an application-clock source, and the store never consults
// it -- expiries are computed inside PostgreSQL as now() + ttl and judged
// against the database's own now(), so the clock supplied here is provably
// irrelevant. A test constructs the store with a deliberately skewed clock
// and pins that a Set with a live TTL still stays visible until the TTL
// genuinely elapses on the database clock (the integration tier's
// TestKVStore_TTLJudgedByTheDatabaseClockNotTheApplicationClock does
// exactly that); under clock-based arithmetic the same construction would
// make the key vanish the instant it was written. The DSN points at a
// closed port so this example stays hermetic, exactly like
// ExampleNewKVStore.
func ExampleWithClock() {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		fmt.Println("unexpected pool construction error:", err)
		return
	}
	defer pool.Close()

	kvpostgres.NewKVStore(pool, kvpostgres.WithClock(func() time.Time {
		// A clock ten minutes behind the database's: the skew that would
		// make every TTL'd write expire instantly under clock-based
		// arithmetic.
		return time.Now().Add(-10 * time.Minute)
	}))

	fmt.Println("store wired; its expiries are judged by the database clock, never this one")
	// Output:
	// store wired; its expiries are judged by the database clock, never this one
}
