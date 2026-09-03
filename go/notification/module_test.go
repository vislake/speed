package notification

import (
	"context"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// stubQueue is a jobs.Queue test double that records every task enqueued on
// it, so a Register or Dispatch test can assert on the enqueue shape without
// starting a worker pool. Get and Cancel are never reached by the delivery
// pipeline and panic rather than pretend.
type stubQueue struct {
	tasks []jobs.Task
}

func (q *stubQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.tasks = append(q.tasks, task)
	return "stub-job", nil
}

func (q *stubQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	panic("stubQueue.Get: not implemented")
}

func (q *stubQueue) Cancel(context.Context, jobs.JobID) error {
	panic("stubQueue.Cancel: not implemented")
}

// stubUserResolver is a UserAddressResolver test double answering from a
// per-user table, with an injectable error for the seam's failure tests.
type stubUserResolver struct {
	byUser map[string]UserAddresses
	err    error
}

func (r *stubUserResolver) Resolve(_ context.Context, userID string) (UserAddresses, error) {
	if r.err != nil {
		return UserAddresses{}, r.err
	}
	return r.byUser[userID], nil
}

// newHostRegistry returns a pkgcore.Registry over the in-process seam
// implementations -- the shape a standalone-deployment host assembles for
// itself -- with the module's three fixture types declared on the
// notification-type registrar, exactly as a declaring business module would
// register them before Bootstrap walks the module graph.
func newHostRegistry(t *testing.T) *pkgcore.Registry {
	t.Helper()
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Notifications.Add(fixtureTypes...); err != nil {
		t.Fatalf("reg.Notifications.Add: %v", err)
	}
	return reg
}

// testModuleOptions returns the six-seam option set every Register test
// needs: a console SMS sender (discarding output -- no test here asserts on
// what the module sends), the module's mail From address, the email and
// phone blind indexers the consent ledger is required to boot with, and the
// delivery pipeline's stub queue and user-address resolver. The indexers
// are per-test fixtures bound to this package's dev index keys, the same
// objects contact_test.go's service tests build; the queue and resolver are
// recording test doubles (stubQueue and stubUserResolver above) whose
// behaviour later files' delivery tests exercise directly.
func testModuleOptions(t *testing.T) []Option {
	t.Helper()
	return []Option{
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	}
}

// TestModule_Register_RequiresSMSSender pins the first of the six
// Register-time seam validations module.go's doc comment promises: a Module
// with no SMS sender -- every other seam present, so the gap is isolated --
// is refused with ErrSMSSenderRequired at boot rather than discovering the
// missing transport on the first verification code a patient needs.
func TestModule_Register_RequiresSMSSender(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without an SMS sender succeeded, want ErrSMSSenderRequired")
	}
	assertCode(t, err, ErrSMSSenderRequired.Code)
}

// TestModule_Register_RequiresMailFrom pins the second seam validation: the
// module cannot compose a single outbound email without a From address, so a
// Module missing it is refused with ErrMailFromRequired even though every
// other seam is present.
func TestModule_Register_RequiresMailFrom(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without a mail From address succeeded, want ErrMailFromRequired")
	}
	assertCode(t, err, ErrMailFromRequired.Code)
}

// TestModule_Register_RequiresEmailIndexer pins the third seam validation:
// an email contact can only be stored queryably through its blind index, so
// a Module missing the email indexer is refused with
// ErrContactEmailIndexerRequired at boot.
func TestModule_Register_RequiresEmailIndexer(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without the email indexer succeeded, want ErrContactEmailIndexerRequired")
	}
	assertCode(t, err, ErrContactEmailIndexerRequired.Code)
}

// TestModule_Register_RequiresPhoneIndexer pins the fourth seam validation:
// the SMS twin of the email indexer, refused with
// ErrContactPhoneIndexerRequired when absent.
func TestModule_Register_RequiresPhoneIndexer(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without the phone indexer succeeded, want ErrContactPhoneIndexerRequired")
	}
	assertCode(t, err, ErrContactPhoneIndexerRequired.Code)
}

