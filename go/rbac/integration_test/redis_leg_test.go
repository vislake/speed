//go:build integration

package rbac_test

// The Redis leg of go/rbac's integration tier: two Service instances --
// each with its own RedisEventBus over one shared Redis client and one
// shared PostgreSQL connection -- stand in for two replicas of a
// distributed deployment.
//
// Why this leg is not optional. rbac caches flattened grant sets per
// subject, and unlike go/config it has NO poller behind the events: the
// only two ways a replica learns that a grant changed are the bus and the
// anti-loss TTL. Both services here are attached with a one-hour TTL
// (attachRBACService), so nothing in this file can pass because an entry
// expired -- every convergence proven below is the bus's doing. A stale
// authorization decision is a security failure, not a performance one: a
// revoked administrator who keeps their access until a cache lifetime
// elapses is exactly the outcome an access-control system exists to
// prevent, and the standalone in-memory bus cannot demonstrate that the
// distributed path avoids it.
//
// The wire format is the second reason. go/pkgcore's own integration tier
// pins that the Redis bus reconstructs a remote payload as JSON in a plain
// map rather than the original struct, which never crosses the wire. rbac's
// invalidation therefore has to recover its event structs from the map
// shape (events.go's roleBindingChangedFromWire / roleChangedFromWire);
// without that recovery the remote event is dropped as a foreign payload
// and the peer serves the revoked grant until its TTL. The spy assertion
// below pins the shape the recovery is written against.
//
// Container lifecycle follows go/config/integration_test/redis_leg_test.go,
// which follows go/pkgcore/integration_test's startRedisClient.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// convergenceDeadline bounds every wait for a remote delivery. Redis
// delivery is asynchronous -- a reader goroutine must wake and claim the
// stream entry -- so assertions on the far side of the bus poll rather
// than assume the delivery landed with the publish.
const convergenceDeadline = 10 * time.Second

// startRedisClient starts a disposable Redis 7 container and returns a
// go-redis client connected to it; both are torn down through t.Cleanup.
// The two replicas of a test share this one client -- they are two bus
// instances over the same server, exactly as two processes would be.
func startRedisClient(t *testing.T, ctx context.Context) *redis.Client {
	t.Helper()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	options, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("redis.ParseURL(%q): %v", uri, err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// replicas returns the two Services of one test: each over its own
// RedisEventBus, sharing the PostgreSQL connection the way two replicas
// share a database. The peer bus is returned too, so a test can subscribe
// a spy alongside the Service's own invalidation handler.
func replicas(t *testing.T, ctx context.Context) (writer, peer *rbac.Service, writerBus, peerBus *pkgcore.RedisEventBus) {
	t.Helper()

	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	client := startRedisClient(t, ctx)
	writerBus = pkgcore.NewRedisEventBus(client)
	peerBus = pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		writerBus.Close()
		peerBus.Close()
	})
	return attachRBACService(t, db, writerBus), attachRBACService(t, db, peerBus), writerBus, peerBus
}

// eventSpy records every Event a bus delivers to it, so a test can assert
// on the wire shape the remote side actually receives.
type eventSpy struct {
	mu     sync.Mutex
	events []pkgcore.Event
}

func (s *eventSpy) handler() pkgcore.EventHandler {
	return func(_ context.Context, evt pkgcore.Event) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.events = append(s.events, evt)
		return nil
	}
}

