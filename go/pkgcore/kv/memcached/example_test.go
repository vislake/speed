package memcached_test

// Runnable documentation for this package's constructors, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead
// of silently rotting. The package's component descriptor -- the
// component a composition configuration selects as its module's
// implementation -- is exercised by the package's own component_test.go.

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
