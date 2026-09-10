package redis

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersKVRedisOnTheSharedRegistry proves this package's init()
// really lands "kv.redis" on pkgcore's shared KVStoreRegistry with the
// capability the distributed deployment mode requires -- the registration
// this package itself performs, verified from the consuming side. pkgcore's PresetDistributed already names this
// implementation for the "kv" seam (preset_test.go pins the name itself);
// this test is what proves the name actually resolves once this package is
// imported.
func TestInit_RegistersKVRedisOnTheSharedRegistry(t *testing.T) {
	impl, caps, err := pkgcore.KVStoreRegistry.Build("kv.redis", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.redis", err)
	}
	if impl == nil {
		t.Error("Build(\"kv.redis\") returned a nil KVStore")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "kv.redis", caps, want)
	}
	if caps != Capabilities {
		t.Errorf("Build(%q) capabilities = %v, want the exported Capabilities constant %v the host reads off this package", "kv.redis", caps, Capabilities)
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

// TestBuiltinClose_ReleasesTheDialedClient drives the registration's
// resource-ownership contract: the value Build hands back is the closable
// wrapper, and Close releases the client the registration itself built.
func TestBuiltinClose_ReleasesTheDialedClient(t *testing.T) {
	impl, _, err := pkgcore.KVStoreRegistry.Build("kv.redis", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.redis", err)
	}
	closable, ok := impl.(*closableKVStore)
	if !ok {
		t.Fatalf("Build(%q) returned %T, want the *closableKVStore whose Close releases the dialed client", "kv.redis", impl)
	}
	if err := closable.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}
