//go:build integration

package config_test

// The Redis leg of go/config's integration tier: two Service instances --
// each with its own RedisEventBus over one shared Redis client and one
// shared PostgreSQL connection -- stand in for two replicas of a
// distributed deployment. A write on one replica must converge the other
// through the bus, not through the poller (which both services disable via
// WithPollInterval(0)): the anti-loss sweep exists as a backstop, and the
// tests here prove the event path converges on its own, over the bus's
// real wire format. go/pkgcore's integration tier pins that the wire
// format is JSON reconstructed into a plain map on the remote side; the
// config layer below it must therefore recover ItemChangedEvent from the
// map shape (events.go's itemChangedFromWire), which is exactly what the
// peer's cache invalidation runs on.
//
// Container lifecycle follows go/pkgcore/integration_test/
// redis_container_test.go's startRedisClient almost line for line.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
)

// startRedisClient starts a disposable Redis 7 container and returns a
// go-redis client connected to it; the client and the container are torn
// down through t.Cleanup. Both replicas in a test share this one client
// (they are two bus instances over the same server, like two processes
// over one Redis).
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
	t.Cleanup(func() { client.Close() })
	return client
}

// newRedisPeerPair returns the two replicas of one test: two Services,
// each attached over its own RedisEventBus, sharing the PostgreSQL
// connection and the cipher key. The cleanup closes the buses (each Close
// destroys its own consumer group, leaving nothing behind on the shared
// server but the stream the container takes with it).
func newRedisPeerPair(t *testing.T, ctx context.Context, client *redis.Client) (*config.Service, *config.Service, pkgcore.EventBus, pkgcore.EventBus) {
	t.Helper()

	db := openConfigPostgres(t, ctx, startPostgresContainer(t, ctx))
	cipher := pgCipher(t)
	busA := pkgcore.NewRedisEventBus(client)
	busB := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})
	svcA := attachConfigService(t, db, busA, cipher)
	svcB := attachConfigService(t, db, busB, cipher)
	return svcA, svcB, busA, busB
}

// remoteEventSpy records every Event a bus delivers to it, so a test can
// assert on the wire shape the remote side actually receives. It is
// registered on the peer bus in addition to the service's own subscriber.
type remoteEventSpy struct {
	mu     sync.Mutex
	events []pkgcore.Event
}

func newRemoteEventSpy() *remoteEventSpy { return &remoteEventSpy{} }

// handler returns an EventHandler fit for pkgcore.EventBus.Subscribe.
func (s *remoteEventSpy) handler() pkgcore.EventHandler {
	return func(_ context.Context, evt pkgcore.Event) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.events = append(s.events, evt)
		return nil
	}
}

func (s *remoteEventSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// countMatching counts the received events match reports true for. The
// assertions below match on payload content (the changed key's values)
// rather than on total counts, because a warm-up marker can still be in
// flight after warmUpMarker returned: content matching is immune to that
// residue, exact counting is not.
func (s *remoteEventSpy) countMatching(match func(pkgcore.Event) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, evt := range s.events {
		if match(evt) {
			n++
		}
	}
	return n
}

// first returns the earliest received event match reports true for.
func (s *remoteEventSpy) first(match func(pkgcore.Event) bool) (pkgcore.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if match(evt) {
			return evt, true
		}
	}
	return pkgcore.Event{}, false
}

// last returns the most recently received event match reports true for.
func (s *remoteEventSpy) last(match func(pkgcore.Event) bool) (pkgcore.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		if match(s.events[i]) {
			return s.events[i], true
		}
	}
	return pkgcore.Event{}, false
}

// requireRemoteItemEvent asserts that a remotely received Event carries
// the config.item.changed type and the JSON-reconstructed map shape --
// never the original struct, which cannot cross the bus -- with the fields
// an invalidation (and the audit trail) depends on.
func requireRemoteItemEvent(t *testing.T, evt pkgcore.Event, wantKey, wantScope string, wantTenant, wantOld, wantNew string, wantSensitive bool) {
	t.Helper()
	if evt.Type != config.EventConfigItemChanged {
		t.Errorf("remote event type = %q, want %q", evt.Type, config.EventConfigItemChanged)
	}
	payload, ok := evt.Payload.(map[string]any)
	if !ok {
		t.Fatalf("remote event payload is %T, want map[string]any (the JSON shape)", evt.Payload)
	}
	if payload["Key"] != wantKey {
		t.Errorf("remote event key = %v, want %q", payload["Key"], wantKey)
	}
	if payload["Scope"] != wantScope {
		t.Errorf("remote event scope = %v, want %q", payload["Scope"], wantScope)
	}
	if payload["TenantID"] != wantTenant {
		t.Errorf("remote event tenant = %v, want %q", payload["TenantID"], wantTenant)
	}
	if payload["OldValue"] != wantOld {
		t.Errorf("remote event old value = %v, want %q", payload["OldValue"], wantOld)
	}
	if payload["NewValue"] != wantNew {
		t.Errorf("remote event new value = %v, want %q", payload["NewValue"], wantNew)
	}
	if payload["Sensitive"] != wantSensitive {
		t.Errorf("remote event sensitive flag = %v, want %v", payload["Sensitive"], wantSensitive)
	}
}

