package memcached_test

// Runnable documentation for the Memcached-backed KVStore, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"context"
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

// ExampleRegistration shows the name-registration path for a configuration
// the flat pkgcore.Config cannot express -- here a per-server topology
// assembled in code, the shape a host reaches for when the server list is
// computed rather than configured. The host builds the client itself, wraps
// it in the registration factory, registers it under a name of its own (the
// built-in "kv.memcached" name is taken), and names that registration in a
// Preset entry. The host keeps ownership: the factory's New returns the
// bare store over the host's client, so Kernel.Shutdown never touches
// either, and the host closes both when it shuts down. Nothing here dials;
// gomemcache connects per operation.
func ExampleRegistration() {
	client := memcache.New("cache-a.internal:11211", "cache-b.internal:11211")
	defer client.Close()

	name := "kv.memcached.host"
	if err := pkgcore.KVStoreRegistry.Register(kvmemcached.Registration(name, client)); err != nil {
		fmt.Println("register:", err)
		return
	}

	preset := pkgcore.PresetStandalone.With("kv", pkgcore.SeamPreset{Implementation: name})
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(preset)).Bootstrap(context.Background())
	fmt.Println(err, reg.KVStore() != nil)

	// Output:
	// <nil> true
}
