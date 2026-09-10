package notification_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"
)

// ExampleInboxMessage walks a delivery into one tenant's in-app inbox end
// to end: open and migrate the database, create a message under the
// recipient's tenant, read it back, mark it read, and watch the same id
// read as not-found from a tenant that does not own it.
//
// The example needs no testcontainers: dbkit.Open over an in-memory SQLite
// database is the standalone deployment mode's ordinary shape, and the
// module's own migration files apply from zero through the real registry.
func ExampleInboxMessage() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:notification_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(notification.NewModule(db)); err != nil {
		fmt.Println("register migrations:", err)
		return
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	repo := notification.NewRepository(db)
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	key := "delivery-note-42"
	msg := &notification.InboxMessage{
		ID:              "inbox-000001",
		RecipientUserID: "user-7",
		TypeKey:         "note.shared",
		Title:           "Note 42 was shared with you",
		Body:            "Lin shared the Caries 101 note with you.",
		DedupeKey:       &key,
	}
	if err = repo.Create(ctx, msg); err != nil {
		fmt.Println("create:", err)
		return
	}

	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		fmt.Println("find:", err)
		return
	}
	fmt.Println("title:", got.Title)

	readAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	got.ReadAt = &readAt
	if err = repo.Update(ctx, got); err != nil {
		fmt.Println("mark read:", err)
		return
	}
	got, err = repo.FindByID(ctx, msg.ID)
	if err != nil {
		fmt.Println("re-read:", err)
		return
	}
	fmt.Println("read at:", got.ReadAt.Format("2006-01-02 15:04:05"))

	_, err = repo.FindByID(pkgcore.WithTenant(ctx, "tenant-bright"), msg.ID)
	if appErr, ok := apperr.As(err); ok {
		fmt.Println("other tenant:", appErr.Code)
	}

	// Output:
	// title: Note 42 was shared with you
	// read at: 2026-09-01 10:00:00
	// other tenant: dbkit.record_not_found
}

// ExampleSendRecordRepository_ListByFilter walks the operator-facing
// send-record search go/admin's surface builds on: finding out whether a
// delivery actually went out, and what happened, filtered by tenant,
// channel and status.
//
// The example needs no delivery pipeline: it writes two send_records rows
// directly, exactly as an outbound delivery attempt itself would (Create
// on a fresh record, Save to settle it), which is enough to demonstrate
// ListByFilter's own contract without standing up a full Dispatch.
func ExampleSendRecordRepository_ListByFilter() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:notification_example_send_records?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(notification.NewModule(db)); err != nil {
		fmt.Println("register migrations:", err)
		return
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	repo := notification.NewSendRecordRepository(db)

	succeeded := &notification.SendRecord{
		ID:              "send-record-000001",
		TenantID:        "tenant-acme",
		TypeKey:         "note.shared",
		Channel:         notification.ChannelEmail,
		RecipientClass:  notification.RecipientClassUser,
		RecipientUserID: "user-7",
		Status:          notification.SendRecordStatusSucceeded,
		IdempotencyKey:  "delivery-key-1",
	}
	if err = repo.Create(ctx, succeeded); err != nil {
		fmt.Println("create succeeded record:", err)
		return
	}

	failed := &notification.SendRecord{
		ID:              "send-record-000002",
		TenantID:        "tenant-acme",
		TypeKey:         "note.shared",
		Channel:         notification.ChannelSMS,
		RecipientClass:  notification.RecipientClassUser,
		RecipientUserID: "user-7",
		Status:          notification.SendRecordStatusFailed,
		Error:           "transport failed",
		IdempotencyKey:  "delivery-key-2",
	}
	if err = repo.Create(ctx, failed); err != nil {
		fmt.Println("create failed record:", err)
		return
	}

	records, err := repo.ListByFilter(ctx, notification.SendRecordFilter{
		TenantID: "tenant-acme",
		Status:   notification.SendRecordStatusFailed,
		Limit:    10,
	})
	if err != nil {
		fmt.Println("list:", err)
		return
	}
	for _, rec := range records {
		fmt.Printf("channel=%s status=%s error=%s\n", rec.Channel, rec.Status, rec.Error)
	}

	// Output:
	// channel=sms status=failed error=transport failed
}