// eventually polls cond until it holds or the deadline passes. Remote
// delivery over Redis is asynchronous -- a reader must wake and claim the
// entry -- so every assertion on the far side of the bus waits through
// this helper rather than assuming the delivery landed with the publish.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// warmUpMarker loops marker publishes on busA until the peer spy on busB
// has received one. A reader's consumer group is created at the stream's
// live end ("$" -- see pkgcore's createGroup), so an entry appended before
// the group existed is never replayed; whether the very first publish wins
// the race against the reader goroutine's group creation is scheduling
// luck, so the marker is republished until one is demonstrably delivered.
// This mirrors go/pkgcore's integration tier warmUp, which exists for the
// same reason. The marker is a change of key at a tenant nobody reads, so
// it is harmless to both services.
func warmUpMarker(t *testing.T, busA pkgcore.EventBus, spy *remoteEventSpy, key string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for published := 1; ; published++ {
		if err := busA.Publish(context.Background(), pkgcore.Event{
			Type: config.EventConfigItemChanged, TenantID: pkgcore.TenantID("warmup"),
			Payload: config.ItemChangedEvent{Key: key, Scope: config.ScopeTenant, TenantID: "warmup"},
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

// changedTo matches a remotely received item-change event whose payload
// reports the key and the new value, in the JSON map shape every remote
// delivery carries.
func changedTo(key, newValue string) func(pkgcore.Event) bool {
	return func(evt pkgcore.Event) bool {
		if evt.Type != config.EventConfigItemChanged {
			return false
		}
		payload, ok := evt.Payload.(map[string]any)
		if !ok {
			return false
		}
		return payload["Key"] == key && payload["NewValue"] == newValue
	}
}

// TestRedisBus_RemoteSet_ConvergesThePeer is the distributed-mode hot
// update, end to end: replica A writes a tenant override; replica B --
// which already served the old value from its own cache -- must converge
// to the new one through the remote invalidation, with its poller disabled
// so nothing but the bus could have done it. Without the wire-map recovery
// in events.go the remote event would be dropped as a foreign payload and
// B would serve the stale value until this test's deadline.
func TestRedisBus_RemoteSet_ConvergesThePeer(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	svcA, svcB, busA, busB := newRedisPeerPair(t, ctx, client)

	// The spy rides the receiving bus: svcA publishes through busA, whose
	// own handlers see the concrete struct synchronously, while the peer
	// bus B receives the JSON map shape this test must assert on.
	spy := newRemoteEventSpy()
	busB.Subscribe(config.EventConfigItemChanged, spy.handler())

	// Warm the delivery path before the counted events (warmUpMarker): the
	// peer's consumer group must exist before a publish or the event lands
	// ahead of the group's live end and is never replayed. The spy is then
	// asserted on content, never on a total count: a marker can still be in
	// flight when warmUpMarker returns, and the two writes below differ by
	// their value, which makes the assertions immune to that residue.
	warmUpMarker(t, busA, spy, "brand.site_name")

	// Replica A writes the tenant override; replica B's first read may
	// still race the delivery, so it is allowed to reach the value through
	// the database -- that read is what warms B's cache with the old value.
	if err := svcA.Set(tenantContext("tenant-a"), config.ScopeTenant, "brand.site_name", config.Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("svcA.Set(Studio A): %v", err)
	}
	eventually(t, "replica B to converge to the first write", func() bool {
		v, err := svcB.Get(tenantContext("tenant-a"), "brand.site_name")
		return err == nil && v.Data == "Studio A"
	})
	eventually(t, "the peer spy to record the first write", func() bool {
		return spy.countMatching(changedTo("brand.site_name", "Studio A")) == 1
	})
	firstWrite, ok := spy.first(changedTo("brand.site_name", "Studio A"))
	if !ok {
		t.Fatal("the peer spy never recorded the first write")
	}
	requireRemoteItemEvent(t, firstWrite, "brand.site_name", "tenant", "tenant-a", "", "Studio A", false)

	// Replica A changes the value again. Replica B's cache holds Studio A;
	// only the remote invalidation can move it to Studio A2 within the
	// deadline, because B's poller is disabled (WithPollInterval(0) in
	// attachConfigService).
	if err := svcA.Set(tenantContext("tenant-a"), config.ScopeTenant, "brand.site_name", config.Value{Data: "Studio A2"}, "alice"); err != nil {
		t.Fatalf("svcA.Set(Studio A2): %v", err)
	}
	eventually(t, "replica B to converge to the second write", func() bool {
		v, err := svcB.Get(tenantContext("tenant-a"), "brand.site_name")
		return err == nil && v.Data == "Studio A2"
	})
	eventually(t, "the peer spy to record the second write", func() bool {
		return spy.countMatching(changedTo("brand.site_name", "Studio A2")) == 1
	})
	secondWrite, ok := spy.first(changedTo("brand.site_name", "Studio A2"))
	if !ok {
		t.Fatal("the peer spy never recorded the second write")
	}
	requireRemoteItemEvent(t, secondWrite, "brand.site_name", "tenant", "tenant-a", "Studio A", "Studio A2", false)
}

// TestRedisBus_RemoteSensitiveChange_CarriesOnlyTheMarker pins the
// redaction discipline on the real bus: when replica A changes a Sensitive
// item, the payload replica B receives carries the marker in place of both
// values and the plaintext nowhere -- not in the field, and not anywhere
// in the serialized payload. B still converges: its invalidation drops the
// cache entry and the next read decrypts the new value from the shared
// database in process.
func TestRedisBus_RemoteSensitiveChange_CarriesOnlyTheMarker(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	svcA, svcB, busA, busB := newRedisPeerPair(t, ctx, client)

	spy := newRemoteEventSpy()
	busB.Subscribe(config.EventConfigItemChanged, spy.handler())

	// A change of a Sensitive key is only distinguishable from a warm-up
	// marker by its flag: the marker struct publishes Sensitive as false,
	// every real change of this item publishes it as true. Counting the
	// true-flagged events is therefore immune to in-flight marker residue,
	// exactly like changedTo in the non-sensitive test above.
	sensitiveChange := func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(map[string]any)
		return ok && payload["Sensitive"] == true
	}

	warmUpMarker(t, busA, spy, "support.reply_email")

	// Two changes, so the second event's OldValue is the first plaintext --
	// which must also arrive redacted.
	if err := svcA.Set(tenantContext("tenant-a"), config.ScopeTenant, "support.reply_email", config.Value{Data: "first@example.com"}, "alice"); err != nil {
		t.Fatalf("svcA.Set(first): %v", err)
	}
	eventually(t, "replica B to serve the first sensitive value", func() bool {
		v, err := svcB.Get(tenantContext("tenant-a"), "support.reply_email")
		return err == nil && v.Data == "first@example.com" && !v.Redacted
	})

	if err := svcA.Set(tenantContext("tenant-a"), config.ScopeTenant, "support.reply_email", config.Value{Data: "second@example.com"}, "alice"); err != nil {
		t.Fatalf("svcA.Set(second): %v", err)
	}
	eventually(t, "replica B to converge to the second sensitive value", func() bool {
		v, err := svcB.Get(tenantContext("tenant-a"), "support.reply_email")
		return err == nil && v.Data == "second@example.com" && !v.Redacted
	})

	// Both changes must reach the peer in their redacted wire form. The last
	// true-flagged event is the second change, whose OldValue -- the first
	// plaintext -- must carry the marker like the NewValue.
	eventually(t, "the peer spy to record both sensitive changes", func() bool {
		return spy.countMatching(sensitiveChange) == 2
	})
	secondChange, ok := spy.last(sensitiveChange)
	if !ok {
		t.Fatal("the peer spy never recorded a sensitive change")
	}
	requireRemoteItemEvent(t, secondChange, "support.reply_email", "tenant", "tenant-a", "[redacted]", "[redacted]", true)

	payload, ok := secondChange.Payload.(map[string]any)
	if !ok {
		t.Fatalf("remote event payload is %T, want map[string]any", secondChange.Payload)
	}
	serialized, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(payload): %v", err)
	}
	for _, plaintext := range []string{"first@example.com", "second@example.com"} {
		if strings.Contains(string(serialized), plaintext) {
			t.Fatalf("the serialized remote payload contains the plaintext %q; a Sensitive value must never cross the bus", plaintext)
		}
	}
}
