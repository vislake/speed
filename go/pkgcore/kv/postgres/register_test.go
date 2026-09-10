package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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
	if caps != Capabilities {
		t.Errorf("KVStoreRegistry.Build() capabilities = %v, want the exported Capabilities constant %v the host reads off this package", caps, Capabilities)
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

// TestClosableKVStoreClose_RunsThePoolCloser drives the wrapper's
// resource-ownership contract directly: Close runs the recorded pool closer
// exactly once, and a wrapper with no closer still closes cleanly.
func TestClosableKVStoreClose_RunsThePoolCloser(t *testing.T) {
	closed := 0
	s := &closableKVStore{closePool: func() { closed++ }}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if closed != 1 {
		t.Errorf("pool closer ran %d times, want exactly 1", closed)
	}

	if err := (&closableKVStore{}).Close(); err != nil {
		t.Errorf("Close() on a wrapper without a closer error = %v, want nil", err)
	}
}

// TestRegistration_WrapsTheHostBuiltPool pins the factory contract a host
// follows on the name-registration path: the Registration carries the name
// the host chose and the package's own exported Capabilities, and its New
// hands back the bare store over the host's pool -- no Close() error
// method, so Kernel.Shutdown leaves the pool and the store to the host, the
// same ownership pkgcore.WithKVStore records. New ignores a preset entry's
// Config: the empty Config below would fail poolFromConfig if the factory
// consulted it.
func TestRegistration_WrapsTheHostBuiltPool(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v, want nil (construction dials nothing)", err)
	}
	t.Cleanup(pool.Close)

	r := Registration("kv.postgres.host", pool)
	if r.Name != "kv.postgres.host" {
		t.Errorf("Registration().Name = %q, want the host-chosen name", r.Name)
	}
	if r.Capabilities != Capabilities {
		t.Errorf("Registration().Capabilities = %v, want the exported constant %v", r.Capabilities, Capabilities)
	}

	impl, err := r.New(pkgcore.Config{})
	if err != nil {
		t.Fatalf("Registration().New() error = %v, want nil: the factory ignores the preset entry's Config", err)
	}
	if impl == nil {
		t.Fatal("Registration().New() returned a nil KVStore")
	}
	if _, ok := impl.(interface{ Close() error }); ok {
		t.Error("Registration().New() returned a value carrying Close() error, want the bare store the host owns")
	}
}
