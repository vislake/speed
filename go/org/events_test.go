package org

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"

	"github.com/vislake/speed/go/org/locales"
)

// recordingBus is an EventBus that delegates to the real in-memory bus and
// keeps a copy of everything published, so a test can assert what org
// announced without subscribing to each type by hand.
type recordingBus struct {
	pkgcore.EventBus

	mu        sync.Mutex
	published []pkgcore.Event
	failWith  error
}

func newRecordingBus() *recordingBus {
	return &recordingBus{EventBus: pkgcore.NewMemoryEventBus()}
}

func (b *recordingBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	b.published = append(b.published, evt)
	failure := b.failWith
	b.mu.Unlock()
	if failure != nil {
		return failure
	}
	return b.EventBus.Publish(ctx, evt)
}

// events returns every event published under eventType, in publish order.
func (b *recordingBus) events(eventType string) []pkgcore.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []pkgcore.Event
	for _, evt := range b.published {
		if evt.Type == eventType {
			out = append(out, evt)
		}
	}
	return out
}

// recordingMailer is a pkgcore.Mailer that keeps every message instead of
// sending it, and can be made to fail.
type recordingMailer struct {
	mu       sync.Mutex
	sent     []pkgcore.Mail
	failWith error
}

func (m *recordingMailer) Send(_ context.Context, mail pkgcore.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	m.sent = append(m.sent, mail)
	return nil
}

func (m *recordingMailer) messages() []pkgcore.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pkgcore.Mail(nil), m.sent...)
}

// testHost is a stand-in for the host's *pkgcore.Registry: the four seams
// org reads at call time, each backed by the standalone deployment mode's own
// implementation, which is exactly what the deployment-mode design promises
// a unit test can use instead of a double.
type testHost struct {
	kv      pkgcore.KVStore
	mailer  *recordingMailer
	catalog *i18n.Catalog
	bus     *recordingBus
}

func (h *testHost) KVStore() pkgcore.KVStore { return h.kv }

// Mailer returns a genuinely nil interface when no mailer is wired, rather
// than a non-nil interface wrapping a nil pointer. A real *pkgcore.Registry
// behaves that way, and a fake that did not would let a nil-mailer test call
// straight through into a nil method receiver.
func (h *testHost) Mailer() pkgcore.Mailer {
	if h.mailer == nil {
		return nil
	}
	return h.mailer
}
func (h *testHost) Locales() *i18n.Catalog     { return h.catalog }
func (h *testHost) EventBus() pkgcore.EventBus { return h.bus }

// compile-time check that the fake offers exactly the seam view org reads.
var _ hostSeams = (*testHost)(nil)

// newTestHost builds a host whose catalog is org's REAL locale bundle, merged
// through the same i18n.Builder the kernel uses. Rendering against the real
// files is the point: a message id this module looks up but never shipped
// fails here rather than in production.
func newTestHost(t *testing.T) *testHost {
	t.Helper()
	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, locales.FS); err != nil {
		t.Fatalf("build the org message catalog: %v", err)
	}
	return &testHost{
		kv:      pkgcore.NewMemoryKVStore(),
		mailer:  &recordingMailer{},
		catalog: builder.Build(),
		bus:     newRecordingBus(),
	}
}

// newTestModule builds a fully wired module over a fresh SQLite database: the
// same construction a host performs, with a fake host attached the way
// Register attaches the real registry.
//
// Going through NewModule rather than the individual constructors is
// deliberate -- it is what wires the tree's member guard and the shared
// services, so the tests exercise the arrangement a host actually gets.
func newTestModule(t *testing.T) (*Module, *testHost) {
	t.Helper()
	host := newTestHost(t)
	m := NewModule(newInvitationTestDB(t),
		WithEmailIndexer(newTestEmailIndexer(t)),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	)
	m.attach(host)
	return m, host
}

