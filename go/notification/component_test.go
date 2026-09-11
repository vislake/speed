package notification

import (
	"context"
	"io"
	"net/http"
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

// TestComponent_DeclaresTheContactIndexKey pins the bootstrap key
// declaration to the module's own single key material: one secret, the HMAC
// key both contact indexers are built from.
func TestComponent_DeclaresTheContactIndexKey(t *testing.T) {
	keys := notificationComponent.BootstrapKeys
	if len(keys) != 1 || keys[0].Key != "notification.contact_index_key" {
		t.Fatalf("BootstrapKeys = %v, want the single notification.contact_index_key declaration", keys)
	}
	if !keys[0].Sensitive {
		t.Error("contact index key is not marked Sensitive, want the key-material marking")
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

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape: an
// empty registry fails the construction naming the missing database instead
// of building a half-wired module.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := notificationComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