// TestModule_Register_RequiresDeliveryQueue pins the fifth seam validation,
// the delivery pipeline's first: outbound deliveries are asynchronous by
// design, so a Module without a queue to carry them is refused with
// ErrDeliveryQueueRequired at boot rather than having every Dispatch refuse
// at run time.
func TestModule_Register_RequiresDeliveryQueue(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithUserAddressResolver(&stubUserResolver{byUser: map[string]UserAddresses{}}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without a delivery queue succeeded, want ErrDeliveryQueueRequired")
	}
	assertCode(t, err, ErrDeliveryQueueRequired.Code)
}

// TestModule_Register_RequiresUserAddressResolver pins the sixth seam
// validation: without a resolver the module cannot reach a user on the email
// or SMS channels at all, so a Module missing it is refused with
// ErrUserAddressResolverRequired at boot rather than dead-letter every such
// delivery.
func TestModule_Register_RequiresUserAddressResolver(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
		WithDeliveryQueue(&stubQueue{}),
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without a user-address resolver succeeded, want ErrUserAddressResolverRequired")
	}
	assertCode(t, err, ErrUserAddressResolverRequired.Code)
}

// TestModule_Register_DeclaresContactAuditActions pins the consent ledger's
// audit-action contribution: after Register, the registry's audit-action
// registrar carries the three actions contact.go's state transitions emit
// (attested, verified, unsubscribed), so a transition can be audited from
// the moment the module boots. Each declared action travels as the same
// string contact.go emits -- never re-typed at the assertion site.
func TestModule_Register_DeclaresContactAuditActions(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	declared := reg.AuditActions.Actions()
	for _, want := range contactAuditActionDecls {
		found := false
		for _, got := range declared {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("audit actions %v do not include %q", declared, want)
		}
	}
}

// TestModule_Register_AttachesTheHostRegistrarToPreferenceService proves the
// wiring contract module.go's doc comment promises: after Register, the
// preference service's type-taxonomy reference is the host registry's live
// notification-type registrar -- the same object the host's declaring modules
// populated -- and resolution answers from the taxonomy it holds (here, the
// defaults of a type the module itself never declared). The module declares no
// types of its own; every type in the matrix is a host-module type.
func TestModule_Register_AttachesTheHostRegistrarToPreferenceService(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx := tenantCtx("tenant-acme")
	got, err := module.Preferences().ResolveChannels(ctx, "user-7", fixtureTypeAppointment)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	want := []string{ChannelInApp, ChannelEmail, ChannelSMS}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveChannels = %v, want the host-registered defaults %v", got, want)
	}
}

// TestModule_Register_AttachesTransportSeamsToContactService pins the
// promise module.go's Contacts() doc makes -- "the service's seams are
// validated and attached (Register) by the time any caller reaches it" --
// for the consent ledger's four transport seams. Register validates the
// SMS sender, the mail From address and the two address blind indexers on
// the module, but a host reaches the ledger through Contacts(); a Register
// that validated them without attaching them to the service it hands out
// would give the host a service whose first CreateContact dereferences a
// nil blind indexer. The PostgreSQL tier's consent-flow test caught
// exactly that wiring gap through a kernel-booted module -- the one path
// no earlier test walked, every service test having built its service by
// hand and assigned the fields directly. This test pins the invariant at
// unit level, where it runs on every pull request.
func TestModule_Register_AttachesTransportSeamsToContactService(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	contacts := module.Contacts()
	if contacts.sms == nil {
		t.Error("Contacts() carries no SMS sender after Register")
	}
	if contacts.mailFrom != testMailFrom {
		t.Errorf("Contacts() mail From = %q, want the option's %q", contacts.mailFrom, testMailFrom)
	}
	if contacts.emailIndexer == nil {
		t.Error("Contacts() carries no email blind indexer after Register")
	}
	if contacts.phoneIndexer == nil {
		t.Error("Contacts() carries no phone blind indexer after Register")
	}
}

