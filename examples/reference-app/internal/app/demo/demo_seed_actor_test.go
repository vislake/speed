package demo

// demo_seed_actor_test.go pins demoSeedCtx's contract: the Actor it attaches
// is the fixed system Actor every audited boot-time demo write is recorded
// under, and it layers over -- never replaces -- the tenant the caller's
// context already carries, so a seed write keeps the tenant scoping of its
// rbac rows and the seed's own actor at once.

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestDemoSeedCtxAttachesTheSeedSystemActorOverTheTenant(t *testing.T) {
	const tenant pkgcore.TenantID = "demo-tenant"
	base := pkgcore.WithTenant(context.Background(), tenant)

	seedCtx := demoSeedCtx(base)

	actor, ok := pkgcore.ActorFromContext(seedCtx)
	if !ok {
		t.Fatal("demoSeedCtx() left the context without an Actor; every rbac-audited demo write would land with a blank attribution")
	}
	if actor.Type != pkgcore.ActorTypeSystem {
		t.Errorf("Actor.Type = %q, want %q", actor.Type, pkgcore.ActorTypeSystem)
	}
	if actor.ID != DemoSeedActorID {
		t.Errorf("Actor.ID = %q, want the seed's fixed id %q", actor.ID, DemoSeedActorID)
	}

	gotTenant, ok := pkgcore.TenantFromContext(seedCtx)
	if !ok || gotTenant != tenant {
		t.Errorf("TenantFromContext() = (%q, %v), want (%q, true): the seed actor must layer over the caller's tenant, not replace it", gotTenant, ok, tenant)
	}
}
