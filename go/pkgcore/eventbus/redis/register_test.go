package redis

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersEventBusRedisOnTheSharedRegistry proves this package's
// init() really lands "eventbus.redis" on pkgcore's shared EventBusRegistry
// with the capability the distributed deployment mode requires -- the same
// assertion pkgcore's own builtin_implementations_test.go made before this
// implementation moved out of its package, now owned by the package that
// performs the registration. pkgcore's PresetDistributed already names this
// implementation for the "eventbus" seam (preset_test.go pins the name
// itself); this test is what proves the name actually resolves once this
// package is imported.
func TestInit_RegistersEventBusRedisOnTheSharedRegistry(t *testing.T) {
	impl, caps, err := pkgcore.EventBusRegistry.Build("eventbus.redis", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.redis", err)
	}
	if impl == nil {
		t.Error("Build(\"eventbus.redis\") returned a nil EventBus")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "eventbus.redis", caps, want)
	}
}

// TestClientFromConfig_DefaultsAddrWhenUnset pins the fallback address a
// zero-configuration Preset relies on.
func TestClientFromConfig_DefaultsAddrWhenUnset(t *testing.T) {
	client, err := clientFromConfig(pkgcore.Config{})
	if err != nil {
		t.Fatalf("clientFromConfig(Config{}) error = %v, want nil", err)
	}
	if got := client.Options().Addr; got != "localhost:6379" {
		t.Errorf("client.Options().Addr = %q, want %q", got, "localhost:6379")
	}
}

// TestClientFromConfig_InvalidDBReturnsError pins that a malformed "db"
// value is rejected rather than silently defaulting.
func TestClientFromConfig_InvalidDBReturnsError(t *testing.T) {
	_, err := clientFromConfig(pkgcore.Config{"db": "not-a-number"})
	if err == nil {
		t.Fatal("clientFromConfig() with an invalid db succeeded, want an error")
	}
}
