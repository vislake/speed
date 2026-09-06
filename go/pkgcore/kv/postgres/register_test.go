package postgres

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersKVPostgres pins that importing this package registers
// "kv.postgres" on pkgcore's shared KVStoreRegistry with the capabilities
// its own doc comment promises, mirroring kv/redis/register_test.go's and
// eventbus/postgres/register_test.go's identical shape.
func TestInit_RegistersKVPostgres(t *testing.T) {
	store, caps, err := pkgcore.KVStoreRegistry.Build("kv.postgres", pkgcore.Config{
		"dsn": "postgres://user:pass@127.0.0.1:1/db",
	})
	if err != nil {
		t.Fatalf("KVStoreRegistry.Build(%q) error = %v, want nil", "kv.postgres", err)
	}
	if store == nil {
		t.Fatal("KVStoreRegistry.Build() returned a nil store")
	}
	want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	if caps != want {
		t.Errorf("KVStoreRegistry.Build() capabilities = %v, want %v", caps, want)
	}
}

// TestPoolFromConfig_MissingDSN pins that a missing "dsn" config key is
// refused explicitly rather than silently falling back to some default --
// unlike kv/redis's clientFromConfig, PostgreSQL has no universal safe
// default DSN.
func TestPoolFromConfig_MissingDSN(t *testing.T) {
	_, err := poolFromConfig(pkgcore.Config{})
	if err == nil {
		t.Fatal("poolFromConfig() with no \"dsn\" error = nil, want a non-nil error")
	}
}
