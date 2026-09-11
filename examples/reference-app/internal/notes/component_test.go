package notes

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared assets.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, notesComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database and the optional creator resolver, and
// pins that both reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))
	reg.Put(stubSubjectResolver{userID: "component-test-creator"})

	instance, err := notesComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *notes.Module", instance, instance)
	}
	if m.subject == nil {
		t.Error("subject is nil, want the registry's resolver wired through WithSubjectResolver")
	}
}

// TestComponent_NewWithoutTheResolver proves the optional dependency's
// absence is a legal construction: creates then fail closed at call time.
func TestComponent_NewWithoutTheResolver(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))

	instance, err := notesComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *notes.Module", instance, instance)
	}
	if m.subject != nil {
		t.Errorf("subject = %v, want nil without a provider", m.subject)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := notesComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
