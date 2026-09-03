//go:build integration

package notification_test

// The Redis leg of go/notification's integration tier: two booted modules
// -- each with its own pkgcore RedisEventBus over one shared Redis client
// and one shared database -- stand in for two replicas of a distributed
// deployment, and the test proves that a delivery written on one replica
// announces its inbox row on the bus the other replica reads.
//
// Why this leg is not optional. The module's own Hub subscribes to
// EventInboxCreated during Register, so an in-process bus (the standalone
// kernel's default) can demonstrate that a delivery announces on the
// replica that performed it -- nothing more. A real product's second
// replica learns about a delivery entirely through the distributed bus:
// every replica runs its own Hub, and a Hub with no announcement to read
// is a silent inbox. The Redis Streams bus is the only path that can prove
// the announcement survives the wire, and the wire is the second reason
// this leg exists: the remote side reconstructs the payload as a plain
// JSON map (never the original struct -- go/pkgcore's own tier pins that),
// so the test asserts the payload's four wire keys by the names the event
// contract spells out (events.go's json tags), which is exactly what any
// cross-replica consumer of the event -- this module's Hub included, and a
// future mirror -- must key on.
//
// The delivery driven here is a real one, not a direct announce: the
// fixture "clinic" module (clinic/, booted only on the writer)
// declares an appointment-reminder notification type with an in-app-only
// default channel and ships its copy in its own locale bundle. A Dispatch
// on the writer therefore exercises the whole pipeline -- the merged
// catalog's render of a DECLARING module's copy (the fixture type is never
// declared by notification itself), the inbox row write, and the announce
// -- and the row read back after the remote event arrives is the proof
// that the payload names a row the receiving side can read. The in-app
// channel needs no transport and no address resolution, so the fixture
// type's delivery needs none of the tier's senders; that is exactly the
// isolation a distributed-delivery proof wants.
//
// Container lifecycle follows go/rbac/integration_test's
// redis_leg_test.go, which follows go/config's, which follows
// go/pkgcore/integration_test's startRedisClient.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/integration_test/clinic"
	"github.com/vislake/speed/go/notification/internal/testutil"
	"github.com/vislake/speed/go/notification/migrations"
	"github.com/vislake/speed/go/pkgcore"
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
// one is demonstrably delivered, mirroring go/rbac's, go/config's and
// go/pkgcore's tiers. It must travel under the very event type the spy
// subscribes to (the bus streams by type), it names a tenant no test
// reads, and its payload is a well-formed InboxCreatedPayload so the
// writer replica's own Hub -- subscribed to the same type -- handles it
// exactly as it handles a real announcement.
func warmUp(t *testing.T, bus pkgcore.EventBus, spy *eventSpy) {
	t.Helper()
	deadline := time.Now().Add(convergenceDeadline)
	for published := 1; ; published++ {
		if err := bus.Publish(context.Background(), pkgcore.Event{
			Type:     notification.EventInboxCreated,
			TenantID: "warmup",
			Payload: notification.InboxCreatedPayload{
				MessageID:       "warmup",
				TenantID:        "warmup",
				RecipientUserID: "warmup",
				TypeKey:         "warmup",
			},
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

// bootReplica boots one notification module as one replica: over db, with
// the six required seams (the SMS sender writing to io.Discard -- no test
// here sends an SMS -- the tier's indexers, a recording stubQueue the test
// reads the delivery job back from, and a resolver answering no addresses,
// which the in-app-only fixture type never consults), through a kernel
// whose event bus is bus. extraModules join the writer's boot, so the
// clinic fixture module (its Register declaring the appointment-reminder
// type) rides the same kernel as the module that delivers its type -- the
// shape a real host assembles. The peer replica boots without them: it
// only needs to run the module's own subscription machinery on its bus.
func bootReplica(t *testing.T, ctx context.Context, db *gorm.DB, bus *pkgcore.RedisEventBus, extraModules ...pkgcore.Module) (*notification.Module, *stubQueue) {
	t.Helper()

	queue := &stubQueue{}
	module := notification.NewModule(db,
		notification.WithSMSSender(notification.NewConsoleSMSSender(io.Discard)),
		notification.WithMailFrom(testMailFrom),
		notification.WithContactEmailIndexer(emailIndexer(t)),
		notification.WithContactPhoneIndexer(phoneIndexer(t)),
		notification.WithDeliveryQueue(queue),
		notification.WithUserAddressResolver(&stubUserResolver{byUser: map[string]notification.UserAddresses{}}),
	)
	modules := []pkgcore.Module{module}
	modules = append(modules, extraModules...)
	if _, err := pkgcore.NewKernel(pkgcore.WithEventBus(bus, 0)).Bootstrap(ctx, modules...); err != nil {
		t.Fatalf("Kernel.Bootstrap over the Redis bus: %v", err)
	}
	return module, queue
}

// TestRedisBus_DeliveredInbox_AnnouncesAcrossReplicas is the distributed
// case this leg exists for: a delivery performed on the writer replica
// (the Dispatch through the real pipeline, the inbox row committed on the
// shared database) must announce itself on the bus the peer replica reads
// -- and the announcement must carry the four wire fields the event
// contract names, under the keys a remote consumer sees, so a peer that
// mirrors inboxes can read the row back without racing its writer.
func TestRedisBus_DeliveredInbox_AnnouncesAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	db := testutil.NewSQLite(t, "notification", migrations.FS)
	registerContactSerializer()

	client := startRedisClient(t, ctx)
	writerBus := pkgcore.NewRedisEventBus(client)
	peerBus := pkgcore.NewRedisEventBus(client)
	t.Cleanup(func() {
		writerBus.Close()
		peerBus.Close()
	})

	// The spy rides the peer's bus alongside the peer module's own
	// subscription (its Hub, attached during Register), which is exactly
	// the observer position a second replica's machinery occupies.
	spy := &eventSpy{}
	peerBus.Subscribe(notification.EventInboxCreated, spy.handler())

	// The writer replica boots with the clinic fixture module, whose
	// Register declares the appointment-reminder type and whose locale
	// bundle carries its copy; the peer boots alone.
	writer, writerQueue := bootReplica(t, ctx, db, writerBus, &clinic.Module{})
	bootReplica(t, ctx, db, peerBus)

	// Warm the stream's consumer groups: the peer's reader must exist
	// before the real delivery, or the announcement lands at the live end
	// of a group that is not there yet.
	warmUp(t, writerBus, spy)

	const tenant = "tenant-acme"
	const user = "user-1"
	d := notification.Dispatch{
		TypeKey: clinic.AppointmentReminderKey,
		Recipient: notification.DispatchRecipient{
			Class:  notification.RecipientClassUser,
			UserID: user,
		},
		Locale: "en-US",
		Params: map[string]any{
			"patient_name":     "Lin",
			"appointment_time": "9:30 AM",
		},
	}
	if _, err := writer.Deliveries().Dispatch(tenantCtx(tenant), d); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(writerQueue.tasks) != 1 {
		t.Fatalf("the delivery queue recorded %d tasks, want exactly one", len(writerQueue.tasks))
	}
	task := writerQueue.tasks[0]
	if task.TenantID != pkgcore.TenantID(tenant) {
		t.Errorf("the delivery task's tenant = %q, want %q", task.TenantID, tenant)
	}

	// One worker attempt over the job the queue recorded: the tenant is
	// rebuilt into the context the way jobs rebuilds it for a worker.
	if _, err := writer.Deliveries().Handle(tenantCtx(string(task.TenantID)), &jobs.Job{
		TenantID: task.TenantID,
		Payload:  task.Payload,
	}, nil); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	var received pkgcore.Event
	eventually(t, "the inbox-created event to reach the peer bus", func() bool {
		evt, ok := spy.first(func(evt pkgcore.Event) bool {
			return evt.Type == notification.EventInboxCreated && evt.TenantID == pkgcore.TenantID(tenant)
		})
		received = evt
		return ok
	})

	if received.Type != notification.EventInboxCreated {
		t.Errorf("remote event type = %q, want %q", received.Type, notification.EventInboxCreated)
	}
	payload, ok := received.Payload.(map[string]any)
	if !ok {
		t.Fatalf("remote event payload is %T, want map[string]any (the JSON wire shape)", received.Payload)
	}
	// The four wire fields of the event contract, by their json tag names
	// -- the keys every cross-replica consumer of the announcement sees.
	// A payload missing any of them cannot name the row the peer is meant
	// to read, so their presence and values are what the wire contract
	// actually needs.
	for key, want := range map[string]any{
		"tenant_id":         tenant,
		"recipient_user_id": user,
		"type_key":          clinic.AppointmentReminderKey,
	} {
		if got := payload[key]; got != want {
			t.Errorf("remote payload %s = %v, want %q", key, got, want)
		}
	}
	messageID, ok := payload["message_id"].(string)
	if !ok || messageID == "" {
		t.Fatalf("remote payload message_id = %v, want the inbox row's id", payload["message_id"])
	}

	// The named row is readable on the shared database under the tenant
	// the event names -- the receiving side's mirror path. The row's
	// rendered copy proves the merged catalog carried the fixture
	// module's bundle across the writer's bootstrap: the title and body
	// come from the fixture's own locale files, interpolated with the
	// dispatch's parameters at delivery time.
	rows, err := notification.NewRepository(db).List(tenantCtx(tenant))
	if err != nil {
		t.Fatalf("List inbox messages: %v", err)
	}
	var row *notification.InboxMessage
	for i := range rows {
		if rows[i].ID == messageID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("no inbox row %q under tenant %q (have %d rows)", messageID, tenant, len(rows))
	}
	if row.RecipientUserID != user {
		t.Errorf("row recipient = %q, want %q", row.RecipientUserID, user)
	}
	if row.TypeKey != clinic.AppointmentReminderKey {
		t.Errorf("row type key = %q, want %q", row.TypeKey, clinic.AppointmentReminderKey)
	}
	if row.Title != "Appointment reminder" {
		t.Errorf("row title = %q, want the fixture bundle's title", row.Title)
	}
	if !strings.Contains(row.Body, "Lin") || !strings.Contains(row.Body, "9:30 AM") {
		t.Errorf("row body = %q, want the fixture bundle's body rendered with the dispatch params", row.Body)
	}
}
