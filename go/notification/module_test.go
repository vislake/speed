package notification

import (
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

// TestModule_Register_AttachesTheHostRegistrarToPreferenceService proves the
// wiring contract module.go's doc comment promises: after Register, the
// preference service's type-taxonomy reference is the host registry's live
// notification-type registrar -- the same object the host's declaring modules
// populated -- and resolution answers from the taxonomy it holds (here, the
// defaults of a type the module itself never declared). The module declares no
// types of its own; every type in the matrix is a host-module type.
func TestModule_Register_AttachesTheHostRegistrarToPreferenceService(t *testing.T) {
	db := newTestDB(t)
	module := NewModule(db)
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
	module := NewModule(db)
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
	module := NewModule(db)
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
