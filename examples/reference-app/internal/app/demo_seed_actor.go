// The demo seeds' audit Actor: the fixed system Actor the app's boot-time
// demo writes are audited under, and demoSeedCtx, the constructor every one
// of them runs on.

package app

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// demoSeedActorID is the audit Actor id the app's boot-time demo and
// platform-staff seeds attribute their rbac role writes to
// (pkgcore.ActorTypeSystem): rbac emits an audit row for every role it
// defines or grants, and boot-time seeding has no operator session behind
// it, so the rows name the seed itself.
const demoSeedActorID = "reference-app-demo-seed"

// demoSeedCtx returns a copy of ctx carrying the demo seed's system Actor.
//
// Every audited boot-time demo write runs under it: rbac emits an audit row
// for each role it defines or grants, and the row carries the Actor
// audit.Emit reads from the context it is handed, so a seed running on a
// bare tenant context would see its rows land with a blank attribution.
//
// The attribution shape is the app's boot-time one -- a write that is a
// config-driven declaration re-affirmed identically on every restart under a
// fixed actor -- the same shape the boot-time ai-gateway credential write
// uses ("reference-app-boot" in server.go).
func demoSeedCtx(ctx context.Context) context.Context {
	return pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: demoSeedActorID})
}
