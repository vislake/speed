package nats_test

// Runnable documentation for the NATS-backed KVStore, compiled by `go test`
// like every other package's examples, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
	kvnats "github.com/vislake/speed/go/pkgcore/kv/nats"
)

// ExampleNewKVStore shows the distributed deployment mode's NATS-backed
// counterpart of pkgcore.NewMemoryKVStore: the host hands its own already-
// connected *nats.Conn and a bucket name to the store, which provisions the
// JetStream KV bucket if it does not already exist and implements the same
// KVStore interface and semantics kv/redis and the in-memory store cover, so
// code written against the interface runs against any of the three.
//
// Unlike kv/redis's own ExampleNewKVStore, this one carries no "Output:"
// comment. kv/redis's NewKVStore dials nothing (go-redis connects lazily),
// so that example can run for real with no server present. This package's
// NewKVStore provisions a bucket at construction, which is a real round trip
// the server must answer, so this example is compiled -- and so still
// guards against the shown API drifting -- but never executed; the
// integration tier's own TestKVStore_ConformsToKVStoreContract runs the
// equivalent call for real, against a live NATS.
func ExampleNewKVStore() {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		panic(err)
	}
	defer nc.Close()

	kv, err := kvnats.NewKVStore(context.Background(), nc, "speed-kv")
	if err != nil {
		panic(err)
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the KVStore interface -- the same role
	// kv/redis's own example assertion plays.
	var _ pkgcore.KVStore = kv

	fmt.Println("store wired against a provisioned JetStream KV bucket")
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "kv.nats" build through pkgcore's shared
// KVStoreRegistry, the database/sql-style driver pattern kv/redis's own
// Example follows.
//
// Unlike that example, Build here really dials -- nats.Connect is
// synchronous, unlike go-redis's lazy client -- so this one points at an
// address nothing listens on and checks only that the failure is a
// connection failure, never pkgcore.ErrUnknownImplementation: proof that
// "kv.nats" really is registered, without this module's unit suite needing a
// live NATS server the way the integration tier's own copy of this
// assertion can afford to require.
func Example() {
	_, _, err := pkgcore.KVStoreRegistry.Build("kv.nats", pkgcore.Config{"url": "nats://127.0.0.1:1"})
	fmt.Println(errors.Is(err, pkgcore.ErrUnknownImplementation))

	// Output:
	// false
}

// ExampleRegistration shows the name-registration path for a typed
// configuration the flat pkgcore.Config cannot express: the host dials the
// connection itself -- here with a private CA pool -- picks the bucket in
// code, wraps both in the registration factory, registers the result under
// a name of its own (the built-in "kv.nats" name is taken), and names that
// registration in a Preset entry. The host keeps ownership: the factory's
// New returns the bare store over the host's connection, so Kernel.Shutdown
// never touches either, and the host closes both when it shuts down.
//
// Like ExampleNewKVStore above, this example carries no "Output:" comment:
// the factory's New provisions the JetStream bucket, a real round trip no
// example can perform without a live server, so the whole registration is
// compiled -- and so still guards against the shown API drifting -- but
// never executed.
func ExampleRegistration() {
	conn, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	name := "kv.nats.host"
	if err := pkgcore.KVStoreRegistry.Register(kvnats.Registration(name, conn, "speed-kv")); err != nil {
		panic(err)
	}

	preset := pkgcore.PresetStandalone.With("kv", pkgcore.SeamPreset{Implementation: name})
	if _, err := pkgcore.NewKernel(pkgcore.WithPreset(preset)).Bootstrap(context.Background()); err != nil {
		panic(err)
	}
}
