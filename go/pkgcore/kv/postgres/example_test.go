package postgres_test

// Runnable documentation for the PostgreSQL-backed KVStore, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"context"
	"fmt"

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

// Example demonstrates the package's self-registration: importing it for
// side effect -- as a distributed-mode host does with a blank import when it
// wants a custom Preset naming "kv.postgres" to resolve the "kv" seam --
// makes "kv.postgres" build through pkgcore's shared KVStoreRegistry, the
// database/sql-style driver pattern kv/redis and eventbus/postgres also
// follow.
func Example() {
	store, caps, err := pkgcore.KVStoreRegistry.Build("kv.postgres", pkgcore.Config{
		"dsn": "postgres://user:pass@127.0.0.1:1/db",
	})
	fmt.Println(err, store != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe|SurvivesRestart
}
