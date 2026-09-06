package postgres

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersEventBusPostgres pins that importing this package
// registers "eventbus.postgres" on pkgcore's shared EventBusRegistry with
// the capabilities its own doc comment promises, mirroring
// eventbus/redis/register_test.go's identical shape.
func TestInit_RegistersEventBusPostgres(t *testing.T) {
	bus, caps, err := pkgcore.EventBusRegistry.Build("eventbus.postgres", pkgcore.Config{
		"dsn":        "postgres://user:pass@127.0.0.1:1/db",
		"replica_id": "replica-1",
	})
	if err != nil {
		t.Fatalf("EventBusRegistry.Build(%q) error = %v, want nil", "eventbus.postgres", err)
	}
	if bus == nil {
		t.Fatal("EventBusRegistry.Build() returned a nil bus")
	}
	want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	if caps != want {
		t.Errorf("EventBusRegistry.Build() capabilities = %v, want %v", caps, want)
	}
}

// TestPoolAndReplicaFromConfig_MissingDSN pins that a missing "dsn" config
// key is refused explicitly rather than silently falling back to some
// default -- unlike eventbus/redis's clientFromConfig, PostgreSQL has no
// universal safe default DSN.
func TestPoolAndReplicaFromConfig_MissingDSN(t *testing.T) {
	_, _, err := poolAndReplicaFromConfig(pkgcore.Config{"replica_id": "replica-1"})
	if err == nil {
		t.Fatal("poolAndReplicaFromConfig() with no \"dsn\" error = nil, want a non-nil error")
	}
}

// TestPoolAndReplicaFromConfig_MissingReplicaID pins that a missing
// "replica_id" config key is refused explicitly rather than silently
// generating a random one, which would defeat the cross-restart durability
// guarantee this implementation exists to provide (see EventBus's own doc
// comment).
func TestPoolAndReplicaFromConfig_MissingReplicaID(t *testing.T) {
	_, _, err := poolAndReplicaFromConfig(pkgcore.Config{"dsn": "postgres://user:pass@127.0.0.1:1/db"})
	if err == nil {
		t.Fatal("poolAndReplicaFromConfig() with no \"replica_id\" error = nil, want a non-nil error")
	}
}
