package nats

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersKVNatsOnTheSharedRegistry proves this package's init()
// lands "kv.nats" on pkgcore's shared KVStoreRegistry -- the same assertion
// kv/redis's own register_test.go makes for "kv.redis". Unlike that test,
// Build here cannot succeed without a live NATS server (nats.Connect dials
// synchronously, unlike go-redis's lazy client), so this test points at an
// address nothing listens on and asserts only that the failure is a
// connection failure, never pkgcore.ErrUnknownImplementation -- proof that
// the name really is registered, without depending on Docker the way the
// full capability assertion (integration_test's own copy of this test) does.
func TestInit_RegistersKVNatsOnTheSharedRegistry(t *testing.T) {
	_, _, err := pkgcore.KVStoreRegistry.Build("kv.nats", pkgcore.Config{"url": "nats://127.0.0.1:1"})
	if err == nil {
		t.Fatal("Build() against an address nothing listens on succeeded, want a connection error")
	}
	if errors.Is(err, pkgcore.ErrUnknownImplementation) {
		t.Errorf("Build(%q) error = %v, want it registered (a connection failure, not %v)", "kv.nats", err, pkgcore.ErrUnknownImplementation)
	}
}

// TestNatsURLFromConfig_DefaultsWhenUnset pins the fallback address a
// zero-configuration Preset relies on.
func TestNatsURLFromConfig_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	if got, want := natsURLFromConfig(pkgcore.Config{}), "nats://127.0.0.1:4222"; got != want {
		t.Errorf("natsURLFromConfig(Config{}) = %q, want %q", got, want)
	}
	if got, want := natsURLFromConfig(pkgcore.Config{"url": "nats://example:4222"}), "nats://example:4222"; got != want {
		t.Errorf("natsURLFromConfig(Config{\"url\": ...}) = %q, want %q", got, want)
	}
}

// TestNatsBucketFromConfig_DefaultsWhenUnset pins the fallback bucket name a
// zero-configuration Preset relies on.
func TestNatsBucketFromConfig_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	if got, want := natsBucketFromConfig(pkgcore.Config{}), defaultBucket; got != want {
		t.Errorf("natsBucketFromConfig(Config{}) = %q, want %q", got, want)
	}
	if got, want := natsBucketFromConfig(pkgcore.Config{"bucket": "custom-bucket"}), "custom-bucket"; got != want {
		t.Errorf("natsBucketFromConfig(Config{\"bucket\": ...}) = %q, want %q", got, want)
	}
}

// TestConnFromConfig_InvalidAddressReturnsError pins that a connection
// failure at construction is reported as an error, never a panic.
func TestConnFromConfig_InvalidAddressReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := connFromConfig(pkgcore.Config{"url": "nats://127.0.0.1:1"}); err == nil {
		t.Fatal("connFromConfig() against an address nothing listens on succeeded, want an error")
	}
}
