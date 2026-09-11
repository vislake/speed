package admin

import (
	"context"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared system purpose.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, adminComponent)
}

// TestComponent_RequiresPinsTheTokenSet pins the component's dependency
// declaration exactly: the database and the queue, the products of the four
// modules the console reads mandatorily, the provider's product the rbac
// declaration stage is ordered behind, and the two optional module
// products. A change here is a change to what a composition must select for
// admin to assemble, so it is asserted token by token rather than by count
// alone.
func TestComponent_RequiresPinsTheTokenSet(t *testing.T) {
	want := []struct {
		token    string
		optional bool
	}{
		{"*gorm.DB", false},
		{"*jobs.Queue", false},
		{"*authn.Module", false},
		{"*org.Module", false},
		{"*compliance.Module", false},
		{"*notification.Module", false},
		{"*rbac.Service", false},
		{"*metering.Module", true},
		{"*billing.Module", true},
	}
	if len(adminComponent.Requires) != len(want) {
		t.Fatalf("Requires has %d entries, want %d", len(adminComponent.Requires), len(want))
	}
	for i, w := range want {
		got := adminComponent.Requires[i]
		if name := reflect.TypeOf(got.Token).String(); name != w.token {
			t.Errorf("Requires[%d].Token = %s, want %s", i, name, w.token)
		}
		if got.Optional != w.optional {
			t.Errorf("Requires[%d] (%s) Optional = %v, want %v", i, w.token, got.Optional, w.optional)
		}
	}
	if len(adminComponent.SystemPurposes) != 1 || adminComponent.SystemPurposes[0] != SystemPurposeAdminCrossTenant {
		t.Errorf("SystemPurposes = %v, want [%s]", adminComponent.SystemPurposes, SystemPurposeAdminCrossTenant)
	}
}

// TestComponent_NewBuildsTheFullFanIn drives the component's New over the
// real module graph buildTestAdminModule stands up, and pins that every
// mandatory module product, the queue and the two optional modules reached
// the built console module.
func TestComponent_NewBuildsTheFullFanIn(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(env.DB)
	reg.Put(env.Queue)
	reg.Put(env.Authn)
	reg.Put(env.Org)
	reg.Put(env.Compliance)
	reg.Put(env.Notification)
	reg.Put(env.Metering)
	reg.Put(env.Billing)

	instance, err := adminComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *admin.Module", instance, instance)
	}
	if m.authnModule != env.Authn || m.orgModule != env.Org || m.complianceModule != env.Compliance || m.notificationModule != env.Notification {
		t.Error("mandatory module products did not reach the built module")
	}
	if m.meteringModule != env.Metering || m.billingModule != env.Billing {
		t.Error("optional module products did not reach the built module")
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
}

// TestComponent_NewWithoutOptionalModules proves the optional module
// products' absence is a legal construction: metering and billing are the
// only dependencies not needed for a boot.
func TestComponent_NewWithoutOptionalModules(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(env.DB)
	reg.Put(env.Queue)
	reg.Put(env.Authn)
	reg.Put(env.Org)
	reg.Put(env.Compliance)
	reg.Put(env.Notification)

	instance, err := adminComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *admin.Module", instance, instance)
	}
	if m.meteringModule != nil || m.billingModule != nil {
		t.Errorf("optional modules = (%v, %v), want both nil without providers", m.meteringModule, m.billingModule)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := adminComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