// ExampleNotificationPreferences walks the preference matrix the way a host
// assembles it: the business module declares its notification types on the
// host registry, the notification module's Register attaches that registrar
// to its preference service, and a recipient's stored choice then wins over
// the declared defaults -- while a recipient with no stored choice receives
// the type's default channels.
//
// The example doubles as the wiring proof: the module itself declares no
// types; every type answered below was registered by the host before and
// after the module's own Register call. The module is constructed with the
// six seams Register requires -- a console SMS sender, a mail From address,
// the two address blind indexers, a queue to carry outbound deliveries (here
// the standalone shape, a jobs.StandaloneQueue over the same database,
// never started because the walk below enqueues nothing) and a
// user-address resolver (staticaddr.New over a fixed table, the minimal
// wiring for a host whose addresses are statically held; never consulted
// because the walk below resolves no address) -- exactly as a
// standalone-deployment host assembles them.
func ExamplePreferenceService() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:notification_preferences_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// The required seams, in the standalone shape: a console sender for SMS,
	// one blind indexer per address channel over dev keys (a real host
	// derives its keys from its own secret store), the standalone queue over
	// the same database, and a resolver over a fixed table of
	// operator-declared addresses, standing in for the host's own address
	// store.
	// The indexers' column argument is the module's exported
	// notification.AddressIndexColumn -- the blind-index column's exact SQL
	// name, carried as a referenced constant rather than a hand-typed
	// string (see its doc comment for why dbkit cannot guard a wrong one).
	emailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn,
		[]byte("abcdef0123456789abcdef0123456789"), dbkit.NormalizeEmail)
	if err != nil {
		fmt.Println("email indexer:", err)
		return
	}
	phoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn,
		[]byte("3456789abcdef0123456789abcdef015"), dbkit.NormalizePhoneE164)
	if err != nil {
		fmt.Println("phone indexer:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	module := notification.NewModule(db,
		notification.WithSMSSender(pkgcore.NewConsoleSMSSender(io.Discard)),
		notification.WithMailFrom("notifications@example.com"),
		notification.WithContactEmailIndexer(emailIndexer),
		notification.WithContactPhoneIndexer(phoneIndexer),
		notification.WithDeliveryQueue(jobs.NewStandaloneQueue(db)),
		notification.WithUserAddressResolver(staticaddr.New(map[string]notification.UserAddresses{
			"user-7": {Email: "demo@example.com", Phone: "+8613800138000"},
		})),
	)
	if err = registry.Register(module); err != nil {
		fmt.Println("register migrations:", err)
		return
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	// A host assembles its own registry over the in-process seam
	// implementations and declares its notification types on it -- the same
	// shape Kernel.Bootstrap composes in a standalone deployment.
	host := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	appointment := pkgcore.NotificationType{
		Key:             "clinic.appointment_reminder",
		Group:           "clinic",
		DefaultChannels: []string{"in_app", "email", "sms"},
		Unsubscribable:  true,
	}
	result := pkgcore.NotificationType{
		Key:             "clinic.result_ready",
		Group:           "clinic",
		DefaultChannels: []string{"in_app", "email"},
		Unsubscribable:  true,
	}
	if err = host.Notifications.Add(appointment, result); err != nil {
		fmt.Println("add types:", err)
		return
	}
	if err = module.Register(host); err != nil {
		fmt.Println("register:", err)
		return
	}

	prefs := module.Preferences()
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	// The recipient opts out of email for result notifications; only channels
	// inside the type's declared space (its defaults) are selectable, so the
	// in_app channel alone survives as the stored choice.
	if err = prefs.Set(ctx, "user-7", result.Key, []string{"in_app"}); err != nil {
		fmt.Println("set:", err)
		return
	}
	channels, err := prefs.ResolveChannels(ctx, "user-7", result.Key)
	if err != nil {
		fmt.Println("resolve:", err)
		return
	}
	fmt.Println("result channels:", channels)

	// No stored preference for the appointment reminder: the declared
	// defaults govern.
	channels, err = prefs.ResolveChannels(ctx, "user-7", appointment.Key)
	if err != nil {
		fmt.Println("resolve default:", err)
		return
	}
	fmt.Println("appointment channels:", channels)

	rows, err := prefs.ListForUser(ctx, "user-7")
	if err != nil {
		fmt.Println("list:", err)
		return
	}
	fmt.Println("stored rows:", len(rows))

	// Output:
	// result channels: [in_app]
	// appointment channels: [in_app email sms]
	// stored rows: 1
}

// ExampleSubjectResolverFunc wires notification's caller-identity seam with
// a closure: the adapter gives a plain (r *http.Request) (string, bool)
// function the Subject method WithSubjectResolver takes, so a host whose
// resolver is one closure needs no type declaration. A real resolver must
// derive the caller from a source the server itself verified; this example
// reads a header only to keep the shape visible.
func ExampleSubjectResolverFunc() {
	resolver := notification.SubjectResolverFunc(func(r *http.Request) (string, bool) {
		userID := r.Header.Get("X-Verified-User")
		return userID, userID != ""
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Verified-User", "u-42")
	userID, ok := resolver.Subject(req)
	fmt.Println(userID, ok)
	// Output: u-42 true
}

// ExampleRegisterContactAddressSerializer registers the VerifiedContact
// address column's cipher through the module's own registrar and builds
// both channel indexers through the module's own constructors: the module
// owns the serializer name, the index column and each channel's normalizer,
// so none of them crosses the host's wiring as a hand-typed string. One
// blind-index key may serve both channels; it must be a different secret
// from the cipher's.
func ExampleRegisterContactAddressSerializer() {
	cipher, err := dbkit.NewCipher([]byte("example-contacts-cipher-32-bytes"))
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	if err := notification.RegisterContactAddressSerializer(cipher); err != nil {
		fmt.Println("register:", err)
		return
	}
	key := []byte("example-blind-index-key-32-bytes")
	if _, err := notification.NewContactEmailIndexer(key); err != nil {
		fmt.Println("email indexer:", err)
		return
	}
	if _, err := notification.NewContactPhoneIndexer(key); err != nil {
		fmt.Println("phone indexer:", err)
		return
	}
	fmt.Println("registered and indexed")
	// Output: registered and indexed
}
