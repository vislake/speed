package notification

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/notification/internal/testutil"
	"github.com/vislake/speed/go/notification/migrations"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the ConfigSchema's empty-config
// decode, the token shapes and the declared bootstrap key path.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, notificationComponent)
}

// TestComponent_DeclaresTheContactIndexKey pins the schema's key-material
// field to the module's own single key: one secret, the HMAC key both
// contact indexers are built from, resolving at exactly the platform key
// path the component exports.
func TestComponent_DeclaresTheContactIndexKey(t *testing.T) {
	descriptors, err := pkgcore.DescribeComponentSchema(moduleName, notificationComponent.ConfigSchema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema() error = %v", err)
	}
	byKey := make(map[string]pkgcore.FieldDescriptor, len(descriptors))
	for _, d := range descriptors {
		byKey[d.Key] = d
	}
	field, ok := byKey[ContactIndexKeyPath]
	if !ok {
		t.Fatalf("the notification component did not declare key material at %q", ContactIndexKeyPath)
	}
	if !field.Derive || field.Type != "[]byte" || !field.Sensitive {
		t.Errorf("field = %+v, want a Sensitive derive-tagged []byte key", field)
	}
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying its four mandatory products plus the two optional
// resolvers, and pins that every value reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(testutil.NewSQLite(t, moduleName, migrations.FS))
	reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
	reg.Put(&stubQueue{})
	reg.Put(&stubUserResolver{byUser: map[string]UserAddresses{}})
	reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "", false }))

	instance, err := notificationComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(map[string]any{
		"mail_from": "notifications@test.example",
		"reply_to":  "support@test.example",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *notification.Module", instance, instance)
	}
	if m.mailFrom != "notifications@test.example" {
		t.Errorf("mailFrom = %q, want the configured value", m.mailFrom)
	}
	if m.mailReplyTo != "support@test.example" {
		t.Errorf("mailReplyTo = %q, want the configured value", m.mailReplyTo)
	}
	if m.sms == nil {
		t.Error("sms is nil, want the registry's sender wired through WithSMSSender")
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithDeliveryQueue")
	}
	if m.resolver == nil {
		t.Error("resolver is nil, want the registry's resolver wired through WithUserAddressResolver")
	}
	if m.subject == nil {
		t.Error("subject is nil, want the registry's optional subject resolver wired")
	}
}

// TestComponent_NewPropagatesOptionalReadFailures pins the optional-read
// contract the component's construction follows: an absent optional product
// is skipped (the module's documented default), while any other read
// failure -- two registered values for one optional token -- fails the
// construction instead of being taken for absence.
func TestComponent_NewPropagatesOptionalReadFailures(t *testing.T) {
	mandatory := func() *pkgcore.ComponentRegistry {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewSQLite(t, moduleName, migrations.FS))
		reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
		reg.Put(&stubQueue{})
		reg.Put(&stubUserResolver{byUser: map[string]UserAddresses{}})
		return reg
	}
	cfg := pkgcore.NewComponentConfig(map[string]any{
		"mail_from": "notifications@test.example",
		"reply_to":  "support@test.example",
	})

	t.Run("absent optional products are skipped", func(t *testing.T) {
		instance, err := notificationComponent.New(context.Background(), mandatory(), cfg)
		if err != nil {
			t.Fatalf("New: %v; want the absent optional resolvers skipped", err)
		}
		m, ok := instance.(*Module)
		if !ok {
			t.Fatalf("New returned %T, want *notification.Module", instance)
		}
		if m.subject != nil || m.userLocale != nil {
			t.Errorf("subject = %v, userLocale = %v; want both left at their absent default", m.subject, m.userLocale)
		}
	})

	t.Run("a duplicate delivery fails the construction", func(t *testing.T) {
		reg := mandatory()
		reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "", false }))
		reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "org:demo", true }))
		_, err := notificationComponent.New(context.Background(), reg, cfg)
		if !errors.Is(err, pkgcore.ErrAmbiguousProvider) {
			t.Fatalf("New = %v, want ErrAmbiguousProvider propagated, not the absent default", err)
		}
	})
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape: an
// empty registry fails the construction naming the missing database instead
// of building a half-wired module.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := notificationComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the module's declaration
// entry point through a real Init stage: the module's Register runs while
// the seats accept writes, so its full surface -- the inbox event catalog,
// the contact audit vocabulary, the delivery job handler, the type
// registrars the services read at call time, the inbox announcement
// subscription and the HTTP mount -- lands in the assembly's own seats and
// on the assembly's own bus.
//
// The module under test is the directly built one: the descriptor's New
// cannot yet supply the two contact blind indexers (a host pre-database
// step over cipher material the host holds), so a module built by the
// descriptor itself fails Register's ErrContactEmailIndexerRequired. The
// declaration body is the same on both paths, which is what this test
// pins.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	bus := pkgcore.NewMemoryEventBus()
	m := NewModule(db, testModuleOptions(t)...)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(bus)
	componenttest.DuringInit(t, reg, m.Register)

	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	if !slices.Contains(types, EventInboxCreated) {
		t.Errorf("Events seat = %v, want the inbox-created declaration", types)
	}
	if actions := reg.AuditActions.Actions(); len(actions) == 0 {
		t.Error("AuditActions seat is empty, want the module's contact audit vocabulary")
	}
	if _, claimed := reg.Jobs.Handlers()[jobTypeDeliver]; !claimed {
		t.Errorf("Jobs seat = %v, want the delivery handler", reg.Jobs.Handlers())
	}
	if routes := reg.Routes.Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built during the Init stage")
	}
	// The declarations reached the running services: both type-scoped
	// services hold the assembly's own registrar, and the delivery service
	// and handler hold the assembly's declaration face for their call-time
	// reads.
	if m.prefs.types == nil || m.contacts.types == nil {
		t.Error("the type-scoped services did not take the assembly's notification registrar")
	}
	if m.deliveries.host == nil || m.handler.host == nil {
		t.Error("the delivery service or handler did not take the assembly's declaration face")
	}
}