func (s *eventSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// first returns the earliest received event match reports true for.
func (s *eventSpy) first(match func(pkgcore.Event) bool) (pkgcore.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if match(evt) {
			return evt, true
		}
	}
	return pkgcore.Event{}, false
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(convergenceDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// warmUp loops marker publishes on bus until spy has received one.
//
// A reader's consumer group is created at the stream's live end, so an
// entry appended before the group existed is never replayed; whether the
// very first publish wins the race against the reader goroutine's group
// creation is scheduling luck. The marker is therefore republished until
// one is demonstrably delivered, mirroring go/config's and go/pkgcore's
// tiers. It names a tenant no test reads, so it invalidates nothing real.
func warmUp(t *testing.T, bus pkgcore.EventBus, spy *eventSpy) {
	t.Helper()
	deadline := time.Now().Add(convergenceDeadline)
	for published := 1; ; published++ {
		if err := bus.Publish(context.Background(), pkgcore.Event{
			Type:     rbac.EventRoleChanged,
			TenantID: "warmup",
			Payload:  rbac.RoleChangedEvent{TenantID: "warmup", RoleKey: "warmup"},
		}); err != nil {
			t.Fatalf("warm-up publish: %v", err)
		}
		if spy.count() >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer never received a warm-up marker after %d publishes", published)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// canRead is the decision every test below watches, wrapped so a polling
// assertion reads as one predicate. A decision error fails the test rather
// than being folded into "denied", so a broken read is never mistaken for
// a converged revocation.
func canRead(t *testing.T, svc *rbac.Service, sub rbac.Subject, resource, action string) bool {
	t.Helper()
	allowed, err := svc.Can(context.Background(), sub, action, resource)
	if err != nil {
		t.Fatalf("Can(%s:%s): %v", resource, action, err)
	}
	return allowed
}

// TestRedisBus_RevokeOnOneReplica_ConvergesTheOther is the security case
// this leg exists for: replica B has already cached a live grant, replica
// A revokes it, and B must stop granting it without waiting for its cache
// lifetime -- which is an hour here, so only the bus can explain the
// change.
func TestRedisBus_RevokeOnOneReplica_ConvergesTheOther(t *testing.T) {
	ctx := context.Background()
	writer, peer, writerBus, peerBus := replicas(t, ctx)

	spy := &eventSpy{}
	peerBus.Subscribe(rbac.EventRoleChanged, spy.handler())
	warmUp(t, writerBus, spy)

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := writer.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if err := writer.AssignRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	// The peer reads the grant and caches it. Everything after this point
	// must beat that cached entry, not outlive it.
	eventually(t, "the peer to observe the new grant", func() bool {
		return canRead(t, peer, sub, "notes", "read")
	})

	if err := writer.RevokeRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	eventually(t, "the peer to stop granting the revoked permission", func() bool {
		return !canRead(t, peer, sub, "notes", "read")
	})
}

// TestRedisBus_AssignOnOneReplica_ConvergesTheOther is the widening
// direction, which matters for a different reason: a user who was just
// granted access and is told they have it must not be refused by whichever
// replica happens to hold their negative decision.
func TestRedisBus_AssignOnOneReplica_ConvergesTheOther(t *testing.T) {
	ctx := context.Background()
	writer, peer, writerBus, peerBus := replicas(t, ctx)

	spy := &eventSpy{}
	peerBus.Subscribe(rbac.EventRoleChanged, spy.handler())
	warmUp(t, writerBus, spy)

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := writer.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}

	// The peer caches the denial first.
	if canRead(t, peer, sub, "notes", "read") {
		t.Fatal("the subject holds the permission before anything was assigned")
	}

	if err := writer.AssignRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	eventually(t, "the peer to observe the new grant", func() bool {
		return canRead(t, peer, sub, "notes", "read")
	})
}

// TestRedisBus_RolePermissionChange_ConvergesTheOther covers the other
// invalidation path: a change to a ROLE's permission set, which widens or
// narrows every subject bound to it at once, and which the peer must apply
// tenant-wide rather than per subject.
//
// The role is widened by EnsureBuiltinRoles reconciling an "owner" that
// was first created with a single permission -- the reconciliation that
// exists so a tenant created before a module was added to the build still
// gets an owner who can use it.
func TestRedisBus_RolePermissionChange_ConvergesTheOther(t *testing.T) {
	ctx := context.Background()
	writer, peer, writerBus, peerBus := replicas(t, ctx)

	spy := &eventSpy{}
	peerBus.Subscribe(rbac.EventRoleChanged, spy.handler())
	warmUp(t, writerBus, spy)

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := writer.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "owner",
		DescriptionKey: "rbac.role.owner",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if err := writer.AssignRole(tenantCtx, sub, "owner", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	// The peer caches a grant set that holds notes:read and NOT
	// billing:manage.
	eventually(t, "the peer to observe the narrow grant", func() bool {
		return canRead(t, peer, sub, "notes", "read")
	})
	if canRead(t, peer, sub, "billing", "manage") {
		t.Fatal("the narrow owner role already grants billing:manage")
	}

	// Reconciliation widens "owner" to the whole frozen catalog.
	if err := writer.EnsureBuiltinRoles(tenantCtx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}
	eventually(t, "the peer to observe the widened role", func() bool {
		return canRead(t, peer, sub, "billing", "manage")
	})
}

// TestRedisBus_RemoteEvent_ArrivesAsAJSONMap pins the wire shape rbac's
// event decoding is written against. The original struct never crosses the
// bus: the remote side reconstructs the payload as a plain map, so a
// handler that type-asserted only the struct would silently drop every
// remote invalidation -- and the failure mode of that bug is a replica
// serving revoked access, which no unit test over the in-memory bus can
// reproduce.
func TestRedisBus_RemoteEvent_ArrivesAsAJSONMap(t *testing.T) {
	ctx := context.Background()
	writer, _, writerBus, peerBus := replicas(t, ctx)

	spy := &eventSpy{}
	peerBus.Subscribe(rbac.EventRoleBindingAssigned, spy.handler())
	warmUpSpy := &eventSpy{}
	peerBus.Subscribe(rbac.EventRoleChanged, warmUpSpy.handler())
	warmUp(t, writerBus, warmUpSpy)

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := writer.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if err := writer.AssignRole(tenantCtx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	isAssignment := func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(map[string]any)
		return ok && payload["UserID"] == "user-1"
	}
	var received pkgcore.Event
	eventually(t, "the assignment event to reach the peer bus", func() bool {
		evt, ok := spy.first(isAssignment)
		received = evt
		return ok
	})

	if received.Type != rbac.EventRoleBindingAssigned {
		t.Errorf("remote event type = %q, want %q", received.Type, rbac.EventRoleBindingAssigned)
	}
	payload, ok := received.Payload.(map[string]any)
	if !ok {
		t.Fatalf("remote event payload is %T, want map[string]any (the JSON shape)", received.Payload)
	}
	// The two fields the peer's invalidation keys on. A payload missing
	// either of them cannot invalidate the right cache entry, so their
	// presence is what the decoding contract actually needs.
	if payload["TenantID"] != "tenant-a" {
		t.Errorf("remote payload TenantID = %v, want %q", payload["TenantID"], "tenant-a")
	}
	if payload["UserID"] != "user-1" {
		t.Errorf("remote payload UserID = %v, want %q", payload["UserID"], "user-1")
	}
}
