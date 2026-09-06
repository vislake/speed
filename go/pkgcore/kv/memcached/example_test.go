package memcached_test

// Runnable documentation for the Memcached-backed KVStore, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"fmt"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
	kvmemcached "github.com/vislake/speed/go/pkgcore/kv/memcached"
)

// ExampleNewKVStore shows a second distributed-mode KVStore alongside
// kv/redis's: the host hands a Memcached client to the store, which
// implements the same KVStore interface and observable semantics kv/redis
// and the in-memory store cover. Read this before reaching for it in
// production: Memcached is a pure in-memory cache with NO persistence of any
// kind, so this store is honestly declared MultiReplicaSafe but NOT
// SurvivesRestart -- a Memcached process restart, rolling upgrade, or
// ordinary memory-pressure eviction silently drops everything it holds, with
// no error surfaced to any caller. Choose it for disposable, rebuildable
// state (a cache in the literal sense); choose kv/redis instead for anything
// that must survive a restart. Constructing the gomemcache client dials
// nothing; the store's first operation is what reaches for the server.
func ExampleNewKVStore() {
	client := memcache.New("memcached:11211")

	kv := kvmemcached.NewKVStore(client)
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the KVStore interface, so it is kept rather
	// than inlined.
	var _ pkgcore.KVStore = kv

	fmt.Println("store wired; Memcached holds no state that survives a restart")
	// Output:
	// store wired; Memcached holds no state that survives a restart
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "kv.memcached" build through pkgcore's shared
// KVStoreRegistry, the database/sql-style driver pattern kv/redis's own
// register.go follows. Unlike "kv.redis", no built-in Preset names this
// implementation -- a host that wants it wires it explicitly, exactly as
// this example does.
func Example() {
	store, caps, err := pkgcore.KVStoreRegistry.Build("kv.memcached", pkgcore.Config{})
	fmt.Println(err, store != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe
}