// seedTree creates a root with two children and returns all three, for tests
// that need a shape rather than a single node.
func seedTree(t *testing.T, tree *TreeService, ctx context.Context) (root, left, right *OrgNode) {
	t.Helper()
	root, err := tree.CreateRoot(ctx, "group", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	if left, err = tree.CreateChild(ctx, root.ID, "left store", "store"); err != nil {
		t.Fatalf("CreateChild(left): %v", err)
	}
	if right, err = tree.CreateChild(ctx, root.ID, "right store", "store"); err != nil {
		t.Fatalf("CreateChild(right): %v", err)
	}
	return root, left, right
}

// userCreatedPayload is a stand-in for authn's own event payload. It is
// declared HERE, in org's tests, precisely because org may not import
// authn's type: the subscriber has to work against a shape it has never
// seen, and this struct is how that claim is exercised.
type userCreatedPayload struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

func TestUserIDFromPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
		wantOK  bool
	}{
		{"publisher struct with a json tag", userCreatedPayload{UserID: "u-1"}, "u-1", true},
		{"map as the redis bus delivers it", map[string]any{"user_id": "u-2"}, "u-2", true},
		{"camelCase spelling", map[string]any{"userId": "u-3"}, "u-3", true},
		{"exported-field spelling", map[string]any{"UserID": "u-4"}, "u-4", true},
		{"mixed-case spelling", map[string]any{"userID": "u-5"}, "u-5", true},
		{"nil payload", nil, "", false},
		{"a bare string", "u-6", "", false},
		{"a map with no user id", map[string]any{"email": "ada@example.com"}, "", false},
		{"an empty user id", map[string]any{"user_id": ""}, "", false},
		{"a non-string user id", map[string]any{"user_id": 42}, "", false},
		{"something json cannot marshal", map[string]any{"user_id": make(chan int)}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := userIDFromPayload(tc.payload)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("userIDFromPayload(%#v) = (%q, %t), want (%q, %t)", tc.payload, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestHandleUserCreated_NoPublisher_Noop is resilience case 1: the
// subscription is installed against a bus nobody publishes on. Nothing is
// delivered and nothing breaks -- and, crucially, Subscribe itself cannot
// fail, so an absent publisher is not even an observable condition.
func TestHandleUserCreated_NoPublisher_Noop(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	if err := host.bus.Publish(context.Background(), pkgcore.Event{
		Type:     "some.other.event",
		TenantID: "tenant-a",
		Payload:  userCreatedPayload{UserID: "u-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx := tenantCtx("tenant-a")
	if _, err := m.Tree().Root(ctx); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Root() error = %v, want org.node_not_found -- nothing should have been created", err)
	}
}

// TestHandleUserCreated_UnknownPayloadShape_LogsAndReturnsNil is resilience
// case 2, and the assertion that matters is the nil: on the in-memory bus a
// handler error propagates back into the PUBLISHER's Publish call, so
// returning one here would make org's failure to understand a payload look
// like a failed user creation inside authn.
func TestHandleUserCreated_UnknownPayloadShape_LogsAndReturnsNil(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	for _, payload := range []any{nil, "a bare string", map[string]any{"email": "ada@example.com"}, 42} {
		err := host.bus.Publish(context.Background(), pkgcore.Event{
			Type:     EventUserCreated,
			TenantID: "tenant-a",
			Payload:  payload,
		})
		if err != nil {
			t.Errorf("Publish(payload %#v) error = %v, want nil -- org must never fail the publisher", payload, err)
		}
	}

	ctx := tenantCtx("tenant-a")
	if _, err := m.Tree().Root(ctx); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Root() error = %v, want org.node_not_found -- an unusable payload must create nothing", err)
	}
}

// TestHandleUserCreated_EmptyTenant_DoesNothing is resilience case 3. A
// self-registering user genuinely has no tenant yet, so there is no workspace
// to create and no error to report.
func TestHandleUserCreated_EmptyTenant_DoesNothing(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	if err := host.bus.Publish(context.Background(), pkgcore.Event{
		Type:    EventUserCreated,
		Payload: userCreatedPayload{UserID: "u-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if joined := host.bus.events(EventMemberJoined); len(joined) != 0 {
		t.Errorf("published %d member-joined events, want 0", len(joined))
	}
}

// TestHandleUserCreated_CreatesTheDefaultWorkspace is resilience case 4: the
// tenant context is rebuilt from the event -- a handler on the distributed
// bus carries none -- and the user ends up with a root node and a membership.
func TestHandleUserCreated_CreatesTheDefaultWorkspace(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	// The publishing context deliberately carries NO tenant, exactly as a
	// remote-bus handler's does: everything downstream must work off the
	// event's own TenantID.
	if err := host.bus.Publish(context.Background(), pkgcore.Event{
		Type:     EventUserCreated,
		TenantID: "tenant-a",
		Payload:  userCreatedPayload{UserID: "u-1", Email: "ada@example.com"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx := tenantCtx("tenant-a")
	root, err := m.Tree().Root(ctx)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	membership, err := m.Members().Get(ctx, "u-1")
	if err != nil {
		t.Fatalf("Get membership: %v", err)
	}
	if membership.NodeID != root.ID {
		t.Errorf("membership.NodeID = %q, want the root %q", membership.NodeID, root.ID)
	}
	if !membership.IsActive() {
		t.Errorf("membership.Status = %q, want %q", membership.Status, MembershipStatusActive)
	}
	if joined := host.bus.events(EventMemberJoined); len(joined) != 1 {
		t.Fatalf("published %d member-joined events, want 1", len(joined))
	}
}

// TestHandleUserCreated_Redelivered_IsIdempotent is the at-least-once
// guarantee every real broker gives: the identical event arrives twice and
// must produce one root and one membership, not two.
func TestHandleUserCreated_Redelivered_IsIdempotent(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	evt := pkgcore.Event{
		Type:     EventUserCreated,
		TenantID: "tenant-a",
		Payload:  userCreatedPayload{UserID: "u-1"},
	}
	for i := range 2 {
		if err := host.bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("Publish #%d: %v", i+1, err)
		}
	}

	ctx := tenantCtx("tenant-a")
	nodes, err := m.Tree().Repository().List(ctx)
	if err != nil {
		t.Fatalf("List nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("tenant holds %d nodes after a redelivery, want exactly 1 root", len(nodes))
	}
	memberships, err := m.Members().Repository().List(ctx)
	if err != nil {
		t.Fatalf("List memberships: %v", err)
	}
	if len(memberships) != 1 {
		t.Errorf("tenant holds %d memberships after a redelivery, want exactly 1", len(memberships))
	}
	// The second delivery changed nothing, so it announced nothing.
	if joined := host.bus.events(EventMemberJoined); len(joined) != 1 {
		t.Errorf("published %d member-joined events, want 1", len(joined))
	}
}

// TestHandleUserCreated_TwoTenants_EachGetTheirOwnWorkspace pins that the
// handler works off the event's tenant rather than any ambient one.
func TestHandleUserCreated_TwoTenants_EachGetTheirOwnWorkspace(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		if err := host.bus.Publish(context.Background(), pkgcore.Event{
			Type:     EventUserCreated,
			TenantID: tenant,
			Payload:  userCreatedPayload{UserID: "u-shared"},
		}); err != nil {
			t.Fatalf("Publish for %s: %v", tenant, err)
		}
	}

	rootA, err := m.Tree().Root(tenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("Root(tenant-a): %v", err)
	}
	rootB, err := m.Tree().Root(tenantCtx("tenant-b"))
	if err != nil {
		t.Fatalf("Root(tenant-b): %v", err)
	}
	if rootA.ID == rootB.ID {
		t.Error("both tenants share one root node; the handler ignored the event's tenant")
	}
	// One person, two tenants, two memberships: that is the whole reason
	// users are identity data and memberships are tenant-scoped link data.
	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		if _, err := m.Members().Get(tenantCtx(tenant), "u-shared"); err != nil {
			t.Errorf("Get membership in %s: %v", tenant, err)
		}
	}
}

// TestHandleUserCreated_DefaultWorkspaceName_ComesFromTheCatalog proves the
// one node org names on its own is not an English literal in Go.
func TestHandleUserCreated_DefaultWorkspaceName_ComesFromTheCatalog(t *testing.T) {
	m, host := newTestModule(t)
	host.bus.Subscribe(EventUserCreated, m.handleUserCreated)

	if err := host.bus.Publish(context.Background(), pkgcore.Event{
		Type:     EventUserCreated,
		TenantID: "tenant-a",
		Payload:  userCreatedPayload{UserID: "u-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	root, err := m.Tree().Root(tenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	want, err := host.catalog.Lookup(i18n.LocaleZHCN, msgDefaultWorkspaceName, nil)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", msgDefaultWorkspaceName, err)
	}
	if root.Name != want {
		t.Errorf("root name = %q, want the catalog's %q", root.Name, want)
	}
	if root.Kind != defaultRootKind {
		t.Errorf("root kind = %q, want %q", root.Kind, defaultRootKind)
	}
}

// TestDefaultWorkspaceName_NoCatalog_FallsBackToAnIdentifier pins the
// fallback: an identifier, never a string in one of the two languages, so a
// catalog failure cannot smuggle English into a Chinese UI.
func TestDefaultWorkspaceName_NoCatalog_FallsBackToAnIdentifier(t *testing.T) {
	m := NewModule(nil)
	if got := m.defaultWorkspaceName(tenantCtx("tenant-a")); got != "tenant-a" {
		t.Errorf("defaultWorkspaceName without a host = %q, want the tenant id", got)
	}
	if got := m.defaultWorkspaceName(context.Background()); got != defaultRootKind {
		t.Errorf("defaultWorkspaceName without a tenant = %q, want %q", got, defaultRootKind)
	}
}

// TestPublishEvent_BusFailure_IsSwallowed pins that a publish failure never
// becomes the caller's error: the business write has already committed, so
// reporting the bus failure would claim a write failed when it did not.
func TestPublishEvent_BusFailure_IsSwallowed(t *testing.T) {
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	host.bus.failWith = errors.New("bus is down")

	if _, err := m.Tree().CreateRoot(ctx, "group", "group"); err != nil {
		t.Fatalf("CreateRoot with a failing bus: %v, want the write to succeed anyway", err)
	}
}

// TestPublishEvent_NoTenant_IsNotPublished pins the guard on the payload's
// own tenant: an event with no tenant is not published at all, rather than
// published with an empty one that a subscriber would silently mis-route.
func TestPublishEvent_NoTenant_IsNotPublished(t *testing.T) {
	_, host := newTestModule(t)
	publishEvent(context.Background(), host, EventNodeCreated, NodeCreated{NodeID: "n-1"})

	if got := host.bus.events(EventNodeCreated); len(got) != 0 {
		t.Errorf("published %d events without a tenant, want 0", len(got))
	}
}

// TestPublishEvent_NoHost_IsSafe pins that a service used before Bootstrap
// (or in a test that never attached a host) does not panic on publish.
func TestPublishEvent_NoHost_IsSafe(t *testing.T) {
	publishEvent(tenantCtx("tenant-a"), nil, EventNodeCreated, NodeCreated{NodeID: "n-1"})
}

// TestTreeService_PublishesNodeEvents closes the loop B1 left open: the three
// org.node.* declarations now have a publisher, and each carries the payload
// its EventDecl names.
func TestTreeService_PublishesNodeEvents(t *testing.T) {
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	created := host.bus.events(EventNodeCreated)
	if len(created) != 3 {
		t.Fatalf("published %d node-created events, want 3", len(created))
	}
	payload, ok := created[0].Payload.(NodeCreated)
	if !ok {
		t.Fatalf("node-created payload is %T, want org.NodeCreated", created[0].Payload)
	}
	if payload.NodeID != root.ID || payload.Path != root.Path {
		t.Errorf("node-created payload = %+v, want the root %q at %q", payload, root.ID, root.Path)
	}
	if created[0].TenantID != "tenant-a" {
		t.Errorf("event tenant = %q, want %q", created[0].TenantID, "tenant-a")
	}

	grandchild, err := m.Tree().CreateChild(ctx, left.ID, "chair", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	oldPath := grandchild.Path
	moved, err := m.Tree().Move(ctx, grandchild.ID, root.ID)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	movedEvents := host.bus.events(EventNodeMoved)
	if len(movedEvents) != 1 {
		t.Fatalf("published %d node-moved events, want 1", len(movedEvents))
	}
	movePayload, ok := movedEvents[0].Payload.(NodeMoved)
	if !ok {
		t.Fatalf("node-moved payload is %T, want org.NodeMoved", movedEvents[0].Payload)
	}
	if movePayload.OldPath != oldPath || movePayload.NewPath != moved.Path {
		t.Errorf("node-moved payload = %+v, want old %q new %q", movePayload, oldPath, moved.Path)
	}
	if movePayload.OldParentID != left.ID || movePayload.NewParentID != root.ID {
		t.Errorf("node-moved parents = (%q -> %q), want (%q -> %q)",
			movePayload.OldParentID, movePayload.NewParentID, left.ID, root.ID)
	}

	if err := m.Tree().Delete(ctx, moved.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deleted := host.bus.events(EventNodeDeleted)
	if len(deleted) != 1 {
		t.Fatalf("published %d node-deleted events, want 1", len(deleted))
	}
	deletePayload, ok := deleted[0].Payload.(NodeDeleted)
	if !ok {
		t.Fatalf("node-deleted payload is %T, want org.NodeDeleted", deleted[0].Payload)
	}
	if deletePayload.Cascade || deletePayload.RemovedCount != 1 {
		t.Errorf("node-deleted payload = %+v, want cascade=false removed=1", deletePayload)
	}
}

// TestEventPayloads_CarryJSONTags pins the wire contract: a payload crossing
// the distributed mode's Redis bus arrives at the subscriber as a map keyed
// by these names, so the tags are public API rather than a serialization
// detail. A field without one would arrive under its Go name and quietly
// break every remote subscriber.
func TestEventPayloads_CarryJSONTags(t *testing.T) {
	payloads := []any{
		NodeCreated{},
		NodeMoved{},
		NodeDeleted{},
		MemberInvited{},
		MemberJoined{},
		MemberRemoved{},
	}
	for _, payload := range payloads {
		typ := reflect.TypeOf(payload)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := range typ.NumField() {
				field := typ.Field(i)
				tag, ok := field.Tag.Lookup("json")
				if !ok || tag == "" {
					t.Errorf("%s.%s has no json tag", typ.Name(), field.Name)
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if name != toSnakeCase(field.Name) {
					t.Errorf("%s.%s json tag = %q, want the snake_case %q the attribute-key convention requires",
						typ.Name(), field.Name, name, toSnakeCase(field.Name))
				}
			}
		})
	}
}

// TestMemberInvited_CarriesNoAddress is a security assertion, not a shape
// one: an event payload is written to a broker, logged by whoever subscribes
// and often traced, so an email address in one publishes PII to all three.
func TestMemberInvited_CarriesNoAddress(t *testing.T) {
	typ := reflect.TypeOf(MemberInvited{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		if name == "email" {
			t.Errorf("MemberInvited declares a field %q; it must carry the blind index only", typ.Field(i).Name)
		}
	}
}

// TestModule_Register_DoesNotDeclareTheAuthnEvent guards the coordination
// point with the parallel authn round: authn owns authn.user.created and
// declaring it here too makes a host running both modules fail to bootstrap
// with ErrDuplicateEventType.
func TestModule_Register_DoesNotDeclareTheAuthnEvent(t *testing.T) {
	reg := bootstrapTestModule(t)
	for _, decl := range reg.Events.Published() {
		if decl.Type == EventUserCreated {
			t.Fatalf("org declares %q; only authn may declare it, and org only subscribes", EventUserCreated)
		}
		if !strings.HasPrefix(decl.Type, moduleName+".") {
			t.Errorf("org declares %q, which is not one of its own events", decl.Type)
		}
	}
}

// toSnakeCase converts a Go field name to the snake_case spelling the
// project's attribute-key convention requires of every serialized name.
func toSnakeCase(name string) string {
	var out []rune
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prevLower := unicode.IsLower(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				out = append(out, '_')
			}
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

// testIndexerKey is a fixed 32-byte HMAC key for the tests' blind indexer. It
// is a test fixture, not a secret: a real deployment injects one from its own
// secret store, and it must differ from the encryption key.
var testIndexerKey = []byte("org-test-blind-index-key-32bytes")

// newTestEmailIndexer builds the blind indexer the module requires.
func newTestEmailIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("email_index", testIndexerKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer: %v", err)
	}
	return indexer
}