// TestModule_Register_TaxonomyIsLiveNotASnapshot proves the reference is
// live: a type the host registers AFTER Register still governs preference
// writes and reads. The module must hold the registrar, never a copy taken at
// registration time -- a module registering before its declaring siblings
// would otherwise freeze a taxonomy missing everything those siblings add
// later.
func TestModule_Register_TaxonomyIsLiveNotASnapshot(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	late := pkgcore.NotificationType{
		Key:             "clinic.billing_invoice_ready",
		Group:           "billing",
		DefaultChannels: []string{ChannelEmail},
		Unsubscribable:  false,
	}
	if err := reg.Notifications.Add(late); err != nil {
		t.Fatalf("reg.Notifications.Add(late type, after Register): %v", err)
	}

	ctx := tenantCtx("tenant-acme")
	if err := module.Preferences().Set(ctx, "user-7", late.Key, []string{ChannelEmail}); err != nil {
		t.Errorf("Set on a type registered after Register: %v", err)
	}
	got, err := module.Preferences().ResolveChannels(ctx, "user-7", late.Key)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	if !reflect.DeepEqual(got, []string{ChannelEmail}) {
		t.Errorf("ResolveChannels = %v, want the stored selection on the late-registered type", got)
	}
}

// TestModule_Register_DeclaresTheInboxEvent pins the module's event-catalog
// contribution: the registry's published declarations carry notification's
// inbox event exactly as events.go declares it -- type key, payload type name
// and description travelling with the declaration, never re-typed at the
// assertion site.
func TestModule_Register_DeclaresTheInboxEvent(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	found := false
	for _, decl := range reg.Events.Published() {
		if decl.Type == inboxEventDecls[0].Type {
			found = true
			if !reflect.DeepEqual(decl, inboxEventDecls[0]) {
				t.Errorf("declared inbox event = %+v, want %+v", decl, inboxEventDecls[0])
			}
		}
	}
	if !found {
		t.Errorf("published declarations %v do not include %s", reg.Events.Published(), inboxEventDecls[0].Type)
	}
}

// TestModule_Register_RegistersTheDeliveryJobHandler pins the module's
// job-handler contribution: after Register, the registry's job-handler
// registrar carries the deliver job type bound to the module's own delivery
// service -- the exact object delivery.go's Dispatch hand, so a host that
// starts a worker pool over the registry's handlers (the shape
// Kernel.Bootstrap composes) routes enqueued deliver jobs back to this
// module without any extra wiring. The bound value is the service itself,
// asserted by pointer: the registrar stores handlers as given, never a
// proxy.
func TestModule_Register_RegistersTheDeliveryJobHandler(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handlers := reg.Jobs.Handlers()
	handler, ok := handlers[jobTypeDeliver]
	if !ok {
		t.Fatalf("job handlers %v do not include %q", mapKeys(handlers), jobTypeDeliver)
	}
	if handler != module.Deliveries() {
		t.Errorf("handler for %q = %T, want the module's own delivery service", jobTypeDeliver, handler)
	}
}

// TestModule_Register_SubscribesTheHubToTheInboxEvent proves the fan-out
// wiring module.go's doc comment promises: after Register, an inbox event
// published on the registry's bus reaches a connection on the module's
// per-replica hub. The hub is the replica's live-update source -- a
// connection is exactly what an in-app inbox panel holds open -- so the
// subscription must ride the real bus, not a direct call into the hub's
// handler: a wiring mistake would leave a panel silent even though the
// handler itself works (which hub_test.go proves in isolation).
func TestModule_Register_SubscribesTheHubToTheInboxEvent(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db, testModuleOptions(t)...)
	reg := newHostRegistry(t)

	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	conn := module.hub.Subscribe()
	defer conn.Close()

	payload := InboxCreatedPayload{
		MessageID:       "inbox-77",
		TenantID:        "tenant-acme",
		RecipientUserID: "user-7",
		TypeKey:         "clinic.appointment_reminder",
	}
	if err := reg.Events.Bus().Publish(context.Background(), pkgcore.Event{
		Type:     EventInboxCreated,
		TenantID: pkgcore.TenantID(payload.TenantID),
		Payload:  payload,
	}); err != nil {
		t.Fatalf("bus publish: %v", err)
	}

	want := `{"message_id":"inbox-77","tenant_id":"tenant-acme","recipient_user_id":"user-7","type_key":"clinic.appointment_reminder"}`
	msg := assertMessage(t, conn, nil)
	if string(msg) != want {
		t.Errorf("hub payload JSON = %s, want %s", msg, want)
	}
}

// mapKeys returns the sorted keys of m for the stable, readable listing the
// registration tests' failure messages want.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
