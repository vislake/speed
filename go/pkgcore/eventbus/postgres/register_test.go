package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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

// TestClosableEventBusClose_StopsTheBusAndRunsThePoolCloser drives the
// wrapper's resource-ownership contract directly (the registry's own New
// cannot run hermetically: it builds a pool for a live server): Close stops
// the bus -- a publish afterwards is refused with ErrEventBusClosed -- and
// runs the recorded pool closer exactly once; a wrapper with no closer still
// closes cleanly.
func TestClosableEventBusClose_StopsTheBusAndRunsThePoolCloser(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v, want nil", err)
	}
	defer pool.Close()

	closed := 0
	b := &closableEventBus{EventBus: NewEventBus(pool, "replica-1"), closePool: func() { closed++ }}
	if err := b.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if closed != 1 {
		t.Errorf("pool closer ran %d times, want exactly 1", closed)
	}
	if err := b.Publish(context.Background(), pkgcore.Event{Type: "some.event", Payload: "x"}); !errors.Is(err, ErrEventBusClosed) {
		t.Errorf("Publish after Close error = %v, want ErrEventBusClosed", err)
	}

	if err := (&closableEventBus{EventBus: NewEventBus(pool, "replica-2")}).Close(); err != nil {
		t.Errorf("Close() on a wrapper without a closer error = %v, want nil", err)
	}
}
