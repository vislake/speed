package pkgcore

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/locales"
	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/migrations"
)

// The fixtures below exercise LookupComponent and Override: a fully
// populated base descriptor (every declaration and every lifecycle
// callback), a distinct construct callback, and two extra requirement
// tokens. Every callback is a named function, so the identity assertions
// compare entry addresses rather than behaviour.
func overrideFixturePrepare(context.Context, *ComponentRegistry) error { return nil }

func overrideFixtureNew(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
	return &compTokenA{}, nil
}

func overrideFixtureVerify(context.Context, *ComponentRegistry, any) error { return nil }

func overrideFixtureInit(context.Context, *ComponentRegistry, any) error { return nil }

func overrideFixtureStart(context.Context, *ComponentRegistry, any) error { return nil }

func overrideFixtureStop(context.Context, *ComponentRegistry, any) error { return nil }

func overrideFixtureClose(context.Context, *ComponentRegistry, any) error { return nil }

// overrideConstruct is the construction an Override call installs in place
// of the base descriptor's New.
func overrideConstruct(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
	return &compTokenB{}, nil
}

// overrideFixtureBase returns a descriptor with every field set: the
// override tests assert, field by field, that each one survives Override
// except Name, New and Requires.
func overrideFixtureBase(name string) Component {
	return Component{
		Name:            name,
		Module:          "overridefixture",
		Prepare:         overrideFixturePrepare,
		New:             overrideFixtureNew,
		Verify:          overrideFixtureVerify,
		Init:            overrideFixtureInit,
		Start:           overrideFixtureStart,
		Stop:            overrideFixtureStop,
		Close:           overrideFixtureClose,
		Requires:        []Requirement{{Token: (*compTokenA)(nil)}},
		Provides:        []any{(*compTokenA)(nil), (*compSpreader)(nil)},
		Capabilities:    MultiReplicaSafe | SurvivesRestart,
		ConfigSchema:    (*compSchema)(nil),
		ConfigNamespace: "overridefixture.ns",
		BootstrapKeys:   []BootstrapKey{{Key: "overridefixture.key", Format: "hexkey"}},
		SystemPurposes:  []SystemPurpose{"overrideFixturePurpose"},
		Migrations:      migrations.FS,
		Locales:         locales.FS,
		OpenAPISpec:     []byte(`{"openapi":"3.0.0"}`),
	}
}

