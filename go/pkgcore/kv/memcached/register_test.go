package memcached

import (
	"testing"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersKVMemcachedOnTheSharedRegistry proves this package's
// init() lands "kv.memcached" on pkgcore's shared KVStoreRegistry with the
// honest capability bits this implementation actually has -- MultiReplicaSafe
// alone, deliberately never SurvivesRestart (see the package doc comment).
// Unlike kv/redis's "kv.redis", no built-in Preset names this implementation
// (preset_test.go pins PresetDistributed's "kv" entry as "kv.redis", not
// this one), so this test is the only place that proves the name resolves at
// all once this package is imported.
func TestInit_RegistersKVMemcachedOnTheSharedRegistry(t *testing.T) {
	impl, caps, err := pkgcore.KVStoreRegistry.Build("kv.memcached", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.memcached", err)
	}
	if impl == nil {
		t.Error("Build(\"kv.memcached\") returned a nil KVStore")
	}
	if want := pkgcore.MultiReplicaSafe; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v (never SurvivesRestart)", "kv.memcached", caps, want)
	}
	if caps != Capabilities {
		t.Errorf("Build(%q) capabilities = %v, want the exported Capabilities constant %v the host reads off this package", "kv.memcached", caps, Capabilities)
	}
}

// TestClientFromConfig_DefaultsAddrsWhenUnset pins the fallback address a
// zero-configuration build relies on.
func TestClientFromConfig_DefaultsAddrsWhenUnset(t *testing.T) {
	client, err := clientFromConfig(pkgcore.Config{})
	if err != nil {
		t.Fatalf("clientFromConfig(Config{}) error = %v, want nil", err)
	}
	if client == nil {
		t.Fatal("clientFromConfig(Config{}) returned a nil client")
	}
}

// TestClientFromConfig_SplitsCommaSeparatedAddrs pins that multiple servers
// can be configured for gomemcache's own rendezvous-hashing ServerList.
func TestClientFromConfig_SplitsCommaSeparatedAddrs(t *testing.T) {
	client, err := clientFromConfig(pkgcore.Config{"addrs": "10.0.0.1:11211, 10.0.0.2:11211"})
	if err != nil {
		t.Fatalf("clientFromConfig() error = %v, want nil", err)
	}
	if client == nil {
		t.Fatal("clientFromConfig() returned a nil client")
	}
}

// TestBuiltinClose_ReleasesTheClientItBuilt drives the registration's
// resource-ownership contract for the built-in "kv.memcached" seam: the
// value Build hands back must carry the Close() error method that releases
// the client the registration itself built from cfg, so Kernel.Bootstrap
// records it and runs it -- at Shutdown, and on Bootstrap's own failure
// path. Without a closer on the registration-built value, nothing anywhere
// could release that client's pooled connections: the host never saw it.
func TestBuiltinClose_ReleasesTheClientItBuilt(t *testing.T) {
	impl, _, err := pkgcore.KVStoreRegistry.Build("kv.memcached", pkgcore.Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.memcached", err)
	}
	closer, ok := impl.(interface{ Close() error })
	if !ok {
		t.Fatalf("Build(%q) returned %T, want a value whose Close() error releases the client the registration built", "kv.memcached", impl)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}

// TestRegistration_WrapsTheHostBuiltClient pins the factory contract a host
// follows on the name-registration path: the Registration carries the name
// the host chose and the package's own exported Capabilities, and its New
// hands back the bare store over the host's client -- no Close() error
// method, so Kernel.Shutdown leaves the client and the store to the host,
// the same ownership pkgcore.WithKVStore records.
func TestRegistration_WrapsTheHostBuiltClient(t *testing.T) {
	client := memcache.New("127.0.0.1:1")

	r := Registration("kv.memcached.host", client)
	if r.Name != "kv.memcached.host" {
		t.Errorf("Registration().Name = %q, want the host-chosen name", r.Name)
	}
	if r.Capabilities != Capabilities {
		t.Errorf("Registration().Capabilities = %v, want the exported constant %v", r.Capabilities, Capabilities)
	}

	impl, err := r.New(pkgcore.Config{})
	if err != nil {
		t.Fatalf("Registration().New() error = %v, want nil", err)
	}
	if impl == nil {
		t.Fatal("Registration().New() returned a nil KVStore")
	}
	if _, ok := impl.(interface{ Close() error }); ok {
		t.Error("Registration().New() returned a value carrying Close() error, want the bare store the host owns")
	}
}
