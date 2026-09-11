package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the ConfigSchema's empty-config
// decode and the token shapes.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, integrationComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database plus both consumed optional products, and
// pins that the configuration and the optional seams reached the built
// module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))
	reg.Put(&fakeQueue{})
	reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-1", true }))

	instance, err := integrationComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(map[string]any{
		"max_api_key_lifetime": "720h",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *integration.Module", instance, instance)
	}
	if m.maxLifetime != 720*time.Hour {
		t.Errorf("maxLifetime = %v, want the configured 720h", m.maxLifetime)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithWebhookQueue")
	}
	if m.subject == nil {
		t.Error("subject is nil, want the registry's resolver wired through WithSubjectResolver")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal construction: only the database is needed.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))

	instance, err := integrationComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *integration.Module", instance, instance)
	}
	if m.queue != nil || m.subject != nil {
		t.Errorf("optional seams = (%v, %v), want both nil without providers", m.queue, m.subject)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := integrationComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