// sameCallback reports whether two callback values are the same function,
// or both nil: function values cannot be compared with ==, so the identity
// check compares the entry addresses reflect exposes.
func sameCallback(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// TestOverride_PreservesEveryDeclarationButNameNewAndRequires pins the
// derivation's field-by-field promise: Module, ConfigSchema,
// ConfigNamespace, BootstrapKeys, SystemPurposes, Capabilities, Provides,
// Migrations, Locales, OpenAPISpec and the six lifecycle callbacks are the
// base descriptor's own, while Name, New and Requires carry the caller's.
func TestOverride_PreservesEveryDeclarationButNameNewAndRequires(t *testing.T) {
	base := overrideFixtureBase("override.fields.base")
	reg := newTestRegistry(t, base)

	override, err := Override(reg, base.Name, "override.fields.copy", overrideConstruct,
		Requirement{Token: (*compTokenB)(nil)})
	if err != nil {
		t.Fatalf("Override(%q) = %v, want nil", base.Name, err)
	}

	if override.Name != "override.fields.copy" {
		t.Errorf("Override Name = %q, want %q", override.Name, "override.fields.copy")
	}
	if !sameCallback(override.New, overrideConstruct) {
		t.Error("Override did not install the caller's construct callback as New")
	}
	if sameCallback(override.New, base.New) {
		t.Error("Override left the base descriptor's New callback in place")
	}

	// The declaration surface: every field is the base's own.
	if override.Module != base.Module {
		t.Errorf("Override Module = %q, want %q", override.Module, base.Module)
	}
	if override.ConfigSchema != base.ConfigSchema {
		t.Errorf("Override ConfigSchema = %v, want %v", override.ConfigSchema, base.ConfigSchema)
	}
	if override.ConfigNamespace != base.ConfigNamespace {
		t.Errorf("Override ConfigNamespace = %q, want %q", override.ConfigNamespace, base.ConfigNamespace)
	}
	if override.Capabilities != base.Capabilities {
		t.Errorf("Override Capabilities = %v, want %v", override.Capabilities, base.Capabilities)
	}
	if !reflect.DeepEqual(override.Provides, base.Provides) {
		t.Errorf("Override Provides = %v, want %v", override.Provides, base.Provides)
	}
	if !reflect.DeepEqual(override.BootstrapKeys, base.BootstrapKeys) {
		t.Errorf("Override BootstrapKeys = %v, want %v", override.BootstrapKeys, base.BootstrapKeys)
	}
	if !reflect.DeepEqual(override.SystemPurposes, base.SystemPurposes) {
		t.Errorf("Override SystemPurposes = %v, want %v", override.SystemPurposes, base.SystemPurposes)
	}
	if override.Migrations != base.Migrations {
		t.Error("Override changed the Migrations asset set")
	}
	if override.Locales != base.Locales {
		t.Error("Override changed the Locales asset set")
	}
	if !reflect.DeepEqual(override.OpenAPISpec, base.OpenAPISpec) {
		t.Errorf("Override OpenAPISpec = %s, want %s", override.OpenAPISpec, base.OpenAPISpec)
	}

	callbacks := []struct {
		name      string
		got, want any
	}{
		{"Prepare", override.Prepare, base.Prepare},
		{"Verify", override.Verify, base.Verify},
		{"Init", override.Init, base.Init},
		{"Start", override.Start, base.Start},
		{"Stop", override.Stop, base.Stop},
		{"Close", override.Close, base.Close},
	}
	for _, cb := range callbacks {
		if !sameCallback(cb.got, cb.want) {
			t.Errorf("Override changed the %s callback", cb.name)
		}
	}

	// The derived descriptor is registrable: a host registers its override
	// beside the module's own descriptor, which keeps its own name.
	if err := reg.Register(override); err != nil {
		t.Errorf("Register(Override(...)) = %v, want nil", err)
	}
	if _, ok := LookupComponent(reg, base.Name); !ok {
		t.Errorf("registering the override displaced the base descriptor %q", base.Name)
	}
}

// TestOverride_AppendsExtraRequirementsWithoutMutatingBase pins the
// requirement list's shape in both directions: with extra tokens the
// override's Requires is the base's list followed by extra, without extra
// it is a copy of the base's own -- and in neither case does the registered
// descriptor's list change.
func TestOverride_AppendsExtraRequirementsWithoutMutatingBase(t *testing.T) {
	base := overrideFixtureBase("override.requires.base")
	reg := newTestRegistry(t, base)

	extra := []Requirement{
		{Token: (*compTokenB)(nil)},
		{Token: (*compSpreader)(nil), Optional: true},
	}
	withExtra, err := Override(reg, base.Name, "override.requires.extra", overrideConstruct, extra...)
	if err != nil {
		t.Fatalf("Override(%q) = %v, want nil", base.Name, err)
	}
	want := append(append([]Requirement(nil), base.Requires...), extra...)
	if !reflect.DeepEqual(withExtra.Requires, want) {
		t.Errorf("Override Requires = %v, want %v", withExtra.Requires, want)
	}

	withoutExtra, err := Override(reg, base.Name, "override.requires.plain", overrideConstruct)
	if err != nil {
		t.Fatalf("Override(%q) = %v, want nil", base.Name, err)
	}
	if !reflect.DeepEqual(withoutExtra.Requires, base.Requires) {
		t.Errorf("Override Requires = %v, want the base's own list %v", withoutExtra.Requires, base.Requires)
	}

	// The override's list is a fresh copy: adjusting it never reaches the
	// registered descriptor.
	withoutExtra.Requires[0] = Requirement{Token: (*compTokenB)(nil)}
	registered, ok := LookupComponent(reg, base.Name)
	if !ok {
		t.Fatalf("LookupComponent(%q) not found", base.Name)
	}
	if !reflect.DeepEqual(registered.Requires, base.Requires) {
		t.Errorf("the registered descriptor's Requires changed to %v, want %v", registered.Requires, base.Requires)
	}
}

// TestOverride_UnregisteredBaseNamesIt pins the refusal: a base name no
// registry carries returns an error naming base and no descriptor.
func TestOverride_UnregisteredBaseNamesIt(t *testing.T) {
	reg := NewComponentRegistry()

	override, err := Override(reg, "override.absent.base", "override.absent.copy", overrideConstruct)
	if err == nil {
		t.Fatal("Override with an unregistered base returned no error")
	}
	for _, want := range []string{"pkgcore: component", `"override.absent.base"`, "no registered descriptor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry element %q", err, want)
		}
	}
	if !reflect.DeepEqual(override, Component{}) {
		t.Errorf("Override returned %+v alongside its error, want the zero Component", override)
	}
}

// TestLookupComponent_FindsRegisteredAndReportsAbsence pins the lookup in
// both directions, against the instance's full registration: a global
// descriptor (mailer.console, seeded by this package's own registration)
// and an instance-local one are both found with their declarations intact,
// and an unknown name reports absence rather than a zero-value hit.
func TestLookupComponent_FindsRegisteredAndReportsAbsence(t *testing.T) {
	local := overrideFixtureBase("override.lookup.local")
	reg := newTestRegistry(t, local)

	found, ok := LookupComponent(reg, local.Name)
	if !ok {
		t.Fatalf("LookupComponent(%q) not found", local.Name)
	}
	if found.Module != local.Module || found.Capabilities != local.Capabilities {
		t.Errorf("LookupComponent(%q) = %+v, want the registered descriptor %+v", local.Name, found, local)
	}
	if !sameCallback(found.New, local.New) {
		t.Error("LookupComponent returned a descriptor whose New callback is not the registered one")
	}
	if _, inGlobal := globalComponent(local.Name); inGlobal {
		t.Fatalf("fixture %q leaked into the global registration", local.Name)
	}

	global, ok := LookupComponent(reg, "mailer.console")
	if !ok {
		t.Fatal("LookupComponent(mailer.console) not found on a fresh registry")
	}
	if global.Module != "mailer" {
		t.Errorf("LookupComponent(mailer.console).Module = %q, want %q", global.Module, "mailer")
	}

	absent, ok := LookupComponent(reg, "override.lookup.absent")
	if ok {
		t.Error("LookupComponent found an unregistered name")
	}
	if !reflect.DeepEqual(absent, Component{}) {
		t.Errorf("LookupComponent returned %+v for an absent name, want the zero Component", absent)
	}
}
