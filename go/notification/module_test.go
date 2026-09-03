package notification

import (
	"io"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

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

// testModuleOptions returns the four-seam option set every Register test
// needs: a console SMS sender (discarding output -- no test here asserts on
// what the module sends), the module's mail From address, and the email and
// phone blind indexers the consent ledger is required to boot with. The
// indexers are per-test fixtures bound to this package's dev index keys, the
// same objects contact_test.go's service tests build.
func testModuleOptions(t *testing.T) []Option {
	t.Helper()
	return []Option{
		WithSMSSender(NewConsoleSMSSender(io.Discard)),
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
	}
}

// TestModule_Register_RequiresSMSSender pins the first of the four
// Register-time seam validations module.go's doc comment promises: a Module
// with no SMS sender -- the mail From address and both indexers present, so
// the gap is isolated -- is refused with ErrSMSSenderRequired at boot rather
// than discovering the missing transport on the first verification code a
// patient needs.
func TestModule_Register_RequiresSMSSender(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db,
		WithMailFrom(testMailFrom),
		WithContactEmailIndexer(testEmailIndexer(t)),
		WithContactPhoneIndexer(testPhoneIndexer(t)),
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
	)

	err := module.Register(newHostRegistry(t))
	if err == nil {
		t.Fatal("Register without the phone indexer succeeded, want ErrContactPhoneIndexerRequired")
	}
	assertCode(t, err, ErrContactPhoneIndexerRequired.Code)
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
