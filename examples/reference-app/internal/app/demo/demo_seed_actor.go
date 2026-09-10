// The demo seeds' audit Actor: the fixed system Actor the app's boot-time
// demo writes are audited under, and demoSeedCtx, the constructor every one
// of them runs on.

package demo

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// DemoSeedActorID is the audit Actor id the app's demo seeds attribute
// their audited writes to (pkgcore.ActorTypeSystem): the rbac role writes
// of the boot-time demo and platform-staff seeds, and the demo credit
// grant (demo_credits.go). No operator session stands behind those
// writes, so the rows name the seed itself. Exported so the flow tests
// can assert the attribution against the same value the seeds use.
const DemoSeedActorID = "reference-app-demo-seed"

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
// uses ("reference-app-boot" in internal/app/server.go).
func demoSeedCtx(ctx context.Context) context.Context {
	return pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: DemoSeedActorID})
}
