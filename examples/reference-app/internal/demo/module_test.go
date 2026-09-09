package demo

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestModule_Name reports the module's registry identity.
func TestModule_Name(t *testing.T) {
	if got := NewModule().Name(); got != "demo" {
		t.Fatalf("Name() = %q, want %q", got, "demo")
	}
}

// TestModule_DependsOn_IsEmpty pins demo's self-containment: it declares
// only copy and two notification types, so it depends on no module (the
// notification module that renders its types is a consumer of the merged
// catalog, never a dependency of the declaration).
func TestModule_DependsOn_IsEmpty(t *testing.T) {
	if deps := NewModule().DependsOn(); len(deps) != 0 {
		t.Fatalf("DependsOn() = %v, want empty", deps)
	}
}

// TestModule_Migrations_IsEmpty pins demo's no-tables shape: its
// Migrations() is an empty FS, which is exactly why cmd/server never
// registers demo on its dbkit.MigrationRegistry (module.go's Migrations
// doc comment).
func TestModule_Migrations_IsEmpty(t *testing.T) {
	m := NewModule()
	entries, err := fs.ReadDir(m.Migrations(), ".")
	if err != nil {
		t.Fatalf("read Migrations(): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Migrations() holds %d entries, want an empty FS -- demo owns no tables", len(entries))
	}
}

// TestModule_OpenAPISpec_IsNil pins demo's no-HTTP-surface shape: the
// demo patient-message route is a hand-written cmd/server route outside
// the OpenAPI machinery, so there is no fragment to return.
func TestModule_OpenAPISpec_IsNil(t *testing.T) {
	if spec := NewModule().OpenAPISpec(); spec != nil {
		t.Fatalf("OpenAPISpec() returned %d bytes, want nil", len(spec))
	}
}

// TestModule_Locales_ContainBothNotificationTypeTemplates guards the
// module's whole reason to exist: every notification type it declares
// must be renderable from BOTH languages' template bundles -- an
// in-app/email/SMS dispatch of either type resolves its copy through the
// merged catalog by the type's key (go/notification renders
// "<type_key>.<channel>.<part>" ids), so a language whose bundle lacks
// the type's keys could never render a message for it.
func TestModule_Locales_ContainBothNotificationTypeTemplates(t *testing.T) {
	m := NewModule()
	localesFS := m.Locales()
	wantKeys := []string{
		TypeKeyPatientReminder + ".email.subject",
		TypeKeyPatientReminder + ".sms.text",
		TypeKeySimulationReady + ".sms.text",
	}
	for _, lang := range []string{"zh-CN.toml", "en-US.toml"} {
		content, err := fs.ReadFile(localesFS, lang)
		if err != nil {
			t.Fatalf("read %s: %v", lang, err)
		}
		for _, key := range wantKeys {
			if !strings.Contains(string(content), `"`+key+`" =`) {
				t.Errorf("%s has no template entry for %q", lang, key)
			}
		}
	}
}

// TestModule_Register_DeclaresBothNotificationTypes exercises the one
// declaration Register performs: both notification types land on the
// registry's notification seat with the exact preference-matrix shape
// their constants declare -- the group, the default channels and the
// unsubscribable flag that decides whether a recipient may silence the
// type (notes' own type comment points at demo.patient_reminder as the
// unsubscribable-false contrast).
func TestModule_Register_DeclaresBothNotificationTypes(t *testing.T) {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := NewModule().Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	types := reg.Notifications.Types()
	if len(types) != 2 {
		t.Fatalf("registered %d notification types, want 2", len(types))
	}
	byKey := map[string]pkgcore.NotificationType{}
	for _, typ := range types {
		byKey[typ.Key] = typ
	}

	reminder, ok := byKey[TypeKeyPatientReminder]
	if !ok {
		t.Fatalf("type %q was not registered (got %v)", TypeKeyPatientReminder, types)
	}
	if reminder.Group != "clinical" {
		t.Errorf("%q Group = %q, want %q", TypeKeyPatientReminder, reminder.Group, "clinical")
	}
	if reminder.Unsubscribable {
		t.Errorf("%q Unsubscribable = true, want false -- a consent-tied patient reminder is not a stream a recipient may silence", TypeKeyPatientReminder)
	}

	ready, ok := byKey[TypeKeySimulationReady]
	if !ok {
		t.Fatalf("type %q was not registered (got %v)", TypeKeySimulationReady, types)
	}
	if len(ready.DefaultChannels) != 1 || ready.DefaultChannels[0] != "sms" {
		t.Errorf("%q DefaultChannels = %v, want [sms]", TypeKeySimulationReady, ready.DefaultChannels)
	}
	if !ready.Unsubscribable {
		t.Errorf("%q Unsubscribable = false, want true -- an ordinary user notification may be silenced", TypeKeySimulationReady)
	}
}

// compile-time check that *Module satisfies pkgcore.Module -- redundant
// with module.go's own assertion, kept here too as a visible part of this
// file's own test surface.
var _ pkgcore.Module = (*Module)(nil)
