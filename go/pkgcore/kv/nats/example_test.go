package nats_test

// Runnable documentation for this package's constructors, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead
// of silently rotting. The package's component descriptor -- the
// component a composition configuration selects as its module's
// implementation -- is exercised by the package's own component_test.go.

import (
	"context"
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
